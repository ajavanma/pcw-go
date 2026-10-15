package pcw_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	pcw "github.com/ajavanma/pcw-go"
)

// TestPCWIntegration uses a real installed PCW worker when PCW_PYTHON names its
// Python interpreter. CI sets this explicitly; normal Go-only tests skip it.
func TestPCWIntegration(t *testing.T) {
	python := os.Getenv("PCW_PYTHON")
	if python == "" {
		t.Skip("set PCW_PYTHON to an interpreter with protocol-capable PCW and the ml extra")
	}
	client, err := pcw.NewClient(pcw.Config{Executable: python, PrefixArgs: []string{"-m", "pointcloud_workflows"}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	root := filepath.Join(t.TempDir(), "artifacts with spaces ; $literal")
	if err = os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	doctor, err := client.Doctor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if doctor.Data.Version != doctor.PCWVersion {
		t.Fatal("worker version mismatch")
	}
	raw, err := client.Synthetic(ctx, pcw.SyntheticRequest{Output: filepath.Join(root, "raw points"), Seed: 42})
	if err != nil {
		t.Fatal(err)
	}
	inspection, err := client.Inspect(ctx, pcw.InspectRequest{Manifest: raw.Data.Manifest})
	if err != nil {
		t.Fatal(err)
	}
	if inspection.Data.TotalPoints == 0 || len(inspection.Data.Scenes) == 0 {
		t.Fatal("empty synthetic fixture")
	}
	preparedDir := filepath.Join(root, "prepared points")
	prepared, err := client.Preprocess(ctx, pcw.PreprocessRequest{Manifest: raw.Data.Manifest, Output: preparedDir, VoxelSize: 0.2})
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Data.VoxelSize != 0.2 || len(prepared.Data.Scenes) != len(inspection.Data.Scenes) {
		t.Fatal("unexpected preprocessing result")
	}
	preparedPath := filepath.Join(preparedDir, "prepared.json")
	preparedInspection, err := client.Inspect(ctx, pcw.InspectRequest{Prepared: preparedPath})
	if err != nil {
		t.Fatal(err)
	}
	if preparedInspection.Data.TotalInputPoints != inspection.Data.TotalPoints {
		t.Fatal("original rows lost")
	}
	// Protocol opt-in does not alter the on-disk schema-v1 prepared metadata.
	disk, err := os.ReadFile(preparedPath)
	if err != nil {
		t.Fatal(err)
	}
	var actual, expected any
	_ = json.Unmarshal(disk, &actual)
	_ = json.Unmarshal(prepared.Raw, &expected)
	diskCanonical, _ := json.Marshal(actual)
	replyCanonical, _ := json.Marshal(expected)
	if !bytes.Equal(diskCanonical, replyCanonical) {
		t.Fatal("protocol differs from saved prepared metadata")
	}
	model := filepath.Join(root, "model run")
	trained, err := client.Train(ctx, pcw.TrainRequest{Prepared: preparedPath, Output: model, Backend: pcw.BackendTorch, Device: pcw.DeviceCPU, Epochs: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(trained.Data.Epochs) != 1 || trained.Data.Device != pcw.DeviceCPU {
		t.Fatal("unexpected training result")
	}
	run, err := client.InspectRun(ctx, pcw.InspectRunRequest{Output: model})
	if err != nil {
		t.Fatal(err)
	}
	if run.Data.State != "completed" || run.Data.RunID == "" {
		t.Fatal("run not complete")
	}
	before, err := os.ReadFile(filepath.Join(model, "complete.json"))
	if err != nil {
		t.Fatal(err)
	}
	resumed, err := client.Train(ctx, pcw.TrainRequest{Prepared: preparedPath, Output: model, Device: pcw.DeviceCPU, Resume: true})
	if err != nil {
		t.Fatal(err)
	}
	var trainedValue, resumedValue any
	if err = json.Unmarshal(trained.Raw, &trainedValue); err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(resumed.Raw, &resumedValue); err != nil {
		t.Fatal(err)
	}
	trainedCanonical, _ := json.Marshal(trainedValue)
	resumedCanonical, _ := json.Marshal(resumedValue)
	if !bytes.Equal(trainedCanonical, resumedCanonical) {
		t.Fatal("completed resume changed result values")
	}
	after, err := os.ReadFile(filepath.Join(model, "complete.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("completed resume rewrote completion")
	}
	evaluation, err := client.Evaluate(ctx, pcw.InferenceRequest{Prepared: preparedPath, Model: model, Output: filepath.Join(root, "evaluation"), Device: pcw.DeviceCPU})
	if err != nil {
		t.Fatal(err)
	}
	if len(evaluation.Data.Metrics) == 0 {
		t.Fatal("missing evaluation metrics")
	}
	predictions, err := client.Predict(ctx, pcw.InferenceRequest{Prepared: preparedPath, Model: model, Output: filepath.Join(root, "predictions"), Device: pcw.DeviceCPU})
	if err != nil {
		t.Fatal(err)
	}
	if len(predictions.Data.Scenes) == 0 {
		t.Fatal("no predictions")
	}

	// A relative path starting with '-' is a literal output name, not a flag.
	relativeClient, err := pcw.NewClient(pcw.Config{Executable: python, PrefixArgs: []string{"-m", "pointcloud_workflows"}, Dir: root})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = relativeClient.Synthetic(ctx, pcw.SyntheticRequest{Output: "--raw-points", Seed: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(root, "--raw-points", "manifest.json")); err != nil {
		t.Fatal(err)
	}
	// Output ownership failures must remain structured worker errors, not retries.
	_, err = client.Synthetic(ctx, pcw.SyntheticRequest{Output: filepath.Join(root, "raw points")})
	var failure *pcw.Error
	if !errors.As(err, &failure) || failure.Kind != pcw.ErrorExit || failure.ExitCode != 2 {
		t.Fatalf("expected structured duplicate-output failure, got %v", err)
	}

	// Python verifies saved artifact hashes; the Go library does not duplicate it.
	// Corrupting a final weight byte must fail inspection without creating attempts.
	attemptsBefore, err := os.ReadDir(filepath.Join(model, "attempts"))
	if err != nil {
		t.Fatal(err)
	}
	weightsPath := filepath.Join(model, "weights.safetensors")
	weights, err := os.ReadFile(weightsPath)
	if err != nil {
		t.Fatal(err)
	}
	weights[len(weights)-1] ^= 1
	if err = os.WriteFile(weightsPath, weights, 0600); err != nil {
		t.Fatal(err)
	}
	_, err = client.InspectRun(ctx, pcw.InspectRunRequest{Output: model})
	if !errors.As(err, &failure) || failure.Kind != pcw.ErrorExit || failure.WorkerCode != "data_error" {
		t.Fatalf("expected Python artifact validation error, got %v", err)
	}
	attemptsAfter, err := os.ReadDir(filepath.Join(model, "attempts"))
	if err != nil {
		t.Fatal(err)
	}
	if len(attemptsBefore) != len(attemptsAfter) {
		t.Fatal("inspection created attempts")
	}
	completionAfter, err := os.ReadFile(filepath.Join(model, "complete.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, completionAfter) {
		t.Fatal("inspection rewrote completion")
	}
	weightsAfter, err := os.ReadFile(weightsPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(weights, weightsAfter) {
		t.Fatal("inspection rewrote weights")
	}
}
