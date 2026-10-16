package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"
)

const (
	maxRegistryBytes = 1 << 20
	maxManifestBytes = 8 << 20
)

var datasetIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)
var manifestHashPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

type registeredDataset struct {
	ID       string
	Manifest string
	SHA256   string
}

type datasetRegistry map[string]registeredDataset

// loadDatasets reads operator configuration once. Relative manifests are based
// on the resolved registry file, never on a request or the working directory.
func loadDatasets(path string) (datasetRegistry, error) {
	if path == "" {
		return nil, nil
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	registryPath, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return nil, err
	}
	data, err := readRegularFile(registryPath, maxRegistryBytes)
	if err != nil {
		return nil, fmt.Errorf("dataset registry: %w", err)
	}
	fields, err := strictObject(data, "schema_version", "datasets")
	if err != nil {
		return nil, fmt.Errorf("dataset registry: %w", err)
	}
	var version int
	if err = json.Unmarshal(fields["schema_version"], &version); err != nil || version != 1 {
		return nil, errors.New("dataset registry requires schema_version 1")
	}
	var entries []json.RawMessage
	if err = json.Unmarshal(fields["datasets"], &entries); err != nil {
		return nil, errors.New("dataset registry datasets must be an array")
	}
	registry := make(datasetRegistry, len(entries))
	for _, entry := range entries {
		fields, err = strictObject(entry, "id", "manifest", "sha256")
		if err != nil {
			return nil, fmt.Errorf("dataset registry entry: %w", err)
		}
		var dataset registeredDataset
		for key, target := range map[string]*string{"id": &dataset.ID, "manifest": &dataset.Manifest, "sha256": &dataset.SHA256} {
			if err = json.Unmarshal(fields[key], target); err != nil {
				return nil, fmt.Errorf("dataset registry %s must be a string", key)
			}
		}
		if _, exists := registry[dataset.ID]; exists {
			return nil, errors.New("duplicate dataset ID")
		}
		if !filepath.IsAbs(dataset.Manifest) {
			dataset.Manifest = filepath.Join(filepath.Dir(registryPath), dataset.Manifest)
		}
		dataset, err = canonicalDataset(dataset)
		if err != nil {
			return nil, err
		}
		registry[dataset.ID] = dataset
	}
	return registry, nil
}

func strictObject(data []byte, keys ...string) (map[string]json.RawMessage, error) {
	if !utf8.Valid(data) {
		return nil, errors.New("invalid UTF-8")
	}
	allowed := make(map[string]bool, len(keys))
	for _, key := range keys {
		allowed[key] = true
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return nil, errors.New("expected object")
	}
	fields := make(map[string]json.RawMessage, len(keys))
	for decoder.More() {
		token, err = decoder.Token()
		if err != nil {
			return nil, err
		}
		key, ok := token.(string)
		if !ok || !allowed[key] || fields[key] != nil {
			return nil, errors.New("unknown or duplicate field")
		}
		var raw json.RawMessage
		if err = decoder.Decode(&raw); err != nil || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return nil, errors.New("invalid or null field")
		}
		fields[key] = raw
	}
	if _, err = decoder.Token(); err != nil {
		return nil, err
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF || len(fields) != len(keys) {
		return nil, errors.New("expected exactly one object with all required fields")
	}
	return fields, nil
}

func canonicalDataset(dataset registeredDataset) (registeredDataset, error) {
	if !datasetIDPattern.MatchString(dataset.ID) || !manifestHashPattern.MatchString(dataset.SHA256) || dataset.Manifest == "" {
		return dataset, errors.New("dataset requires a safe ID, manifest path and lowercase SHA-256")
	}
	absolute, err := filepath.Abs(dataset.Manifest)
	if err != nil {
		return dataset, err
	}
	dataset.Manifest, err = filepath.EvalSymlinks(absolute)
	if err != nil {
		return dataset, err
	}
	return dataset, dataset.verify("")
}

// verify detects path or manifest drift. It is an integrity check, not a
// transactional snapshot: operators must keep registered sources immutable.
func (dataset registeredDataset) verify(outputRoot string) error {
	resolved, err := filepath.EvalSymlinks(dataset.Manifest)
	if err != nil {
		return err
	}
	if resolved != dataset.Manifest {
		return errors.New("registered manifest path changed")
	}
	if outputRoot != "" {
		output, err := canonicalPath(outputRoot)
		if err != nil {
			return err
		}
		if output != outputRoot || pathsOverlap(filepath.Dir(resolved), output) {
			return errors.New("dataset and output directories must remain separate")
		}
	}
	data, err := readRegularFile(resolved, maxManifestBytes)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(data)
	if hex.EncodeToString(digest[:]) != dataset.SHA256 {
		return errors.New("registered manifest does not match its SHA-256")
	}
	return nil
}

func readRegularFile(path string, limit int64) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > limit {
		return nil, fmt.Errorf("expected a regular file of at most %d bytes", limit)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err = file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("expected a readable regular file")
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("file exceeds %d bytes", limit)
	}
	return data, nil
}

// canonicalPath also handles output directories that have not been created.
// Existing ancestors are resolved first so symlink aliases cannot hide overlap.
func canonicalPath(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err == nil {
		return resolved, nil
	}
	if !os.IsNotExist(err) || filepath.Dir(absolute) == absolute {
		return "", err
	}
	parent, err := canonicalPath(filepath.Dir(absolute))
	if err != nil {
		return "", err
	}
	return filepath.Join(parent, filepath.Base(absolute)), nil
}

func pathsOverlap(first, second string) bool {
	within := func(root, child string) bool {
		relative, err := filepath.Rel(root, child)
		return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
	}
	return within(first, second) || within(second, first)
}
