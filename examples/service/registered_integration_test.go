package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	pcw "github.com/ajavanma/pcw-go"
)

func integrationClient(t *testing.T) *pcw.Client {
	t.Helper()
	python := os.Getenv("PCW_PYTHON")
	if python == "" {
		t.Skip("set PCW_PYTHON to an interpreter with protocol-capable PCW and the ml extra")
	}
	client, err := pcw.NewClient(pcw.Config{Executable: python, PrefixArgs: []string{"-m", "pointcloud_workflows"}})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func generateRegisteredFixture(t *testing.T, client *pcw.Client) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	// This is fixture-backed integration, not an external survey benchmark.
	// Generation happens once outside the service's output root; HTTP requests
	// must reuse those existing files without synthesizing replacement inputs.
	result, err := client.Synthetic(ctx, pcw.SyntheticRequest{Output: filepath.Join(privateRoot(t), "registered source"), Seed: 17})
	if err != nil {
		t.Fatal(err)
	}
	return result.Data.Manifest
}

func registeredHandler(t *testing.T, client *pcw.Client, manifest, output string) *service {
	t.Helper()
	registryPath := writeRegistry(t, privateRoot(t), manifest, fileDigest(t, manifest))
	registry, err := loadDatasets(registryPath)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := newService(pcwWorkflow(client), output, 1, 3*time.Minute, nil, registry)
	if err != nil {
		t.Fatal(err)
	}
	return handler
}

func snapshotFiles(t *testing.T, root string) map[string][]byte {
	t.Helper()
	files := map[string][]byte{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		files[relative] = data
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func TestServiceRegisteredPCWIntegration(t *testing.T) {
	client := integrationClient(t)
	manifest := generateRegisteredFixture(t, client)
	digest := fileDigest(t, manifest)
	before := snapshotFiles(t, filepath.Dir(manifest))
	root := privateRoot(t)
	server := httptest.NewServer(registeredHandler(t, client, manifest, root))
	defer server.Close()
	httpClient := server.Client()
	httpClient.Timeout = 3 * time.Minute
	var previousID string
	for range 2 {
		response, err := httpClient.Post(server.URL+"/v1/workflows", "application/json", strings.NewReader(`{"dataset_id":"survey-01","seed":7,"voxel_size":0.2}`))
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil || response.StatusCode != http.StatusOK {
			t.Fatalf("registered HTTP workflow: status %d, body %s (%v)", response.StatusCode, body, err)
		}
		var receipt workflowReceipt
		if err = json.Unmarshal(body, &receipt); err != nil {
			t.Fatal(err)
		}
		if receipt.WorkflowID == "" || receipt.WorkflowID == previousID || receipt.DatasetID != "survey-01" || receipt.Input.DatasetID != "survey-01" || receipt.SourceManifestSHA256 != digest {
			t.Fatalf("incorrect registered receipt: %s", body)
		}
		previousID = receipt.WorkflowID
		if receipt.Input.Seed != 7 || receipt.Input.VoxelSize != 0.2 || receipt.InputPoints != 180 || receipt.Voxels <= 0 || receipt.RunState != "completed" || receipt.PredictionScenes != 1 || receipt.PCWVersion == "" {
			t.Fatalf("unexpected completed workflow: %s", body)
		}
		if bytes.Contains(body, []byte(root)) || bytes.Contains(body, []byte(filepath.Dir(manifest))) {
			t.Fatalf("receipt exposes operator paths: %s", body)
		}
		if _, exists := receipt.Artifacts["raw"]; exists || len(receipt.Artifacts) != 5 {
			t.Fatalf("registered receipt must only advertise created outputs: %#v", receipt.Artifacts)
		}
		for name, reference := range receipt.Artifacts {
			if !filepath.IsLocal(filepath.FromSlash(reference)) || !strings.HasPrefix(reference, receipt.WorkflowID+"/") {
				t.Fatalf("unsafe %s reference %q", name, reference)
			}
			if _, err = os.Stat(filepath.Join(root, filepath.FromSlash(reference))); err != nil {
				t.Fatalf("missing %s output: %v", name, err)
			}
		}
		directory := filepath.Join(root, receipt.WorkflowID)
		for _, absent := range []string{"raw", "generation.json"} {
			if _, err = os.Stat(filepath.Join(directory, absent)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("registered workflow generated %s: %v", absent, err)
			}
		}
		saved, err := os.ReadFile(filepath.Join(directory, "result.json"))
		if err != nil || !bytes.Equal(body, saved) {
			t.Fatalf("saved receipt differs: %s (%v)", saved, err)
		}
		var prepared struct {
			SourceManifestSHA256 string `json:"source_manifest_sha256"`
		}
		readArtifactJSON(t, filepath.Join(directory, "prepared", "prepared.json"), &prepared)
		if prepared.SourceManifestSHA256 != digest || prepared.SourceManifestSHA256 != receipt.SourceManifestSHA256 {
			t.Fatalf("prepared output lost pinned source identity: %+v", prepared)
		}
		assertRegisteredPCWArtifacts(t, client, directory, receipt)
	}
	after := snapshotFiles(t, filepath.Dir(manifest))
	if len(before) != len(after) {
		t.Fatalf("service changed source directory: before %d files, after %d", len(before), len(after))
	}
	for name, original := range before {
		if !bytes.Equal(original, after[name]) {
			t.Errorf("service modified registered input %s", name)
		}
	}
	outputs, err := os.ReadDir(root)
	if err != nil || len(outputs) != 2 {
		t.Fatalf("expected two isolated workflow outputs, got %v (%v)", outputs, err)
	}
}

func assertRegisteredPCWArtifacts(t *testing.T, client *pcw.Client, directory string, receipt workflowReceipt) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	prepared, err := client.Inspect(ctx, pcw.InspectRequest{Prepared: filepath.Join(directory, "prepared", "prepared.json")})
	if err != nil || prepared.Data.TotalInputPoints != receipt.InputPoints || prepared.Data.TotalVoxels != receipt.Voxels || prepared.Data.VoxelSize != receipt.Input.VoxelSize {
		t.Fatalf("prepared inspection disagrees with receipt: %+v (%v)", prepared.Data, err)
	}
	run, err := client.InspectRun(ctx, pcw.InspectRunRequest{Output: filepath.Join(directory, "model")})
	if err != nil || run.Data.State != "completed" {
		t.Fatalf("saved model failed PCW integrity inspection: %+v (%v)", run.Data, err)
	}
	var training pcw.TrainResult
	readArtifactJSON(t, filepath.Join(directory, "model", "training.json"), &training)
	if training.Backend != pcw.BackendTorch || training.Device != pcw.DeviceCPU || len(training.Epochs) != 1 {
		t.Fatalf("expected one CPU training epoch: %+v", training)
	}
	for _, stage := range []string{"evaluation", "predictions"} {
		var report pcw.InferenceResult
		readArtifactJSON(t, filepath.Join(directory, stage, stage+".json"), &report)
		if report.Device != pcw.DeviceCPU || report.Backend != pcw.BackendTorch || len(report.Scenes) != 1 {
			t.Fatalf("incorrect %s output: %+v", stage, report)
		}
		if stage == "evaluation" && len(report.Metrics) == 0 {
			t.Fatal("evaluation produced no metrics")
		}
		for _, scene := range report.Scenes {
			if !filepath.IsLocal(filepath.FromSlash(scene.Path)) || scene.InputPoints == 0 {
				t.Fatalf("invalid %s scene reference: %+v", stage, scene)
			}
			if fileDigest(t, filepath.Join(directory, stage, filepath.FromSlash(scene.Path))) != scene.SHA256 {
				t.Fatalf("%s scene does not match its saved checksum", stage)
			}
		}
	}
}

func TestServiceRegisteredPCWRejectsInvalidSources(t *testing.T) {
	client := integrationClient(t)
	manifest := generateRegisteredFixture(t, client)
	original, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatal(err)
	}
	var dataset map[string]any
	if err = json.Unmarshal(original, &dataset); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"changed manifest", "corrupt scene", "missing train", "missing test", "missing predict"} {
		t.Run(mode, func(t *testing.T) {
			root := privateRoot(t)
			if strings.HasPrefix(mode, "missing ") {
				split := strings.TrimPrefix(mode, "missing ")
				filtered := make([]any, 0)
				for _, scene := range dataset["scenes"].([]any) {
					if scene.(map[string]any)["split"] != split {
						filtered = append(filtered, scene)
					}
				}
				modified := make(map[string]any, len(dataset))
				for key, value := range dataset {
					modified[key] = value
				}
				modified["scenes"] = filtered
				data, err := json.Marshal(modified)
				if err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(manifest, data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			t.Cleanup(func() {
				if err := os.WriteFile(manifest, original, 0600); err != nil {
					t.Fatal(err)
				}
			})
			handler := registeredHandler(t, client, manifest, root)
			switch mode {
			case "changed manifest":
				if err = os.WriteFile(manifest, append(original, ' '), 0600); err != nil {
					t.Fatal(err)
				}
			case "corrupt scene":
				scene := dataset["scenes"].([]any)[0].(map[string]any)["path"].(string)
				path := filepath.Join(filepath.Dir(manifest), filepath.FromSlash(scene))
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(path, []byte("corrupt scene bytes"), 0600); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := os.WriteFile(path, data, 0600); err != nil {
						t.Fatal(err)
					}
				})
			}
			response := serveRequest(handler, workflowRequest(context.Background(), `{"dataset_id":"survey-01"}`, "application/json"))
			assertErrorJSON(t, response, http.StatusInternalServerError)
			if strings.Contains(response.Body.String(), filepath.Dir(manifest)) || strings.Contains(response.Body.String(), root) {
				t.Fatalf("failed workflow disclosed operator paths: %s", response.Body)
			}
			entries, err := os.ReadDir(root)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				for _, name := range []string{"result.json", "model", "evaluation", "predictions", "raw"} {
					if _, err := os.Stat(filepath.Join(root, entry.Name(), name)); !errors.Is(err, os.ErrNotExist) {
						t.Errorf("invalid source created %s (%v)", name, err)
					}
				}
			}
		})
	}
}
