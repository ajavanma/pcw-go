// roundtrip runs an installed PCW worker through a complete synthetic CPU flow.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"time"

	pcw "github.com/ajavanma/pcw-go"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	executable := flag.String("pcw", "pcw", "PCW executable path")
	python := flag.String("python", os.Getenv("PCW_PYTHON"), "Python interpreter with PCW installed (overrides -pcw)")
	output := flag.String("output", "", "new output directory (default: temporary directory)")
	flag.Parse()
	config := pcw.Config{Executable: *executable}
	if *python != "" {
		config.Executable = *python
		config.PrefixArgs = []string{"-m", "pointcloud_workflows"}
	}
	client, err := pcw.NewClient(config)
	if err != nil {
		return err
	}
	root := *output
	if root == "" {
		root, err = os.MkdirTemp("", "pcw go roundtrip ")
		if err != nil {
			return err
		}
	} else {
		root, err = filepath.Abs(root)
		if err != nil {
			return err
		}
		if err = os.Mkdir(root, 0700); err != nil {
			return err
		}
	}
	// The example retains artifacts for inspection, including after failure.
	fmt.Fprintln(os.Stderr, "Artifacts:", root)
	signalCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ctx, cancel := context.WithTimeout(signalCtx, 5*time.Minute)
	defer cancel()
	doctor, err := client.Doctor(ctx)
	if err != nil {
		return err
	}
	raw, err := client.Synthetic(ctx, pcw.SyntheticRequest{Output: filepath.Join(root, "raw points"), Seed: 42})
	if err != nil {
		return err
	}
	if _, err = client.Inspect(ctx, pcw.InspectRequest{Manifest: raw.Data.Manifest}); err != nil {
		return err
	}
	preparedDir := filepath.Join(root, "prepared points")
	if _, err = client.Preprocess(ctx, pcw.PreprocessRequest{Manifest: raw.Data.Manifest, Output: preparedDir, VoxelSize: 0.2}); err != nil {
		return err
	}
	prepared := filepath.Join(preparedDir, "prepared.json")
	summary, err := client.Inspect(ctx, pcw.InspectRequest{Prepared: prepared})
	if err != nil {
		return err
	}
	model := filepath.Join(root, "model run")
	if _, err = client.Train(ctx, pcw.TrainRequest{Prepared: prepared, Output: model, Device: pcw.DeviceCPU, Backend: pcw.BackendTorch, Epochs: 1}); err != nil {
		return err
	}
	run, err := client.InspectRun(ctx, pcw.InspectRunRequest{Output: model})
	if err != nil {
		return err
	}
	// A completed resume asks Python to verify and return the existing result.
	if _, err = client.Train(ctx, pcw.TrainRequest{Prepared: prepared, Output: model, Device: pcw.DeviceCPU, Resume: true}); err != nil {
		return err
	}
	if _, err = client.Evaluate(ctx, pcw.InferenceRequest{Prepared: prepared, Model: model, Output: filepath.Join(root, "evaluation"), Device: pcw.DeviceCPU}); err != nil {
		return err
	}
	predictions, err := client.Predict(ctx, pcw.InferenceRequest{Prepared: prepared, Model: model, Output: filepath.Join(root, "predictions"), Device: pcw.DeviceCPU})
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]any{"pcw_version": doctor.PCWVersion, "output": root, "input_points": summary.Data.TotalInputPoints, "voxels": summary.Data.TotalVoxels, "run_state": run.Data.State, "prediction_scenes": len(predictions.Data.Scenes)})
}
