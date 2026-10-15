package pcw

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// These responses were emitted by the Python implementation, including actual
// CPU training, saved-run resume and inference. Fixture roots and the platform
// description are normalized; artifact schemas and values remain unchanged.
func TestFrozenProtocolV1(t *testing.T) {
	dir, err := filepath.Abs("testdata")
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "protocol-v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixtures map[string]json.RawMessage
	if err = json.Unmarshal(data, &fixtures); err != nil {
		t.Fatal(err)
	}
	for key, raw := range fixtures {
		t.Run(key, func(t *testing.T) {
			var expected envelope
			if err := decodeOne(raw, &expected); err != nil {
				t.Fatal(err)
			}
			client := fakeClient(t, "fixture", func(c *Config) { c.Dir = dir })
			reply, _, err := client.run(context.Background(), expected.Command, []string{expected.Command, key})
			if !*expected.OK {
				failure := errorKind(t, err, ErrorExit)
				var want workerError
				_ = json.Unmarshal(expected.Error, &want)
				if failure.WorkerCode != want.Code {
					t.Fatalf("worker code %s", failure.WorkerCode)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var target any
			switch key {
			case "doctor":
				target = &DoctorResult{}
			case "synthetic":
				target = &SyntheticResult{}
			case "inspect_raw":
				target = &DatasetResult{}
				err = datasetValidator("rptu-raw-dataset", true)(reply.Result)
			case "inspect_prepared":
				target = &DatasetResult{}
				err = datasetValidator("rptu-prepared-dataset", true)(reply.Result)
			case "preprocess":
				target = &DatasetResult{}
				err = datasetValidator("rptu-prepared-dataset", false)(reply.Result)
			case "train", "resume":
				target = &TrainResult{}
				err = schema(reply.Result, "rptu-training-report")
			case "run_inspect_completed", "run_inspect_incomplete":
				target = &RunResult{}
				err = schema(reply.Result, "pcw-run-inspection")
			case "evaluate", "predict":
				target = &InferenceResult{}
			case "config_validate":
				target = &map[string]json.RawMessage{}
			default:
				t.Fatalf("unrecognized fixture %q", key)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err = json.Unmarshal(reply.Result, target); err != nil {
				t.Fatal(err)
			}
		})
	}
}
