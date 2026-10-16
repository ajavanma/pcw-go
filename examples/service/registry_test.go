package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fileDigest(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func writeRegistry(t *testing.T, directory, manifest, digest string) string {
	t.Helper()
	data, err := json.Marshal(map[string]any{
		"schema_version": 1,
		"datasets":       []map[string]string{{"id": "survey-01", "manifest": manifest, "sha256": digest}},
	})
	if err != nil {
		t.Fatal(err)
	}
	name := filepath.Join(directory, "datasets.json")
	if err = os.WriteFile(name, data, 0600); err != nil {
		t.Fatal(err)
	}
	return name
}

func registryFixture(t *testing.T) (directory, manifest, digest string) {
	t.Helper()
	directory = privateRoot(t)
	source := filepath.Join(directory, "operator inputs ; $literal")
	if err := os.Mkdir(source, 0700); err != nil {
		t.Fatal(err)
	}
	manifest = filepath.Join(source, "manifest.json")
	// Registry loading verifies identity and location; Python owns the dataset
	// contract and scene validation when the workflow starts.
	if err := os.WriteFile(manifest, []byte("{\"fixture\":true}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return directory, manifest, fileDigest(t, manifest)
}

func TestDatasetRegistryResolvesRelativePathsAndChecksIdentity(t *testing.T) {
	directory, manifest, digest := registryFixture(t)
	relative, err := filepath.Rel(directory, manifest)
	if err != nil {
		t.Fatal(err)
	}
	registryPath := writeRegistry(t, directory, relative, digest)
	registry, err := loadDatasets(registryPath)
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := filepath.EvalSymlinks(manifest)
	if err != nil {
		t.Fatal(err)
	}
	want := registeredDataset{ID: "survey-01", Manifest: canonical, SHA256: digest}
	if len(registry) != 1 || registry["survey-01"] != want {
		t.Fatalf("registry = %#v, want %#v", registry, want)
	}
	// The registry's real location is the reference point even when an operator
	// supplies its path through a symlink in another directory.
	alias := filepath.Join(privateRoot(t), "registry-link.json")
	if err = os.Symlink(registryPath, alias); err != nil {
		t.Fatal(err)
	}
	linked, err := loadDatasets(alias)
	if err != nil || linked["survey-01"] != want {
		t.Fatalf("symlinked registry = %#v (%v)", linked, err)
	}
	if registry, err = loadDatasets(""); err != nil || len(registry) != 0 {
		t.Fatalf("omitted registry must preserve synthetic mode: %#v (%v)", registry, err)
	}
	if err = os.WriteFile(manifest, []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = loadDatasets(registryPath); err == nil {
		t.Fatal("registry accepted a manifest different from its pinned SHA-256")
	}
}

func TestDatasetRegistryRejectsInvalidConfiguration(t *testing.T) {
	directory, manifest, digest := registryFixture(t)
	entry := fmt.Sprintf(`{"id":"survey-01","manifest":%q,"sha256":%q}`, manifest, digest)
	wrap := func(value string) string { return `{"schema_version":1,"datasets":` + value + `}` }
	cases := map[string]string{
		"empty":               "",
		"null":                `null`,
		"array root":          `[]`,
		"bad UTF-8":           "{\"schema_version\":1,\"datasets\":[],\"\xff\":0}",
		"missing version":     `{"datasets":[]}`,
		"future version":      `{"schema_version":2,"datasets":[]}`,
		"fractional version":  `{"schema_version":1.0,"datasets":[]}`,
		"boolean version":     `{"schema_version":true,"datasets":[]}`,
		"null version":        `{"schema_version":null,"datasets":[]}`,
		"case variant":        `{"Schema_version":1,"datasets":[]}`,
		"unknown root key":    `{"schema_version":1,"datasets":[],"extra":true}`,
		"duplicate version":   `{"schema_version":1,"schema_version":1,"datasets":[]}`,
		"duplicate datasets":  `{"schema_version":1,"datasets":[],"datasets":[]}`,
		"missing datasets":    `{"schema_version":1}`,
		"null datasets":       wrap(`null`),
		"object datasets":     wrap(`{}`),
		"null entry":          wrap(`[null]`),
		"unknown entry key":   wrap(`[` + strings.TrimSuffix(entry, `}`) + `,"url":"https://example.com/data"}]`),
		"duplicate entry key": wrap(`[` + strings.TrimSuffix(entry, `}`) + `,"id":"survey-01"}]`),
		"case variant entry":  wrap(`[` + strings.Replace(entry, `"id"`, `"ID"`, 1) + `]`),
		"missing ID":          wrap(fmt.Sprintf(`[{"manifest":%q,"sha256":%q}]`, manifest, digest)),
		"missing manifest":    wrap(fmt.Sprintf(`[{"id":"survey-01","sha256":%q}]`, digest)),
		"missing digest":      wrap(fmt.Sprintf(`[{"id":"survey-01","manifest":%q}]`, manifest)),
		"null manifest":       wrap(fmt.Sprintf(`[{"id":"survey-01","manifest":null,"sha256":%q}]`, digest)),
		"empty manifest":      wrap(fmt.Sprintf(`[{"id":"survey-01","manifest":"","sha256":%q}]`, digest)),
		"duplicate IDs":       wrap(`[` + entry + `,` + entry + `]`),
		"trailing object":     wrap(`[]`) + `{}`,
		"oversized registry":  wrap(`[]`) + strings.Repeat(" ", 1<<20),
	}
	for _, id := range []string{"", "../survey", ".hidden", "survey.json", "survey/name", "survey\\name", "https://example.com", "with space", "☁", strings.Repeat("x", 65)} {
		cases["unsafe ID "+id] = wrap(fmt.Sprintf(`[{"id":%q,"manifest":%q,"sha256":%q}]`, id, manifest, digest))
	}
	for _, hash := range []string{"", digest[:63], digest + "0", strings.ToUpper(digest), strings.Repeat("g", 64)} {
		cases["invalid SHA "+hash] = wrap(fmt.Sprintf(`[{"id":"survey-01","manifest":%q,"sha256":%q}]`, manifest, hash))
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			file := filepath.Join(directory, "invalid.json")
			if err := os.WriteFile(file, []byte(data), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := loadDatasets(file); err == nil {
				t.Fatalf("invalid registry accepted: %.200s", data)
			}
		})
	}
}

func TestDatasetRegistryRejectsMissingOrNonregularManifest(t *testing.T) {
	directory, manifest, digest := registryFixture(t)
	for _, source := range []string{filepath.Join(directory, "missing.json"), filepath.Dir(manifest)} {
		registry := writeRegistry(t, directory, source, digest)
		if _, err := loadDatasets(registry); err == nil {
			t.Fatalf("accepted missing or directory manifest %q", source)
		}
	}
}

func TestDatasetRegistryBoundsManifestAndAllowsEmptyRegistry(t *testing.T) {
	directory, manifest, _ := registryFixture(t)
	empty := filepath.Join(directory, "empty.json")
	if err := os.WriteFile(empty, []byte(`{"schema_version":1,"datasets":[]}`), 0600); err != nil {
		t.Fatal(err)
	}
	if registry, err := loadDatasets(empty); err != nil || len(registry) != 0 {
		t.Fatalf("empty registry rejected: %#v (%v)", registry, err)
	}
	if err := os.WriteFile(manifest, bytes.Repeat([]byte(" "), (8<<20)+1), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadDatasets(writeRegistry(t, directory, manifest, fileDigest(t, manifest))); err == nil {
		t.Fatal("registry accepted a manifest exceeding 8 MiB")
	}
}

func TestServiceRejectsOverlappingDatasetAndOutput(t *testing.T) {
	directory, manifest, digest := registryFixture(t)
	registry := datasetRegistry{"survey-01": {ID: "survey-01", Manifest: manifest, SHA256: digest}}
	alias := filepath.Join(directory, "source-alias")
	if err := os.Symlink(filepath.Dir(manifest), alias); err != nil {
		t.Fatal(err)
	}
	for name, output := range map[string]string{
		"same directory":        filepath.Dir(manifest),
		"output contains input": directory,
		"output inside input":   filepath.Join(filepath.Dir(manifest), "results"),
		"symlink overlap":       filepath.Join(alias, "linked-results"),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := newService(func(context.Context, string, workflowInput) (workflowSummary, error) {
				return successfulSummary(), nil
			}, output, 1, time.Second, nil, registry); err == nil {
				t.Fatalf("accepted overlapping output %q", output)
			}
		})
	}
}

func TestServiceOwnsRegisteredInputsAndRedactsSourcePaths(t *testing.T) {
	_, manifest, digest := registryFixture(t)
	root := privateRoot(t)
	registry := datasetRegistry{"survey-01": {ID: "survey-01", Manifest: manifest, SHA256: digest}}
	var got workflowInput
	handler, err := newService(func(_ context.Context, _ string, input workflowInput) (workflowSummary, error) {
		got = input
		return successfulSummary(), nil
	}, root, 1, time.Second, log.New(io.Discard, "", 0), registry)
	if err != nil {
		t.Fatal(err)
	}
	delete(registry, "survey-01")
	registry["injected"] = registeredDataset{ID: "injected", Manifest: "/untrusted", SHA256: digest}
	response := serveRequest(handler, workflowRequest(context.Background(), `{"dataset_id":"survey-01","seed":7,"voxel_size":0.1}`, "application/json"))
	if response.Code != http.StatusOK {
		t.Fatalf("registered workflow failed: %d %s", response.Code, response.Body)
	}
	if got.DatasetID != "survey-01" || got.source == nil || got.source.SHA256 != digest || got.Seed != 7 || got.VoxelSize != 0.1 {
		t.Fatalf("wrong input reached runner: %+v", got)
	}
	var receipt workflowReceipt
	if err := json.Unmarshal(response.Body.Bytes(), &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.DatasetID != "survey-01" || receipt.SourceManifestSHA256 != digest || receipt.Input.DatasetID != "survey-01" {
		t.Fatalf("missing registered source identity: %s", response.Body)
	}
	if _, ok := receipt.Artifacts["raw"]; ok || len(receipt.Artifacts) != 5 {
		t.Fatalf("registered workflow advertises nonexistent raw output: %#v", receipt.Artifacts)
	}
	if bytes.Contains(response.Body.Bytes(), []byte(manifest)) || bytes.Contains(response.Body.Bytes(), []byte(root)) {
		t.Fatalf("receipt leaked operator paths: %s", response.Body)
	}
	response = serveRequest(handler, workflowRequest(context.Background(), `{"dataset_id":"injected"}`, "application/json"))
	assertErrorJSON(t, response, http.StatusNotFound)
}

func TestServiceRejectsInvalidOrUnknownDatasetBeforeWork(t *testing.T) {
	cases := []struct {
		body   string
		status int
	}{
		{`{"dataset_id":"missing"}`, http.StatusNotFound},
		{`{"dataset_id":""}`, http.StatusBadRequest},
		{`{"dataset_id":null}`, http.StatusBadRequest},
		{`{"dataset_id":7}`, http.StatusBadRequest},
		{`{"dataset_id":"../source"}`, http.StatusBadRequest},
		{`{"dataset_id":"https://example.com/data"}`, http.StatusBadRequest},
		{`{"dataset_id":"dataset.npz"}`, http.StatusBadRequest},
		{`{"Dataset_id":"survey-01"}`, http.StatusBadRequest},
		{`{"dataset_id":"a","dataset_id":"b"}`, http.StatusBadRequest},
		{`{"manifest":"/operator/data/manifest.json"}`, http.StatusBadRequest},
		{`{"dataset_url":"https://example.com/data.npz"}`, http.StatusBadRequest},
		{`{"dataset_id":"survey-01","manifest":"/operator/data/manifest.json"}`, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.body, func(t *testing.T) {
			root := privateRoot(t)
			calls := 0
			handler := testService(t, func(context.Context, string, workflowInput) (workflowSummary, error) {
				calls++
				return successfulSummary(), nil
			}, root, 1, time.Second)
			response := serveRequest(handler, workflowRequest(context.Background(), tc.body, "application/json"))
			assertErrorJSON(t, response, tc.status)
			if calls != 0 {
				t.Fatal("invalid or unknown dataset reached the workflow")
			}
			assertNoOutputs(t, root)
		})
	}
}

func TestServiceRejectsRegisteredSourceDrift(t *testing.T) {
	for _, mode := range []string{"manifest bytes", "manifest symlink", "output symlink"} {
		t.Run(mode, func(t *testing.T) {
			_, manifest, digest := registryFixture(t)
			root := privateRoot(t)
			registry := datasetRegistry{"survey-01": {ID: "survey-01", Manifest: manifest, SHA256: digest}}
			called := false
			handler, err := newService(func(context.Context, string, workflowInput) (workflowSummary, error) {
				called = true
				return successfulSummary(), nil
			}, root, 1, time.Second, nil, registry)
			if err != nil {
				t.Fatal(err)
			}
			sourceBefore := snapshotFiles(t, filepath.Dir(manifest))
			switch mode {
			case "manifest bytes":
				err = os.WriteFile(manifest, []byte("changed manifest"), 0600)
			case "manifest symlink":
				// Even identical content at a different target cannot replace the
				// canonical source that the operator registered at startup.
				replacement := filepath.Join(privateRoot(t), "replacement.json")
				if err = os.WriteFile(replacement, sourceBefore["manifest.json"], 0600); err != nil {
					t.Fatal(err)
				}
				if err = os.Remove(manifest); err != nil {
					t.Fatal(err)
				}
				err = os.Symlink(replacement, manifest)
			case "output symlink":
				if err = os.Remove(root); err != nil {
					t.Fatal(err)
				}
				err = os.Symlink(filepath.Dir(manifest), root)
			}
			if err != nil {
				t.Fatal(err)
			}
			response := serveRequest(handler, workflowRequest(context.Background(), `{"dataset_id":"survey-01"}`, "application/json"))
			assertErrorJSON(t, response, http.StatusInternalServerError)
			if called {
				t.Fatal("source or output drift reached the workflow")
			}
			if strings.Contains(response.Body.String(), root) || strings.Contains(response.Body.String(), manifest) {
				t.Fatalf("drift error exposed operator paths: %s", response.Body)
			}
			if mode == "output symlink" {
				after := snapshotFiles(t, filepath.Dir(manifest))
				if len(after) != len(sourceBefore) || !bytes.Equal(after["manifest.json"], sourceBefore["manifest.json"]) {
					t.Fatal("output symlink retarget wrote into the registered source")
				}
			}
		})
	}
}

func TestPreparedSourceMustMatchRegisteredManifest(t *testing.T) {
	const digest = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	for _, raw := range []string{
		`{}`, `null`, `[]`, `{`,
		`{"source_manifest_sha256":null}`,
		`{"source_manifest_sha256":7}`,
		`{"source_manifest_sha256":"wrong"}`,
		`{"source_manifest_sha256":"ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"}`,
	} {
		if err := verifyPreparedSource(json.RawMessage(raw), digest); err == nil {
			t.Errorf("accepted missing, malformed or different source identity: %s", raw)
		}
	}
	if err := verifyPreparedSource(json.RawMessage(`{"source_manifest_sha256":"`+digest+`"}`), digest); err != nil {
		t.Fatalf("matching source identity rejected: %v", err)
	}
}
