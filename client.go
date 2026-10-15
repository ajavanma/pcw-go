// Package pcw runs PointCloud Workflows through its versioned JSON protocol.
// PCW owns computation, artifact validation and checkpoint compatibility. Client
// runs one process per call and never retries or resumes work implicitly.
package pcw

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const ProtocolVersion = 1

// Config describes a local PCW installation. Executable is a program name or
// path, never a shell command. PrefixArgs supports Python's -m invocation.
// Dir controls the worker directory; relative artifact paths are relative to it.
// Zero limits select defaults. Limits apply per call, concurrency per Client.
type Config struct {
	Executable     string
	PrefixArgs     []string
	Dir            string
	MaxConcurrent  int
	MaxStdoutBytes int
	MaxStderrBytes int
	WaitDelay      time.Duration
}

// Client is safe for concurrent use. Construction does not launch a worker.
type Client struct {
	config Config
	slots  chan struct{}
}

func NewClient(config Config) (*Client, error) {
	if config.MaxConcurrent < 0 || config.MaxStdoutBytes < 0 || config.MaxStderrBytes < 0 || config.WaitDelay < 0 {
		return nil, errors.New("pcw: limits must not be negative")
	}
	if config.Executable == "" {
		config.Executable = "pcw"
	}
	if strings.IndexByte(config.Executable, 0) >= 0 {
		return nil, errors.New("pcw: executable contains NUL")
	}
	config.PrefixArgs = append([]string(nil), config.PrefixArgs...)
	if config.MaxConcurrent == 0 {
		config.MaxConcurrent = 1
	}
	if config.MaxStdoutBytes == 0 {
		config.MaxStdoutBytes = 8 << 20
	}
	if config.MaxStderrBytes == 0 {
		config.MaxStderrBytes = 1 << 20
	}
	if config.WaitDelay == 0 {
		config.WaitDelay = 2 * time.Second
	}
	return &Client{config: config, slots: make(chan struct{}, config.MaxConcurrent)}, nil
}

// Result retains the complete worker result so additive fields are accessible.
// Stderr is bounded diagnostics, not part of the JSON result.
type Result[T any] struct {
	PCWVersion string
	Data       T
	Raw        json.RawMessage
	Stderr     string
}

type ErrorKind string

const (
	ErrorRequest     ErrorKind = "request"
	ErrorLaunch      ErrorKind = "launch"
	ErrorExit        ErrorKind = "exit"
	ErrorCanceled    ErrorKind = "canceled"
	ErrorProtocol    ErrorKind = "protocol"
	ErrorOutputLimit ErrorKind = "output_limit"
)

// Error distinguishes caller errors, launch failures, canceled execution,
// worker failures and malformed protocol replies. ExitCode is -1 when no normal
// worker exit status is available. WorkerCode comes from a valid error envelope.
// Unwrap preserves errors.Is(err, context.Canceled/DeadlineExceeded).
type Error struct {
	Kind       ErrorKind
	Command    string
	ExitCode   int
	WorkerCode string
	Stderr     string
	Message    string
	Cause      error
}

func (e *Error) Error() string { return fmt.Sprintf("pcw %s: %s: %s", e.Command, e.Kind, e.Message) }
func (e *Error) Unwrap() error { return e.Cause }

// limitedBuffer cancels its process once its limit is exceeded. Write still
// reports consuming the input so os/exec can finish draining the closed pipes.
type limitedBuffer struct {
	buf      bytes.Buffer
	limit    int
	exceeded bool
	cancel   context.CancelFunc
	mu       sync.Mutex
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	remaining := b.limit - b.buf.Len()
	if len(p) > remaining {
		b.exceeded = true
		b.buf.Write(p[:remaining])
		b.cancel()
	} else {
		b.buf.Write(p)
	}
	return len(p), nil
}
func (b *limitedBuffer) snapshot() ([]byte, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]byte(nil), b.buf.Bytes()...), b.exceeded
}

type envelope struct {
	ProtocolVersion *int            `json:"protocol_version"`
	PCWVersion      string          `json:"pcw_version"`
	Command         string          `json:"command"`
	OK              *bool           `json:"ok"`
	Result          json.RawMessage `json:"result"`
	Error           json.RawMessage `json:"error"`
}
type workerError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (c *Client) run(ctx context.Context, command string, args []string) (envelope, string, error) {
	fail := func(kind ErrorKind, code int, stderr, message string, cause error) (envelope, string, error) {
		return envelope{}, stderr, &Error{Kind: kind, Command: command, ExitCode: code, Stderr: stderr, Message: message, Cause: cause}
	}
	if err := ctx.Err(); err != nil {
		return fail(ErrorCanceled, -1, "", err.Error(), err)
	}
	select {
	case c.slots <- struct{}{}:
		defer func() { <-c.slots }()
	case <-ctx.Done():
		return fail(ErrorCanceled, -1, "", ctx.Err().Error(), ctx.Err())
	}
	if err := ctx.Err(); err != nil {
		return fail(ErrorCanceled, -1, "", err.Error(), err)
	}
	childCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	argv := append([]string(nil), c.config.PrefixArgs...)
	argv = append(argv, "--protocol-version", "1")
	argv = append(argv, pathArguments(args)...)
	argv = append(argv, "--json")
	cmd := exec.CommandContext(childCtx, c.config.Executable, argv...)
	cmd.Dir = c.config.Dir
	cmd.WaitDelay = c.config.WaitDelay
	configureProcess(cmd)
	stdout := &limitedBuffer{limit: c.config.MaxStdoutBytes, cancel: cancel}
	stderr := &limitedBuffer{limit: c.config.MaxStderrBytes, cancel: cancel}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	err := cmd.Run()
	cleanupProcess(cmd)
	output, outputExceeded := stdout.snapshot()
	diagnostic, diagnosticExceeded := stderr.snapshot()
	stderrText := string(diagnostic)
	code := -1
	if cmd.ProcessState != nil {
		code = cmd.ProcessState.ExitCode()
	}
	if outputExceeded || diagnosticExceeded {
		return fail(ErrorOutputLimit, code, stderrText, "worker output exceeded configured limit", err)
	}
	if ctx.Err() != nil {
		return fail(ErrorCanceled, code, stderrText, ctx.Err().Error(), ctx.Err())
	}
	if cmd.ProcessState == nil {
		return fail(ErrorLaunch, code, stderrText, "could not start worker", err)
	}
	// WaitDelay expiration means inherited pipes were not cleanly closed, even if
	// the direct worker returned a successful envelope.
	if errors.Is(err, exec.ErrWaitDelay) {
		return fail(ErrorProtocol, code, stderrText, "worker pipes remained open after exit", err)
	}
	var reply envelope
	if parseErr := decodeEnvelope(output, &reply); parseErr != nil {
		return fail(ErrorProtocol, code, stderrText, "invalid JSON envelope: "+parseErr.Error(), err)
	}
	if reply.ProtocolVersion == nil || *reply.ProtocolVersion != ProtocolVersion || reply.PCWVersion == "" || reply.Command != command || reply.OK == nil {
		return fail(ErrorProtocol, code, stderrText, "missing or incompatible envelope fields", err)
	}
	if len(reply.Result) == 0 || len(reply.Error) == 0 {
		return fail(ErrorProtocol, code, stderrText, "envelope must contain result and error", err)
	}
	if *reply.OK {
		if err != nil || code != 0 || !isNull(reply.Error) || !isObject(reply.Result) {
			return fail(ErrorProtocol, code, stderrText, "success envelope contradicts exit status or payload", err)
		}
	} else {
		var failure workerError
		if err == nil || code == 0 || !isNull(reply.Result) || !isObject(reply.Error) || decodeWorkerError(reply.Error, &failure) != nil || !validFailureStatus(failure.Code, code) || failure.Message == "" {
			return fail(ErrorProtocol, code, stderrText, "failure envelope contradicts exit status or payload", err)
		}
		return envelope{}, stderrText, &Error{Kind: ErrorExit, Command: command, ExitCode: code, WorkerCode: failure.Code, Stderr: stderrText, Message: failure.Message, Cause: err}
	}
	return reply, stderrText, nil
}

func isNull(value json.RawMessage) bool { return bytes.Equal(bytes.TrimSpace(value), []byte("null")) }
func isObject(value json.RawMessage) bool {
	value = bytes.TrimSpace(value)
	return len(value) > 1 && value[0] == '{'
}
func decodeOne(data []byte, target any) error {
	if !utf8.Valid(data) {
		return errors.New("JSON is not valid UTF-8")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return errors.New("expected exactly one JSON value")
	}
	// encoding/json otherwise silently accepts duplicate envelope/result keys.
	return uniqueKeys(data)
}
func uniqueKeys(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var walk func() error
	walk = func() error {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		delim, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			seen := map[string]bool{}
			for decoder.More() {
				key, err := decoder.Token()
				if err != nil {
					return err
				}
				name := key.(string)
				if seen[name] {
					return fmt.Errorf("duplicate JSON key %q", name)
				}
				seen[name] = true
				if err := walk(); err != nil {
					return err
				}
			}
		case '[':
			for decoder.More() {
				if err := walk(); err != nil {
					return err
				}
			}
		default:
			return errors.New("invalid JSON delimiter")
		}
		_, err = decoder.Token()
		return err
	}
	return walk()
}

func execute[T any](ctx context.Context, c *Client, command string, args []string, validate func(json.RawMessage) error) (Result[T], error) {
	var result Result[T]
	reply, stderr, err := c.run(ctx, command, args)
	if err != nil {
		return result, err
	}
	if err = validate(reply.Result); err == nil {
		err = json.Unmarshal(reply.Result, &result.Data)
	}
	if err != nil {
		return Result[T]{}, &Error{Kind: ErrorProtocol, Command: command, ExitCode: 0, Stderr: stderr, Message: "invalid result: " + err.Error(), Cause: err}
	}
	result.PCWVersion = reply.PCWVersion
	result.Raw = reply.Result
	result.Stderr = stderr
	return result, nil
}

// Attach path values to their flags so argparse treats leading '-' literally.
func pathArguments(args []string) []string {
	result := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--manifest", "--prepared", "--output", "--model":
			if i+1 < len(args) {
				result = append(result, args[i]+"="+args[i+1])
				i++
				continue
			}
		}
		result = append(result, args[i])
	}
	return result
}
func validFailureStatus(code string, exit int) bool {
	switch code {
	case "usage_error", "data_error", "runtime_error", "io_error":
		return exit == 2
	case "internal_error":
		return exit == 1
	default:
		return code != "" && exit != 0
	}
}

// Decode protocol keys literally: encoding/json's case-insensitive struct
// matching must not allow an additive field to override a canonical field.
func decodeEnvelope(data []byte, reply *envelope) error {
	var fields map[string]json.RawMessage
	if err := decodeOne(data, &fields); err != nil {
		return err
	}
	destinations := map[string]any{
		"protocol_version": &reply.ProtocolVersion, "pcw_version": &reply.PCWVersion,
		"command": &reply.Command, "ok": &reply.OK,
	}
	for key, destination := range destinations {
		raw, exists := fields[key]
		if !exists {
			return fmt.Errorf("missing %s", key)
		}
		if err := json.Unmarshal(raw, destination); err != nil {
			return err
		}
	}
	var exists bool
	reply.Result, exists = fields["result"]
	if !exists {
		return errors.New("missing result")
	}
	reply.Error, exists = fields["error"]
	if !exists {
		return errors.New("missing error")
	}
	return nil
}

func decodeWorkerError(raw json.RawMessage, result *workerError) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	for key, destination := range map[string]*string{"code": &result.Code, "message": &result.Message} {
		value, exists := fields[key]
		if !exists {
			return fmt.Errorf("missing error.%s", key)
		}
		if err := json.Unmarshal(value, destination); err != nil {
			return err
		}
	}
	return nil
}
