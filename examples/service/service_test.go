package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func privateRoot(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "artifacts")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	return root
}

func successfulSummary() workflowSummary {
	return workflowSummary{PCWVersion: "test-worker", InputPoints: 180, Voxels: 152, RunState: "completed", PredictionScenes: 1}
}

func testService(t *testing.T, runner workflowRunner, root string, limit int, timeout time.Duration) *service {
	t.Helper()
	s, err := newService(runner, root, limit, timeout, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func workflowRequest(ctx context.Context, body, contentType string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/v1/workflows", strings.NewReader(body)).WithContext(ctx)
	if contentType != "" {
		r.Header.Set("Content-Type", contentType)
	}
	return r
}

func serveRequest(s http.Handler, r *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

func assertNoOutputs(t *testing.T, root string) {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("rejected request created outputs: %v", entries)
	}
}

func assertErrorJSON(t *testing.T, response *httptest.ResponseRecorder, status int) {
	t.Helper()
	if response.Code != status {
		t.Fatalf("status = %d, want %d; body = %s", response.Code, status, response.Body)
	}
	if got := response.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Fatalf("error Content-Type = %q", got)
	}
	var body struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || body.Error == "" {
		t.Fatalf("missing JSON error: %s (%v)", response.Body, err)
	}
}

func TestServiceRejectsInvalidRequestsBeforeWork(t *testing.T) {
	cases := []struct {
		name, body, contentType string
		status                  int
	}{
		{"missing content type", `{}`, "", http.StatusUnsupportedMediaType},
		{"text content type", `{}`, "text/plain", http.StatusUnsupportedMediaType},
		{"malformed content type", `{}`, `application/json; charset="`, http.StatusUnsupportedMediaType},
		{"empty body", ``, "application/json", http.StatusBadRequest},
		{"null object", `null`, "application/json", http.StatusBadRequest},
		{"array", `[]`, "application/json", http.StatusBadRequest},
		{"truncated", `{"seed":`, "application/json", http.StatusBadRequest},
		{"case variant", `{"Seed":42}`, "application/json", http.StatusBadRequest},
		{"invalid UTF-8", "{\"seed\":\"\xff\"}", "application/json", http.StatusBadRequest},
		{"unknown option", `{"output":"/private/escape"}`, "application/json", http.StatusBadRequest},
		{"executable option", `{"executable":"sh"}`, "application/json", http.StatusBadRequest},
		{"null seed", `{"seed":null}`, "application/json", http.StatusBadRequest},
		{"negative seed", `{"seed":-1}`, "application/json", http.StatusBadRequest},
		{"fractional seed", `{"seed":1.5}`, "application/json", http.StatusBadRequest},
		{"oversized seed", `{"seed":4294967296}`, "application/json", http.StatusBadRequest},
		{"string seed", `{"seed":"42"}`, "application/json", http.StatusBadRequest},
		{"boolean seed", `{"seed":true}`, "application/json", http.StatusBadRequest},
		{"duplicate seed", `{"seed":1,"seed":2}`, "application/json", http.StatusBadRequest},
		{"null voxel size", `{"voxel_size":null}`, "application/json", http.StatusBadRequest},
		{"zero voxel size", `{"voxel_size":0}`, "application/json", http.StatusBadRequest},
		{"small voxel size", `{"voxel_size":0.0009}`, "application/json", http.StatusBadRequest},
		{"large voxel size", `{"voxel_size":10.1}`, "application/json", http.StatusBadRequest},
		{"overflow voxel size", `{"voxel_size":1e400}`, "application/json", http.StatusBadRequest},
		{"duplicate voxel size", `{"voxel_size":0.1,"voxel_size":0.2}`, "application/json", http.StatusBadRequest},
		{"two objects", `{} {}`, "application/json", http.StatusBadRequest},
		{"trailing garbage", `{} garbage`, "application/json", http.StatusBadRequest},
		{"large body", "{}" + strings.Repeat(" ", 4095), "application/json", http.StatusRequestEntityTooLarge},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := privateRoot(t)
			calls := 0
			s := testService(t, func(context.Context, string, workflowInput) (workflowSummary, error) {
				calls++
				return successfulSummary(), nil
			}, root, 1, time.Second)
			response := serveRequest(s, workflowRequest(context.Background(), tc.body, tc.contentType))
			assertErrorJSON(t, response, tc.status)
			if calls != 0 {
				t.Fatal("invalid request reached worker")
			}
			assertNoOutputs(t, root)
		})
	}
}

func TestServiceDefaultsAndInputBounds(t *testing.T) {
	cases := []struct {
		name, body string
		want       workflowInput
	}{
		{"defaults", `{}`, workflowInput{Seed: 42, VoxelSize: 0.2}},
		{"zero seed and minimum voxel size", `{"seed":0,"voxel_size":0.001}`, workflowInput{Seed: 0, VoxelSize: 0.001}},
		{"maximum values", `{"seed":4294967295,"voxel_size":10}`, workflowInput{Seed: 4294967295, VoxelSize: 10}},
		{"exact body limit", "{}" + strings.Repeat(" ", 4094), workflowInput{Seed: 42, VoxelSize: 0.2}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got workflowInput
			s := testService(t, func(_ context.Context, _ string, input workflowInput) (workflowSummary, error) {
				got = input
				return successfulSummary(), nil
			}, privateRoot(t), 1, time.Second)
			response := serveRequest(s, workflowRequest(context.Background(), tc.body, "application/json; charset=utf-8"))
			if response.Code != http.StatusOK || got != tc.want {
				t.Fatalf("status %d input %+v, want %+v; body %s", response.Code, got, tc.want, response.Body)
			}
			var receipt workflowReceipt
			if err := json.Unmarshal(response.Body.Bytes(), &receipt); err != nil || receipt.Input != tc.want {
				t.Fatalf("receipt omitted effective input: %s (%v)", response.Body, err)
			}
		})
	}
}

func TestServiceRoutesDoNotStartWork(t *testing.T) {
	root := privateRoot(t)
	calls := 0
	s := testService(t, func(context.Context, string, workflowInput) (workflowSummary, error) {
		calls++
		return successfulSummary(), nil
	}, root, 1, time.Second)
	for _, tc := range []struct {
		method, path string
		status       int
	}{
		{http.MethodGet, "/healthz", http.StatusOK},
		{http.MethodPost, "/healthz", http.StatusMethodNotAllowed},
		{http.MethodGet, "/v1/workflows", http.StatusMethodNotAllowed},
		{http.MethodPost, "/v1/workflows/extra", http.StatusNotFound},
		{http.MethodGet, "/unknown", http.StatusNotFound},
	} {
		response := serveRequest(s, httptest.NewRequest(tc.method, tc.path, nil))
		if response.Code != tc.status {
			t.Errorf("%s %s status %d, want %d", tc.method, tc.path, response.Code, tc.status)
		}
	}
	if calls != 0 {
		t.Fatal("routing or liveness request reached worker")
	}
	assertNoOutputs(t, root)
}

func TestServiceIsolatesOutputsAndPersistsReceipt(t *testing.T) {
	root := privateRoot(t)
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	var directories []string
	s := testService(t, func(_ context.Context, directory string, _ workflowInput) (workflowSummary, error) {
		directories = append(directories, directory)
		return successfulSummary(), nil
	}, root, 1, time.Second)
	var previousID string
	for range 2 {
		response := serveRequest(s, workflowRequest(context.Background(), `{}`, "application/json"))
		if response.Code != http.StatusOK {
			t.Fatalf("request failed: %d %s", response.Code, response.Body)
		}
		var receipt workflowReceipt
		if err := json.Unmarshal(response.Body.Bytes(), &receipt); err != nil {
			t.Fatal(err)
		}
		if receipt.WorkflowID == "" || receipt.WorkflowID == previousID || filepath.Base(receipt.WorkflowID) != receipt.WorkflowID {
			t.Fatalf("workflow ID is not a unique path component: %q", receipt.WorkflowID)
		}
		previousID = receipt.WorkflowID
		directory := directories[len(directories)-1]
		if directory != filepath.Join(resolvedRoot, receipt.WorkflowID) {
			t.Fatalf("worker directory %q does not match receipt %q", directory, receipt.WorkflowID)
		}
		info, err := os.Stat(directory)
		if err != nil || info.Mode().Perm()&0077 != 0 {
			t.Fatalf("workflow directory permissions: %v (%v)", info, err)
		}
		if len(receipt.Artifacts) != 6 {
			t.Fatalf("expected six artifact references, got %#v", receipt.Artifacts)
		}
		for key, reference := range receipt.Artifacts {
			if filepath.IsAbs(reference) || strings.Contains(reference, "\\") || !strings.HasPrefix(reference, receipt.WorkflowID+"/") || strings.Contains(reference, "..") {
				t.Errorf("artifact %s is not a safe relative reference: %q", key, reference)
			}
		}
		saved, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(receipt.Artifacts["result"])))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(saved, response.Body.Bytes()) {
			t.Fatalf("saved receipt differs from HTTP response: %s / %s", saved, response.Body)
		}
		if bytes.Contains(saved, []byte(root)) || receipt.workflowSummary != successfulSummary() {
			t.Fatalf("receipt exposed root or changed summary: %s", saved)
		}
		entries, err := os.ReadDir(directory)
		if err != nil || len(entries) != 1 || entries[0].Name() != "result.json" {
			t.Fatalf("unexpected files after receipt publication: %v (%v)", entries, err)
		}
	}
}

func TestServiceRejectsSaturationWithoutQueue(t *testing.T) {
	root := privateRoot(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	var calls atomic.Int32
	s := testService(t, func(ctx context.Context, _ string, _ workflowInput) (workflowSummary, error) {
		if calls.Add(1) == 1 {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
				return workflowSummary{}, ctx.Err()
			}
		}
		return successfulSummary(), nil
	}, root, 1, 10*time.Second)
	first := make(chan *httptest.ResponseRecorder, 1)
	go func() { first <- serveRequest(s, workflowRequest(context.Background(), `{}`, "application/json")) }()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("first workflow did not start")
	}
	second := make(chan *httptest.ResponseRecorder, 1)
	go func() { second <- serveRequest(s, workflowRequest(context.Background(), `{}`, "application/json")) }()
	select {
	case response := <-second:
		assertErrorJSON(t, response, http.StatusTooManyRequests)
	case <-time.After(2 * time.Second):
		t.Fatal("saturated request queued instead of returning")
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 1 || calls.Load() != 1 {
		t.Fatalf("saturated request created work: %v calls=%d (%v)", entries, calls.Load(), err)
	}
	// Health remains responsive while the entire workflow occupies its slot.
	if response := serveRequest(s, httptest.NewRequest(http.MethodGet, "/healthz", nil)); response.Code != http.StatusOK {
		t.Fatalf("liveness blocked by workflow: %d", response.Code)
	}
	releaseOnce.Do(func() { close(release) })
	select {
	case response := <-first:
		if response.Code != http.StatusOK {
			t.Fatalf("first workflow failed: %s", response.Body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first workflow did not finish")
	}
	if response := serveRequest(s, workflowRequest(context.Background(), `{}`, "application/json")); response.Code != http.StatusOK || calls.Load() != 2 {
		t.Fatalf("completed workflow did not release slot: %d calls=%d", response.Code, calls.Load())
	}
}

func TestServiceDeadlineCancelsWorker(t *testing.T) {
	root := privateRoot(t)
	var directory string
	s := testService(t, func(ctx context.Context, output string, _ workflowInput) (workflowSummary, error) {
		directory = output
		if _, ok := ctx.Deadline(); !ok {
			return workflowSummary{}, errors.New("no workflow deadline")
		}
		<-ctx.Done()
		// Even a runner returning a late success cannot publish a receipt.
		return successfulSummary(), nil
	}, root, 1, 10*time.Millisecond)
	response := serveRequest(s, workflowRequest(context.Background(), `{}`, "application/json"))
	assertErrorJSON(t, response, http.StatusGatewayTimeout)
	if _, err := os.Stat(directory); err != nil {
		t.Fatalf("partial directory was removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(directory, "result.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("timed out workflow published receipt: %v", err)
	}
}

func TestServiceCancellationReleasesSlotAndRetainsPartialWork(t *testing.T) {
	root := privateRoot(t)
	entered := make(chan string, 1)
	var calls atomic.Int32
	s := testService(t, func(ctx context.Context, directory string, _ workflowInput) (workflowSummary, error) {
		if calls.Add(1) == 1 {
			if err := os.WriteFile(filepath.Join(directory, "partial.txt"), []byte("partial"), 0600); err != nil {
				return workflowSummary{}, err
			}
			entered <- directory
			<-ctx.Done()
			return workflowSummary{}, ctx.Err()
		}
		return successfulSummary(), nil
	}, root, 1, 10*time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan *httptest.ResponseRecorder, 1)
	go func() { finished <- serveRequest(s, workflowRequest(ctx, `{}`, "application/json")) }()
	var directory string
	select {
	case directory = <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not start")
	}
	cancel()
	select {
	case response := <-finished:
		assertErrorJSON(t, response, http.StatusServiceUnavailable)
	case <-time.After(2 * time.Second):
		t.Fatal("request cancellation did not reach worker")
	}
	if _, err := os.Stat(filepath.Join(directory, "partial.txt")); err != nil {
		t.Fatalf("partial output was removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(directory, "result.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cancelled workflow published a receipt: %v", err)
	}
	if response := serveRequest(s, workflowRequest(context.Background(), `{}`, "application/json")); response.Code != http.StatusOK || calls.Load() != 2 {
		t.Fatalf("cancellation did not release slot: %d calls=%d", response.Code, calls.Load())
	}
}

func TestServiceAlreadyCancelledRequestCreatesNoOutput(t *testing.T) {
	root := privateRoot(t)
	called := false
	s := testService(t, func(context.Context, string, workflowInput) (workflowSummary, error) {
		called = true
		return successfulSummary(), nil
	}, root, 1, time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	response := serveRequest(s, workflowRequest(ctx, `{}`, "application/json"))
	assertErrorJSON(t, response, http.StatusServiceUnavailable)
	if called {
		t.Fatal("already cancelled request started worker")
	}
	assertNoOutputs(t, root)
}

func TestServiceRedactsFailuresAndReleasesSlot(t *testing.T) {
	root := privateRoot(t)
	const sensitive = "WORKER_STDERR_SECRET"
	calls := 0
	var failedDirectory string
	s := testService(t, func(_ context.Context, directory string, _ workflowInput) (workflowSummary, error) {
		calls++
		if calls == 1 {
			failedDirectory = directory
			return workflowSummary{}, errors.New(sensitive + ": " + directory)
		}
		return successfulSummary(), nil
	}, root, 1, time.Second)
	response := serveRequest(s, workflowRequest(context.Background(), `{}`, "application/json"))
	assertErrorJSON(t, response, http.StatusInternalServerError)
	if strings.Contains(response.Body.String(), sensitive) || strings.Contains(response.Body.String(), root) {
		t.Fatalf("worker error leaked to client: %s", response.Body)
	}
	if _, err := os.Stat(failedDirectory); err != nil {
		t.Fatalf("failed workflow directory removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(failedDirectory, "result.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed workflow published receipt: %v", err)
	}
	if response := serveRequest(s, workflowRequest(context.Background(), `{}`, "application/json")); response.Code != http.StatusOK || calls != 2 {
		t.Fatalf("worker error did not release slot: %d calls=%d", response.Code, calls)
	}
}

func TestServiceReceiptFailureDoesNotReportSuccess(t *testing.T) {
	root := privateRoot(t)
	s := testService(t, func(_ context.Context, directory string, _ workflowInput) (workflowSummary, error) {
		return successfulSummary(), os.Remove(directory)
	}, root, 1, time.Second)
	response := serveRequest(s, workflowRequest(context.Background(), `{}`, "application/json"))
	assertErrorJSON(t, response, http.StatusInternalServerError)
	if strings.Contains(response.Body.String(), root) {
		t.Fatalf("receipt failure leaked storage path: %s", response.Body)
	}
}

func TestNewServiceRejectsInvalidConfiguration(t *testing.T) {
	runner := func(context.Context, string, workflowInput) (workflowSummary, error) { return successfulSummary(), nil }
	root := privateRoot(t)
	file := filepath.Join(root, "file")
	if err := os.WriteFile(file, nil, 0600); err != nil {
		t.Fatal(err)
	}
	sharedDirectory := filepath.Join(root, "shared")
	if err := os.Mkdir(sharedDirectory, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sharedDirectory, 0750); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		runner  workflowRunner
		root    string
		limit   int
		timeout time.Duration
	}{
		{"nil runner", nil, root, 1, time.Second},
		{"empty root", runner, "", 1, time.Second},
		{"root is file", runner, file, 1, time.Second},
		{"shared output directory", runner, sharedDirectory, 1, time.Second},
		{"zero limit", runner, root, 0, time.Second},
		{"negative limit", runner, root, -1, time.Second},
		{"zero timeout", runner, root, 1, 0},
		{"negative timeout", runner, root, 1, -time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := newService(tc.runner, tc.root, tc.limit, tc.timeout, log.New(io.Discard, "", 0)); err == nil {
				t.Fatal("invalid configuration accepted")
			}
		})
	}
}
