package main

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"

	pcw "github.com/ajavanma/pcw-go"
)

// pcwWorkflow keeps computation and artifact validation in the installed Python
// worker. Every request runs once; neither retries nor resume are implicit.
func pcwWorkflow(client *pcw.Client) workflowRunner {
	return func(ctx context.Context, root string, input workflowInput) (workflowSummary, error) {
		var result workflowSummary
		var manifest string
		if input.source == nil {
			raw, err := client.Synthetic(ctx, pcw.SyntheticRequest{Output: filepath.Join(root, "raw"), Seed: input.Seed})
			if err != nil {
				return result, err
			}
			manifest = raw.Data.Manifest
		} else {
			manifest = input.source.Manifest
			if err := input.source.verify(filepath.Dir(root)); err != nil {
				return result, err
			}
		}
		raw, err := client.Inspect(ctx, pcw.InspectRequest{Manifest: manifest})
		if err != nil {
			return result, err
		}
		if input.source != nil {
			splits := make(map[pcw.Split]bool)
			for _, scene := range raw.Data.Scenes {
				splits[scene.Split] = true
			}
			if !splits[pcw.SplitTrain] || !splits[pcw.SplitTest] || !splits[pcw.SplitPredict] {
				return result, errors.New("registered dataset requires train, test and predict scenes")
			}
		}
		preparedDirectory := filepath.Join(root, "prepared")
		if _, err = client.Preprocess(ctx, pcw.PreprocessRequest{Manifest: manifest, Output: preparedDirectory, VoxelSize: input.VoxelSize}); err != nil {
			return result, err
		}
		if input.source != nil {
			if err = input.source.verify(filepath.Dir(root)); err != nil {
				return result, err
			}
		}
		preparedPath := filepath.Join(preparedDirectory, "prepared.json")
		prepared, err := client.Inspect(ctx, pcw.InspectRequest{Prepared: preparedPath})
		if err != nil {
			return result, err
		}
		if input.source != nil {
			if err = verifyPreparedSource(prepared.Raw, input.source.SHA256); err != nil {
				return result, err
			}
		}
		modelDirectory := filepath.Join(root, "model")
		seed := int64(input.Seed)
		const maxVoxels = 4096
		if _, err = client.Train(ctx, pcw.TrainRequest{
			Prepared: preparedPath, Output: modelDirectory, Device: pcw.DeviceCPU,
			Backend: pcw.BackendTorch, Epochs: 1, Seed: &seed, MaxVoxels: maxVoxels,
		}); err != nil {
			return result, err
		}
		run, err := client.InspectRun(ctx, pcw.InspectRunRequest{Output: modelDirectory})
		if err != nil {
			return result, err
		}
		if run.Data.State != "completed" {
			return result, errors.New("training run is incomplete")
		}
		if _, err = client.Evaluate(ctx, pcw.InferenceRequest{
			Prepared: preparedPath, Model: modelDirectory, Output: filepath.Join(root, "evaluation"),
			Device: pcw.DeviceCPU, MaxVoxels: maxVoxels,
		}); err != nil {
			return result, err
		}
		predictions, err := client.Predict(ctx, pcw.InferenceRequest{
			Prepared: preparedPath, Model: modelDirectory, Output: filepath.Join(root, "predictions"),
			Device: pcw.DeviceCPU, MaxVoxels: maxVoxels,
		})
		if err != nil {
			return result, err
		}
		return workflowSummary{
			PCWVersion: predictions.PCWVersion, InputPoints: prepared.Data.TotalInputPoints,
			Voxels: prepared.Data.TotalVoxels, RunState: run.Data.State,
			PredictionScenes: len(predictions.Data.Scenes),
		}, nil
	}
}

func verifyPreparedSource(raw json.RawMessage, want string) error {
	var provenance map[string]json.RawMessage
	if err := json.Unmarshal(raw, &provenance); err != nil {
		return errors.New("invalid prepared source provenance")
	}
	var hash string
	if err := json.Unmarshal(provenance["source_manifest_sha256"], &hash); err != nil || hash != want {
		return errors.New("prepared source manifest does not match registered SHA-256")
	}
	return nil
}
