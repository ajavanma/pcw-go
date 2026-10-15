package pcw

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

type Device string

const (
	DeviceAuto Device = "auto"
	DeviceCPU  Device = "cpu"
	DeviceCUDA Device = "cuda"
)

type Backend string

const (
	BackendAuto        Backend = "auto"
	BackendTorch       Backend = "torch"
	BackendWarpConvNet Backend = "warpconvnet"
)

type Split string

const (
	SplitTrain      Split = "train"
	SplitValidation Split = "val"
	SplitTest       Split = "test"
	SplitPredict    Split = "predict"
)

type DoctorResult struct {
	Version      string                     `json:"version"`
	Python       string                     `json:"python"`
	Platform     string                     `json:"platform"`
	Packages     map[string]*string         `json:"packages"`
	Capabilities map[string]json.RawMessage `json:"capabilities"`
}
type SyntheticRequest struct {
	Output string
	Seed   uint32
}
type SyntheticResult struct {
	Manifest string `json:"manifest"`
	Seed     uint32 `json:"seed"`
}

// InspectRequest requires exactly one source. Paths are interpreted by PCW.
type InspectRequest struct {
	Manifest string
	Prepared string
}
type LabelMapping struct {
	DatasetID int64  `json:"dataset_id"`
	TrainID   int64  `json:"train_id"`
	Name      string `json:"name"`
}
type Scene struct {
	ID            string `json:"id"`
	Split         Split  `json:"split"`
	Path          string `json:"path,omitempty"`
	SHA256        string `json:"sha256"`
	Points        int64  `json:"points,omitempty"`
	InputPoints   int64  `json:"input_points,omitempty"`
	ValidPoints   int64  `json:"valid_points"`
	InvalidPoints int64  `json:"invalid_points,omitempty"`
	VoxelCount    int64  `json:"voxel_count,omitempty"`
}

// DatasetResult covers raw inspection, prepared inspection and preprocessing.
// Only prepared reports include VoxelSize. Totals are inspection-only; the
// complete result, including per-label statistics, is available in Result.Raw.
type DatasetResult struct {
	SchemaVersion      int            `json:"schema_version"`
	Kind               string         `json:"kind"`
	CoordinateUnit     string         `json:"coordinate_unit"`
	FeatureNames       []string       `json:"feature_names"`
	LabelMapping       []LabelMapping `json:"label_mapping"`
	IgnoreLabel        *int64         `json:"ignore_label"`
	VoxelSize          float64        `json:"voxel_size,omitempty"`
	Scenes             []Scene        `json:"scenes"`
	TotalPoints        int64          `json:"total_points,omitempty"`
	TotalInputPoints   int64          `json:"total_input_points,omitempty"`
	TotalValidPoints   int64          `json:"total_valid_points,omitempty"`
	TotalInvalidPoints int64          `json:"total_invalid_points"`
	TotalVoxels        int64          `json:"total_voxels,omitempty"`
}
type PreprocessRequest struct {
	Manifest  string
	Output    string
	VoxelSize float64
}

// TrainRequest's zero Epochs, LearningRate and MaxVoxels use PCW defaults on
// new runs and saved settings on resume. Seed nil likewise leaves it unspecified.
// Resume is always explicit; the runner never retries a command automatically.
type TrainRequest struct {
	Prepared     string
	Output       string
	Device       Device
	Backend      Backend
	Epochs       int
	LearningRate float64
	Seed         *int64
	MaxVoxels    int
	Resume       bool
}
type TrainResult struct {
	SchemaVersion int               `json:"schema_version"`
	Kind          string            `json:"kind"`
	Backend       Backend           `json:"backend"`
	Device        Device            `json:"device"`
	Architecture  string            `json:"architecture"`
	Epochs        []json.RawMessage `json:"epochs"`
	Train         json.RawMessage   `json:"train"`
	Validation    json.RawMessage   `json:"val"`
}

// InferenceRequest is shared by Evaluate and Predict. An empty Split selects
// PCW's command default (test for Evaluate, predict for Predict).
type InferenceRequest struct {
	Prepared  string
	Model     string
	Output    string
	Device    Device
	Split     Split
	MaxVoxels int
}
type InferenceResult struct {
	SchemaVersion int             `json:"schema_version"`
	Kind          string          `json:"kind"`
	Backend       Backend         `json:"backend"`
	Device        Device          `json:"device"`
	Split         Split           `json:"split"`
	Scenes        []Scene         `json:"scenes"`
	Metrics       json.RawMessage `json:"metrics,omitempty"`
}
type InspectRunRequest struct{ Output string }
type RunResult struct {
	SchemaVersion int             `json:"schema_version"`
	Kind          string          `json:"kind"`
	RunID         string          `json:"run_id"`
	Output        string          `json:"output"`
	State         string          `json:"state"`
	Spec          json.RawMessage `json:"spec"`
	Completion    json.RawMessage `json:"completion"`
}

func (c *Client) Doctor(ctx context.Context) (Result[DoctorResult], error) {
	return execute[DoctorResult](ctx, c, "doctor", []string{"doctor"}, func(raw json.RawMessage) error {
		return fields(raw, map[string]string{"version": "string", "python": "string", "platform": "string", "packages": "object", "capabilities": "object"})
	})
}
func (c *Client) Synthetic(ctx context.Context, r SyntheticRequest) (Result[SyntheticResult], error) {
	if err := paths("synthetic", r.Output); err != nil {
		return Result[SyntheticResult]{}, err
	}
	return execute[SyntheticResult](ctx, c, "synthetic", []string{"synthetic", "--output", r.Output, "--seed", strconv.FormatUint(uint64(r.Seed), 10)}, func(raw json.RawMessage) error {
		return fields(raw, map[string]string{"manifest": "string", "seed": "number"})
	})
}
func (c *Client) Inspect(ctx context.Context, r InspectRequest) (Result[DatasetResult], error) {
	if (r.Manifest == "") == (r.Prepared == "") {
		return Result[DatasetResult]{}, requestError("inspect", "exactly one of Manifest or Prepared is required")
	}
	args := []string{"inspect"}
	kind := "rptu-raw-dataset"
	if r.Manifest != "" {
		args = append(args, "--manifest", r.Manifest)
	} else {
		args = append(args, "--prepared", r.Prepared)
		kind = "rptu-prepared-dataset"
	}
	if err := paths("inspect", args[2]); err != nil {
		return Result[DatasetResult]{}, err
	}
	return execute[DatasetResult](ctx, c, "inspect", args, datasetValidator(kind, true))
}
func (c *Client) Preprocess(ctx context.Context, r PreprocessRequest) (Result[DatasetResult], error) {
	if err := paths("preprocess", r.Manifest, r.Output); err != nil {
		return Result[DatasetResult]{}, err
	}
	if !positive(r.VoxelSize) {
		return Result[DatasetResult]{}, requestError("preprocess", "VoxelSize must be positive and finite")
	}
	return execute[DatasetResult](ctx, c, "preprocess", []string{"preprocess", "--manifest", r.Manifest, "--output", r.Output, "--voxel-size", formatFloat(r.VoxelSize)}, datasetValidator("rptu-prepared-dataset", false))
}
func (c *Client) Train(ctx context.Context, r TrainRequest) (Result[TrainResult], error) {
	if err := paths("train", r.Prepared, r.Output); err != nil {
		return Result[TrainResult]{}, err
	}
	if err := deviceCheck("train", r.Device); err != nil {
		return Result[TrainResult]{}, err
	}
	if r.Backend != "" && r.Backend != BackendAuto && r.Backend != BackendTorch && r.Backend != BackendWarpConvNet {
		return Result[TrainResult]{}, requestError("train", "unknown Backend")
	}
	if r.Epochs < 0 || r.MaxVoxels < 0 || (r.LearningRate != 0 && !positive(r.LearningRate)) || r.Seed != nil && *r.Seed < 0 {
		return Result[TrainResult]{}, requestError("train", "invalid training option")
	}
	args := []string{"train", "--prepared", r.Prepared, "--output", r.Output}
	if r.Device != "" {
		args = append(args, "--device", string(r.Device))
	}
	if r.Backend != "" {
		args = append(args, "--backend", string(r.Backend))
	}
	if r.Epochs != 0 {
		args = append(args, "--epochs", strconv.Itoa(r.Epochs))
	}
	if r.LearningRate != 0 {
		args = append(args, "--learning-rate", formatFloat(r.LearningRate))
	}
	if r.Seed != nil {
		args = append(args, "--seed", strconv.FormatInt(*r.Seed, 10))
	}
	if r.MaxVoxels != 0 {
		args = append(args, "--max-voxels", strconv.Itoa(r.MaxVoxels))
	}
	if r.Resume {
		args = append(args, "--resume")
	}
	return execute[TrainResult](ctx, c, "train", args, func(raw json.RawMessage) error {
		if err := schema(raw, "rptu-training-report"); err != nil {
			return err
		}
		return fields(raw, map[string]string{"backend": "string", "device": "string", "architecture": "string", "epochs": "array", "train": "object", "val": "nullableObject"})
	})
}
func (c *Client) Evaluate(ctx context.Context, r InferenceRequest) (Result[InferenceResult], error) {
	return c.inference(ctx, "evaluate", r)
}
func (c *Client) Predict(ctx context.Context, r InferenceRequest) (Result[InferenceResult], error) {
	return c.inference(ctx, "predict", r)
}
func (c *Client) inference(ctx context.Context, command string, r InferenceRequest) (Result[InferenceResult], error) {
	if err := paths(command, r.Prepared, r.Model, r.Output); err != nil {
		return Result[InferenceResult]{}, err
	}
	if err := deviceCheck(command, r.Device); err != nil {
		return Result[InferenceResult]{}, err
	}
	if r.MaxVoxels < 0 {
		return Result[InferenceResult]{}, requestError(command, "MaxVoxels must not be negative")
	}
	if r.Split != "" && r.Split != SplitTrain && r.Split != SplitValidation && r.Split != SplitTest && r.Split != SplitPredict {
		return Result[InferenceResult]{}, requestError(command, "unknown Split")
	}
	args := []string{command, "--prepared", r.Prepared, "--model", r.Model, "--output", r.Output}
	if r.Device != "" {
		args = append(args, "--device", string(r.Device))
	}
	if r.Split != "" {
		args = append(args, "--split", string(r.Split))
	}
	if r.MaxVoxels != 0 {
		args = append(args, "--max-voxels", strconv.Itoa(r.MaxVoxels))
	}
	kind := "rptu-predictions"
	if command == "evaluate" {
		kind = "rptu-evaluation"
	}
	return execute[InferenceResult](ctx, c, command, args, func(raw json.RawMessage) error {
		if err := schema(raw, kind); err != nil {
			return err
		}
		required := map[string]string{"backend": "string", "device": "string", "split": "string", "scenes": "array"}
		if command == "evaluate" {
			required["metrics"] = "object"
		}
		return fields(raw, required)
	})
}
func (c *Client) InspectRun(ctx context.Context, r InspectRunRequest) (Result[RunResult], error) {
	if err := paths("run inspect", r.Output); err != nil {
		return Result[RunResult]{}, err
	}
	return execute[RunResult](ctx, c, "run inspect", []string{"run", "inspect", "--output", r.Output}, func(raw json.RawMessage) error {
		if err := schema(raw, "pcw-run-inspection"); err != nil {
			return err
		}
		if err := fields(raw, map[string]string{"run_id": "string", "output": "string", "state": "string", "spec": "object", "completion": "nullableObject"}); err != nil {
			return err
		}
		var r RunResult
		_ = json.Unmarshal(raw, &r)
		if r.State != "completed" && r.State != "incomplete" {
			return errors.New("unknown run state")
		}
		if (r.State == "completed") == isNull(r.Completion) {
			return errors.New("run state contradicts completion")
		}
		return nil
	})
}
func requestError(command, message string) error {
	return &Error{Kind: ErrorRequest, Command: command, ExitCode: -1, Message: message}
}
func paths(command string, values ...string) error {
	for _, value := range values {
		if value == "" || strings.IndexByte(value, 0) >= 0 {
			return requestError(command, "paths must be nonempty and contain no NUL")
		}
	}
	return nil
}
func deviceCheck(command string, device Device) error {
	if device != "" && device != DeviceAuto && device != DeviceCPU && device != DeviceCUDA {
		return requestError(command, "unknown Device")
	}
	return nil
}
func positive(x float64) bool      { return x > 0 && !math.IsInf(x, 0) && !math.IsNaN(x) }
func formatFloat(x float64) string { return strconv.FormatFloat(x, 'g', -1, 64) }
func schema(raw json.RawMessage, kind string) error {
	if err := fields(raw, map[string]string{"schema_version": "number", "kind": "string"}); err != nil {
		return err
	}
	var document struct {
		SchemaVersion int    `json:"schema_version"`
		Kind          string `json:"kind"`
	}
	if err := json.Unmarshal(raw, &document); err != nil {
		return err
	}
	if document.SchemaVersion != 1 || document.Kind != kind {
		return fmt.Errorf("expected schema_version 1 and kind %q", kind)
	}
	return nil
}
func fields(raw json.RawMessage, required map[string]string) error {
	var document map[string]json.RawMessage
	if err := json.Unmarshal(raw, &document); err != nil {
		return err
	}
	for key, kind := range required {
		value, ok := document[key]
		if !ok {
			return fmt.Errorf("missing %s", key)
		}
		valid := false
		switch kind {
		case "string":
			var s string
			valid = json.Unmarshal(value, &s) == nil && s != ""
		case "object":
			valid = isObject(value)
		case "nullableObject":
			valid = isObject(value) || isNull(value)
		case "array":
			valid = len(value) > 0 && value[0] == '['
		case "number":
			var number json.Number
			valid = !isNull(value) && json.Unmarshal(value, &number) == nil
		}
		if !valid {
			return fmt.Errorf("invalid %s (expected %s)", key, kind)
		}
	}
	return nil
}
func datasetValidator(kind string, inspect bool) func(json.RawMessage) error {
	return func(raw json.RawMessage) error {
		if err := schema(raw, kind); err != nil {
			return err
		}
		required := map[string]string{"coordinate_unit": "string", "feature_names": "array", "label_mapping": "array", "scenes": "array"}
		if kind == "rptu-prepared-dataset" {
			required["voxel_size"] = "number"
		}
		if inspect {
			required["total_invalid_points"] = "number"
			if kind == "rptu-raw-dataset" {
				required["total_points"] = "number"
			} else {
				required["total_input_points"] = "number"
				required["total_valid_points"] = "number"
				required["total_voxels"] = "number"
			}
		}
		if err := fields(raw, required); err != nil {
			return err
		}
		var report DatasetResult
		if err := json.Unmarshal(raw, &report); err != nil {
			return err
		}
		if kind == "rptu-prepared-dataset" && !positive(report.VoxelSize) {
			return errors.New("voxel_size must be positive and finite")
		}
		if report.TotalPoints < 0 || report.TotalInputPoints < 0 || report.TotalValidPoints < 0 || report.TotalInvalidPoints < 0 || report.TotalVoxels < 0 {
			return errors.New("negative point counts")
		}
		return nil
	}
}
