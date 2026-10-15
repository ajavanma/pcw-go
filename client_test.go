package pcw

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

const doctorJSON = `{"version":"0.1.0","python":"3.12.0","platform":"test","packages":{},"capabilities":{},"future_field":true}`

func success(command, result string) string {
	return `{"protocol_version":1,"pcw_version":"0.1.0","command":` + strconv.Quote(command) + `,"ok":true,"result":` + result + `,"error":null}`
}

// TestWorker is a real subprocess fixture, avoiding shell parsing and exercising
// the same process/pipe boundary as Python. It is inert in the parent test run.
func TestWorker(t *testing.T) {
	index := -1
	for i, arg := range os.Args {
		if arg == "--" {
			index = i
			break
		}
	}
	if index < 0 {
		return
	}
	mode := os.Args[index+1]
	args := os.Args[index+2:]
	if len(args) < 4 || args[0] != "--protocol-version" || args[1] != "1" || args[len(args)-1] != "--json" {
		fmt.Fprintln(os.Stderr, "incorrect protocol arguments")
		os.Exit(90)
	}
	command := args[2]
	if command == "run" {
		command = "run inspect"
	}
	reply := success(command, doctorJSON)
	switch mode {
	case "fixture":
		content, err := os.ReadFile("protocol-v1.json")
		if err != nil {
			os.Exit(92)
		}
		var fixtures map[string]json.RawMessage
		if json.Unmarshal(content, &fixtures) != nil {
			os.Exit(93)
		}
		key := args[3]
		data, exists := fixtures[key]
		if !exists {
			os.Exit(94)
		}
		fmt.Print(string(data))
		var fixture struct {
			OK bool `json:"ok"`
		}
		_ = json.Unmarshal(data, &fixture)
		if !fixture.OK {
			os.Exit(2)
		}
		os.Exit(0)
	case "valid":
		fmt.Fprint(os.Stderr, "diagnostic\n")
	case "argv":
		encoded, _ := json.Marshal(args)
		reply = success(command, strings.TrimSuffix(doctorJSON, "}")+`,"argv":`+string(encoded)+`}`)
	case "mismatch":
		reply = success("train", doctorJSON)
	case "version":
		reply = strings.Replace(reply, `"protocol_version":1`, `"protocol_version":2`, 1)
	case "missing-version":
		reply = strings.Replace(reply, `"protocol_version":1,`, "", 1)
	case "missing-error":
		reply = strings.Replace(reply, `,"error":null`, "", 1)
	case "uppercase-ok":
		reply = strings.Replace(reply, `"ok":true`, `"OK":true`, 1)
	case "case-alias":
		reply = strings.Replace(reply, `"ok":true`, `"ok":true,"OK":false`, 1)
	case "missing-ok":
		reply = strings.Replace(reply, `"ok":true,`, "", 1)
	case "null-result":
		reply = success(command, "null")
	case "empty-result":
		reply = success(command, "{}")
	case "array-result":
		reply = success(command, "[]")
	case "bad-result":
		reply = success(command, strings.Replace(doctorJSON, `"packages":{}`, `"packages":null`, 1))
	case "duplicate":
		reply = strings.Replace(reply, `"ok":true`, `"ok":false,"ok":true`, 1)
	case "nested-duplicate":
		reply = success(command, strings.Replace(doctorJSON, `"version":"0.1.0"`, `"version":"0.0.0","version":"0.1.0"`, 1))
	case "multiple":
		reply += "\n" + reply
	case "trailing":
		reply += " garbage"
	case "invalid-utf8":
		reply = strings.Replace(reply, "3.12.0", string([]byte{0xff}), 1)
	case "null-envelope":
		reply = "null"
	case "failure", "failure-zero", "failure-wrong-exit", "failure-unknown-code", "failure-uppercase-code":
		reply = `{"protocol_version":1,"pcw_version":"0.1.0","command":"doctor","ok":false,"result":null,"error":{"code":"data_error","message":"bad input"}}`
	case "plain-exit":
		fmt.Fprint(os.Stderr, "crashed")
		os.Exit(2)
	case "stdout-limit":
		fmt.Print(strings.Repeat("x", 4096))
		time.Sleep(time.Minute)
	case "stderr-limit":
		fmt.Fprint(os.Stderr, strings.Repeat("x", 4096))
		time.Sleep(time.Minute)
	case "sleep":
		time.Sleep(time.Minute)
	case "descendant", "orphan":
		child := exec.Command(os.Args[0], "-test.run=TestWorker", "--", "sleep", "--protocol-version", "1", "doctor", "--json")
		child.Stdout = os.Stdout
		child.Stderr = os.Stderr
		if err := child.Start(); err != nil {
			os.Exit(91)
		}
		_ = os.WriteFile("child.pid", []byte(strconv.Itoa(child.Process.Pid)), 0600)
		if mode == "descendant" {
			time.Sleep(time.Minute)
		}
	}
	if mode == "failure-uppercase-code" {
		reply = strings.Replace(reply, `"code"`, `"CODE"`, 1)
	}
	if mode == "failure-unknown-code" {
		reply = strings.Replace(reply, "data_error", "unknown_error", 1)
	}
	fmt.Print(reply)
	if mode == "failure-wrong-exit" {
		os.Exit(1)
	}
	if mode == "failure-unknown-code" || mode == "failure-uppercase-code" {
		os.Exit(2)
	}
	if mode == "failure" || mode == "success-nonzero" {
		os.Exit(2)
	}
	os.Exit(0)
}
func fakeClient(t *testing.T, mode string, update func(*Config)) *Client {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	config := Config{Executable: executable, PrefixArgs: []string{"-test.run=TestWorker", "--", mode}, WaitDelay: 100 * time.Millisecond}
	if update != nil {
		update(&config)
	}
	client, err := NewClient(config)
	if err != nil {
		t.Fatal(err)
	}
	return client
}
func errorKind(t *testing.T, err error, kind ErrorKind) *Error {
	t.Helper()
	var actual *Error
	if !errors.As(err, &actual) || actual.Kind != kind {
		t.Fatalf("got %v, want %s", err, kind)
	}
	return actual
}
func TestDoctor(t *testing.T) {
	client := fakeClient(t, "valid", nil)
	result, err := client.Doctor(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.PCWVersion != "0.1.0" || result.Data.Python != "3.12.0" || result.Stderr != "diagnostic\n" || !strings.Contains(string(result.Raw), "future_field") {
		t.Fatalf("incomplete result: %+v", result)
	}
}
func TestRejectProtocol(t *testing.T) {
	for _, mode := range []string{"mismatch", "version", "missing-version", "missing-error", "missing-ok", "uppercase-ok", "null-result", "empty-result", "array-result", "bad-result", "duplicate", "nested-duplicate", "multiple", "trailing", "invalid-utf8", "null-envelope", "failure-zero", "failure-wrong-exit", "failure-uppercase-code", "success-nonzero", "plain-exit"} {
		t.Run(mode, func(t *testing.T) {
			_, err := fakeClient(t, mode, nil).Doctor(context.Background())
			errorKind(t, err, ErrorProtocol)
		})
	}
}
func TestWorkerFailure(t *testing.T) {
	_, err := fakeClient(t, "failure", nil).Doctor(context.Background())
	failure := errorKind(t, err, ErrorExit)
	if failure.ExitCode != 2 || failure.WorkerCode != "data_error" || failure.Message != "bad input" {
		t.Fatalf("wrong failure: %+v", failure)
	}
}
func TestLaunchFailure(t *testing.T) {
	client, err := NewClient(Config{Executable: filepath.Join(t.TempDir(), "missing")})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Doctor(context.Background())
	errorKind(t, err, ErrorLaunch)
}
func TestOutputLimits(t *testing.T) {
	for _, mode := range []string{"stdout-limit", "stderr-limit"} {
		t.Run(mode, func(t *testing.T) {
			client := fakeClient(t, mode, func(c *Config) { c.MaxStdoutBytes = 1024; c.MaxStderrBytes = 1024 })
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, err := client.Doctor(ctx)
			failure := errorKind(t, err, ErrorOutputLimit)
			if len(failure.Stderr) > 1024 {
				t.Fatal("unbounded stderr")
			}
		})
	}
}
func TestCancellation(t *testing.T) {
	client := fakeClient(t, "sleep", nil)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := client.Doctor(ctx)
	errorKind(t, err, ErrorCanceled)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline lost: %v", err)
	}
}
func TestCanceledBeforeLaunch(t *testing.T) {
	client := fakeClient(t, "valid", nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := client.Doctor(ctx)
	errorKind(t, err, ErrorCanceled)
	if !errors.Is(err, context.Canceled) {
		t.Fatal("cancel lost")
	}
}
func TestConcurrencyQueue(t *testing.T) {
	client := fakeClient(t, "sleep", nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan error, 1)
	go func() { _, err := client.Doctor(ctx); finished <- err }()
	deadline := time.Now().Add(time.Second)
	for len(client.slots) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("first call did not acquire slot")
		}
		time.Sleep(time.Millisecond)
	}
	secondCtx, secondCancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer secondCancel()
	_, err := client.Doctor(secondCtx)
	failure := errorKind(t, err, ErrorCanceled)
	if failure.ExitCode != -1 {
		t.Fatal("queued call launched")
	}
	cancel()
	errorKind(t, <-finished, ErrorCanceled)
}
func TestArgumentBoundaries(t *testing.T) {
	prefix := []string{"-test.run=TestWorker", "--", "argv"}
	executable, _ := os.Executable()
	client, err := NewClient(Config{Executable: executable, PrefixArgs: prefix, Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	prefix[2] = "sleep" // NewClient must own its config slice.
	reply, _, err := client.run(context.Background(), "doctor", []string{"doctor", "--path", "space ; $(not-a-command)"})
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Args []string `json:"argv"`
	}
	if err = json.Unmarshal(reply.Result, &body); err != nil {
		t.Fatal(err)
	}
	want := []string{"--protocol-version", "1", "doctor", "--path", "space ; $(not-a-command)", "--json"}
	if !reflect.DeepEqual(body.Args, want) {
		t.Fatalf("args = %#v", body.Args)
	}
}
func TestRequestValidation(t *testing.T) {
	client := fakeClient(t, "plain-exit", nil)
	calls := []func() error{
		func() error { _, e := client.Synthetic(context.Background(), SyntheticRequest{}); return e },
		func() error {
			_, e := client.Inspect(context.Background(), InspectRequest{Manifest: "a", Prepared: "b"})
			return e
		},
		func() error {
			_, e := client.Preprocess(context.Background(), PreprocessRequest{Manifest: "a", Output: "b", VoxelSize: -1})
			return e
		},
		func() error {
			_, e := client.Train(context.Background(), TrainRequest{Prepared: "a", Output: "b", Device: "bad"})
			return e
		},
		func() error {
			_, e := client.Predict(context.Background(), InferenceRequest{Prepared: "a", Model: "b", Output: "c", Split: "bad"})
			return e
		},
		func() error { _, e := client.InspectRun(context.Background(), InspectRunRequest{}); return e },
	}
	for _, call := range calls {
		errorKind(t, call(), ErrorRequest)
	}
	for _, config := range []Config{{MaxConcurrent: -1}, {MaxStdoutBytes: -1}, {MaxStderrBytes: -1}, {WaitDelay: -1}} {
		if _, err := NewClient(config); err == nil {
			t.Fatal("negative config accepted")
		}
	}
}
func TestRunResultValidation(t *testing.T) {
	valid := `{"schema_version":1,"kind":"pcw-run-inspection","run_id":"abc","output":"/run","state":"completed","spec":{},"completion":{}}`
	if err := schema(json.RawMessage(valid), "pcw-run-inspection"); err != nil {
		t.Fatal(err)
	}
	if err := schema(json.RawMessage(strings.Replace(valid, `"schema_version":1`, `"schema_version":2`, 1)), "pcw-run-inspection"); err == nil {
		t.Fatal("new artifact schema accepted")
	}
}
func TestPlatformSupport(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Log("only direct process cancellation is available on this platform")
	}
}

func TestPathArgumentBoundaries(t *testing.T) {
	args := []string{"predict", "--prepared", "--input", "--output", "-result", "--model", "model space"}
	want := []string{"predict", "--prepared=--input", "--output=-result", "--model=model space"}
	if got := pathArguments(args); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q", got)
	}
}

func TestEnvelopeIgnoresAdditiveCaseAlias(t *testing.T) {
	_, err := fakeClient(t, "case-alias", nil).Doctor(context.Background())
	if err != nil {
		t.Fatal(err)
	}
}

func TestUnknownWorkerCodeRemainsFailure(t *testing.T) {
	_, err := fakeClient(t, "failure-unknown-code", nil).Doctor(context.Background())
	failure := errorKind(t, err, ErrorExit)
	if failure.WorkerCode != "unknown_error" || failure.ExitCode != 2 {
		t.Fatalf("future error not preserved: %+v", failure)
	}
}
