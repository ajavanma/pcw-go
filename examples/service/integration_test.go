package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	pcw "github.com/ajavanma/pcw-go"
)

// CI supplies PCW_PYTHON in both supported Python compatibility jobs. Without
// that explicit worker installation, the ordinary Go suite remains standalone.
func TestServicePCWIntegration(t *testing.T) {
	python := os.Getenv("PCW_PYTHON")
	if python == "" {
		t.Skip("set PCW_PYTHON to an interpreter with protocol-capable PCW and the ml extra")
	}
	client, err := pcw.NewClient(pcw.Config{Executable: python, PrefixArgs: []string{"-m", "pointcloud_workflows"}})
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(privateRoot(t), "service outputs ; $literal")
	if err = os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	handler := testService(t, pcwWorkflow(client), root, 1, 3*time.Minute)
	server := httptest.NewServer(handler)
	defer server.Close()
	httpClient := server.Client()
	httpClient.Timeout = 3 * time.Minute
	response, err := httpClient.Post(server.URL+"/v1/workflows", "application/json", strings.NewReader(`{"seed":7,"voxel_size":0.2}`))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("workflow HTTP status %d: %s (%v)", response.StatusCode, body, err)
	}
	var receipt workflowReceipt
	if err = json.Unmarshal(body, &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.Input != (workflowInput{Seed: 7, VoxelSize: 0.2}) || receipt.PCWVersion == "" || receipt.InputPoints != 180 || receipt.Voxels <= 0 || receipt.RunState != "completed" || receipt.PredictionScenes != 1 {
		t.Fatalf("unexpected workflow result: %s", body)
	}
	if bytes.Contains(body, []byte(root)) {
		t.Fatalf("HTTP receipt exposes operator filesystem: %s", body)
	}
	for name, reference := range receipt.Artifacts {
		if !filepath.IsLocal(filepath.FromSlash(reference)) || !strings.HasPrefix(reference, receipt.WorkflowID+"/") {
			t.Fatalf("artifact %s escapes workflow: %q", name, reference)
		}
		if _, err = os.Stat(filepath.Join(root, filepath.FromSlash(reference))); err != nil {
			t.Fatalf("missing artifact %s: %v", name, err)
		}
	}
	directory := filepath.Join(root, receipt.WorkflowID)
	saved, err := os.ReadFile(filepath.Join(directory, "result.json"))
	if err != nil || !bytes.Equal(saved, body) {
		t.Fatalf("saved receipt differs from response: %s (%v)", saved, err)
	}
	var generation struct {
		Seed uint32 `json:"seed"`
	}
	readArtifactJSON(t, filepath.Join(directory, "raw", "generation.json"), &generation)
	if generation.Seed != receipt.Input.Seed {
		t.Fatal("HTTP seed was not passed to synthetic generation")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	prepared, err := client.Inspect(ctx, pcw.InspectRequest{Prepared: filepath.Join(directory, "prepared", "prepared.json")})
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Data.VoxelSize != receipt.Input.VoxelSize || prepared.Data.TotalInputPoints != receipt.InputPoints || prepared.Data.TotalVoxels != receipt.Voxels {
		t.Fatalf("receipt disagrees with saved prepared data: %+v", prepared.Data)
	}
	run, err := client.InspectRun(ctx, pcw.InspectRunRequest{Output: filepath.Join(directory, "model")})
	if err != nil || run.Data.State != "completed" {
		t.Fatalf("saved model failed PCW integrity inspection: %+v (%v)", run.Data, err)
	}
	var training pcw.TrainResult
	readArtifactJSON(t, filepath.Join(directory, "model", "training.json"), &training)
	if training.Backend != pcw.BackendTorch || training.Device != pcw.DeviceCPU || len(training.Epochs) != 1 {
		t.Fatalf("expected one CPU training epoch, got %+v", training)
	}
	for _, stage := range []string{"evaluation", "predictions"} {
		var report pcw.InferenceResult
		readArtifactJSON(t, filepath.Join(directory, stage, stage+".json"), &report)
		if report.Device != pcw.DeviceCPU || report.Backend != pcw.BackendTorch || len(report.Scenes) != 1 {
			t.Fatalf("unexpected %s report: %+v", stage, report)
		}
		if stage == "evaluation" && len(report.Metrics) == 0 {
			t.Fatal("evaluation did not produce metrics")
		}
		for _, scene := range report.Scenes {
			if !filepath.IsLocal(filepath.FromSlash(scene.Path)) {
				t.Fatalf("invalid saved scene path: %q", scene.Path)
			}
			data, err := os.ReadFile(filepath.Join(directory, stage, filepath.FromSlash(scene.Path)))
			if err != nil {
				t.Fatal(err)
			}
			digest := sha256.Sum256(data)
			if hex.EncodeToString(digest[:]) != scene.SHA256 || scene.InputPoints == 0 {
				t.Fatalf("%s scene does not match saved report: %+v", stage, scene)
			}
		}
	}
}

func readArtifactJSON(t *testing.T, path string, target any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(data, target); err != nil {
		t.Fatalf("decode %s: %v", filepath.Base(path), err)
	}
}
