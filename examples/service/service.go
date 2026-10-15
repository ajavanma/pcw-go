package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"time"
	"unicode/utf8"
)

const maxRequestBytes = 4 << 10

type workflowInput struct {
	Seed      uint32  `json:"seed"`
	VoxelSize float64 `json:"voxel_size"`
}

type workflowSummary struct {
	PCWVersion       string `json:"pcw_version"`
	InputPoints      int64  `json:"input_points"`
	Voxels           int64  `json:"voxels"`
	RunState         string `json:"run_state"`
	PredictionScenes int    `json:"prediction_scenes"`
}

type workflowReceipt struct {
	WorkflowID string            `json:"workflow_id"`
	Input      workflowInput     `json:"input"`
	Artifacts  map[string]string `json:"artifacts"`
	workflowSummary
}

type workflowRunner func(context.Context, string, workflowInput) (workflowSummary, error)

type service struct {
	runner  workflowRunner
	root    string
	slots   chan struct{}
	timeout time.Duration
	logger  *log.Logger
}

func newService(runner workflowRunner, root string, maxConcurrent int, timeout time.Duration, logger *log.Logger) (*service, error) {
	if runner == nil || root == "" || maxConcurrent < 1 || timeout <= 0 {
		return nil, errors.New("runner, output directory, positive concurrency and timeout are required")
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(absolute, 0700); err != nil {
		return nil, err
	}
	info, err := os.Stat(absolute)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("output must be a private directory (mode 0700)")
	}
	if logger == nil {
		logger = log.New(io.Discard, "", 0)
	}
	return &service{runner: runner, root: absolute, slots: make(chan struct{}, maxConcurrent), timeout: timeout, logger: logger}, nil
}

func (s *service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	switch r.URL.Path {
	case "/healthz":
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			writeError(w, http.StatusMethodNotAllowed, "method not allowed", "")
			return
		}
		// Liveness is intentionally independent of the worker and its dependencies.
		_, _ = io.WriteString(w, "{\"status\":\"ok\"}\n")
	case "/v1/workflows":
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			writeError(w, http.StatusMethodNotAllowed, "method not allowed", "")
			return
		}
		s.runWorkflow(w, r)
	default:
		writeError(w, http.StatusNotFound, "not found", "")
	}
}

func (s *service) runWorkflow(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), s.timeout)
	defer cancel()
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeError(w, http.StatusUnsupportedMediaType, "Content-Type must be application/json", "")
		return
	}
	// A context deadline alone does not interrupt a slow request-body read.
	// The network server supports this; httptest recorders do not need it.
	deadline, _ := ctx.Deadline()
	readDeadline := time.Now().Add(10 * time.Second)
	if deadline.Before(readDeadline) {
		readDeadline = deadline
	}
	if err = http.NewResponseController(w).SetReadDeadline(readDeadline); err != nil && !errors.Is(err, http.ErrNotSupported) {
		s.workflowError(w, "", err)
		return
	}
	body := http.MaxBytesReader(w, r.Body, maxRequestBytes)
	defer body.Close()
	data, err := io.ReadAll(body)
	if err != nil {
		if !time.Now().Before(deadline) {
			s.workflowError(w, "", context.DeadlineExceeded)
			return
		}
		if ctx.Err() != nil {
			s.workflowError(w, "", ctx.Err())
			return
		}
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "request body exceeds 4096 bytes", "")
		} else {
			writeError(w, http.StatusBadRequest, "could not read request body", "")
		}
		return
	}
	input, err := decodeInput(data)
	if err != nil {
		writeError(w, http.StatusBadRequest, "expected an object with optional seed and voxel_size", "")
		return
	}
	if err = ctx.Err(); err != nil {
		s.workflowError(w, "", err)
		return
	}
	// Admission covers the entire workflow. A busy request never gets an output
	// directory or queues behind the active request's individual subprocesses.
	select {
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	default:
		writeError(w, http.StatusTooManyRequests, "workflow capacity is full", "")
		return
	}
	if err = ctx.Err(); err != nil {
		s.workflowError(w, "", err)
		return
	}
	directory, err := os.MkdirTemp(s.root, "workflow-")
	if err != nil {
		s.workflowError(w, "", err)
		return
	}
	id := filepath.Base(directory)
	s.logger.Printf("workflow %s started", id)
	summary, err := s.runner(ctx, directory, input)
	if err == nil {
		err = ctx.Err()
	}
	if err != nil {
		s.workflowError(w, id, err)
		return
	}
	artifacts := map[string]string{"result": path.Join(id, "result.json")}
	for _, name := range []string{"raw", "prepared", "model", "evaluation", "predictions"} {
		artifacts[name] = path.Join(id, name)
	}
	receipt := workflowReceipt{WorkflowID: id, Input: input, Artifacts: artifacts, workflowSummary: summary}
	response, err := json.Marshal(receipt)
	if err == nil {
		response = append(response, '\n')
		err = saveReceipt(directory, response)
	}
	if err != nil {
		s.workflowError(w, id, err)
		return
	}
	s.logger.Printf("workflow %s completed", id)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(response)
}

// Decode literal keys rather than encoding/json's case-insensitive struct
// matching. Duplicate keys, null, unknown keys and trailing JSON are rejected.
func decodeInput(data []byte) (workflowInput, error) {
	input := workflowInput{Seed: 42, VoxelSize: 0.2}
	if !utf8.Valid(data) {
		return input, errors.New("invalid UTF-8")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return input, errors.New("expected object")
	}
	seen := map[string]bool{}
	for decoder.More() {
		token, err = decoder.Token()
		if err != nil {
			return input, err
		}
		key, ok := token.(string)
		if !ok || seen[key] {
			return input, errors.New("invalid or duplicate key")
		}
		seen[key] = true
		var raw json.RawMessage
		if err = decoder.Decode(&raw); err != nil || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return input, errors.New("expected a number")
		}
		switch key {
		case "seed":
			err = json.Unmarshal(raw, &input.Seed)
		case "voxel_size":
			err = json.Unmarshal(raw, &input.VoxelSize)
		default:
			return input, errors.New("unknown key")
		}
		if err != nil {
			return input, err
		}
	}
	if _, err = decoder.Token(); err != nil {
		return input, err
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		return input, errors.New("expected exactly one JSON object")
	}
	if math.IsNaN(input.VoxelSize) || math.IsInf(input.VoxelSize, 0) || input.VoxelSize < 0.001 || input.VoxelSize > 10 {
		return input, errors.New("voxel_size must be between 0.001 and 10")
	}
	return input, nil
}

func (s *service) workflowError(w http.ResponseWriter, id string, err error) {
	s.logger.Printf("workflow %s failed: %v", id, err)
	status, message := http.StatusInternalServerError, "workflow failed; retained artifacts are available to the operator"
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		status, message = http.StatusGatewayTimeout, "workflow deadline exceeded"
	case errors.Is(err, context.Canceled):
		status, message = http.StatusServiceUnavailable, "workflow canceled"
	}
	writeError(w, status, message, id)
}

func writeError(w http.ResponseWriter, status int, message, id string) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(struct {
		Error      string `json:"error"`
		WorkflowID string `json:"workflow_id,omitempty"`
	}{Error: message, WorkflowID: id})
}

func saveReceipt(directory string, data []byte) error {
	file, err := os.CreateTemp(directory, ".result-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err = file.Write(data); err == nil {
		err = file.Sync()
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(file.Name(), filepath.Join(directory, "result.json")); err != nil {
		return fmt.Errorf("save receipt: %w", err)
	}
	return nil
}
