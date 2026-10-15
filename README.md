# pcw-go

A Go client and local runner for [PointCloud Workflows](https://github.com/ajavanma/PointCloud-Workflows).
Go manages execution; the installed Python worker owns point-cloud processing,
CPU/GPU models, artifact validation and checkpoint compatibility.

This repository is private. Its API is experimental and has no tagged release.
The local runner is tested on Linux and macOS with Go 1.25 and later.

## Start locally

Clone this repository using your authenticated GitHub account. Install PCW in a
separate Python 3.11 or 3.12 environment:

```sh
python3 -m venv .venv
# On Linux, install the CPU PyTorch wheel before the requirements below:
# .venv/bin/python -m pip install 'torch==2.13.0' --index-url https://download.pytorch.org/whl/cpu
.venv/bin/python -m pip install -r tests/pcw-requirements.txt
PCW_PYTHON="$PWD/.venv/bin/python" go run ./examples/roundtrip
```

The example creates synthetic point clouds, inspects and preprocesses them,
trains one epoch on the CPU, inspects and resumes the completed run, evaluates,
and exports predictions. It prints an artifact directory and retains it for
inspection. Set `-output /path/to/new-directory` to choose a destination, or
`-pcw /path/to/pcw` to use the CLI directly. No research data or GPU is required.

The requirements file pins the exact PCW revision used by compatibility CI.
An older PCW installation without protocol version 1 is not supported. The
Python runtime remains required; building the Go program does not bundle it.

## Use the library

Private module consumers need authenticated Git access and
`GOPRIVATE=github.com/ajavanma/pcw-go` in addition to any existing private patterns.
Pin a reviewed commit until a module release exists.

```go
client, err := pcw.NewClient(pcw.Config{
    Executable:    "/opt/pcw/bin/pcw",
    Dir:           "/work",
    MaxConcurrent: 2,
})
if err != nil {
    return err
}
result, err := client.Preprocess(ctx, pcw.PreprocessRequest{
    Manifest:  "raw/manifest.json",
    Output:    "prepared",
    VoxelSize: 0.2,
})
if err != nil {
    return err
}
fmt.Println(len(result.Data.Scenes))
```

Import the module as `pcw "github.com/ajavanma/pcw-go"`. To use an interpreter
directly, set `Executable` to its path and `PrefixArgs` to
`[]string{"-m", "pointcloud_workflows"}`. Commands are argument arrays, never
shell scripts. Relative dataset and output paths resolve in `Config.Dir` (or the
calling process's directory when unset).

Methods cover `Doctor`, `Synthetic`, `Inspect`, `Preprocess`, `Train`, `Evaluate`,
`Predict` and `InspectRun`. Each returns typed `Data`, the complete result as
`Raw`, the worker's `PCWVersion` and bounded `Stderr` diagnostics. Go does not
load NPZ arrays or safetensors files. Python verifies artifacts when a command
consumes or inspects them.

## Execution and recovery

- One worker process per call; one concurrent call per client by default.
  Configure `MaxConcurrent` to bound parallel workers. Waiting callers respect
  context cancellation. There is no background service or durable queue.
- Default output limits are 8 MiB stdout and 1 MiB stderr per call. Exceeding
  either cancels the process and returns `ErrorOutputLimit`. Logs are bounded
  diagnostics, not a progress-streaming API.
- On Linux/macOS cancellation kills the worker's process group. It is immediate,
  not a graceful checkpoint request; Python retains its last committed epoch.
  Descendants that deliberately detach into a new session are outside this
  runner's control. Other platforms use direct-process cancellation and are not
  covered by the local POSIX workflow support claim.
- Commands never retry automatically. `TrainRequest.Resume` explicitly asks PCW
  to resume the same compatible run. A completed resume verifies and returns
  its saved report. Preprocess, evaluation and prediction require fresh outputs.
- A canceled/failed call may already have produced files. Inspect the run and
  choose recovery explicitly. `InspectRun` is a read-only snapshot of completion,
  not an activity monitor or a promise that a checkpoint can resume.

Use `errors.As(err, &failure)` with `var failure *pcw.Error` for launch, worker,
protocol, output-limit, request or cancellation failures. `WorkerCode` and
`ExitCode` distinguish structured Python failures; `errors.Is` recognizes
`context.Canceled` and `context.DeadlineExceeded`.

See the [protocol reference](https://github.com/ajavanma/PointCloud-Workflows/blob/main/docs/protocol.md)
and [design and compatibility policy](docs/design.md).

## Development

```sh
go vet ./...
go test -race ./...
PCW_PYTHON=/absolute/path/to/python go test -race -count=1 ./...
```

The first test command needs only Go; the real-worker test explicitly skips
unless `PCW_PYTHON` is set. CI sets it for Python 3.11 and 3.12, installing the
pinned worker. Frozen real response fixtures also test decoder compatibility
without Python. Go tests cover malformed responses, cancellation, descendant
cleanup, output limits and concurrent callers.

Changes go through PRs and all CI jobs must pass before merge. GitHub currently
requires a Pro upgrade to enforce branch protection on this private repository;
[the intended rule](scripts/branch-protection.json) is checked in for activation
when eligible. Squash titles are short and commit bodies are blank. There is no
package, documentation or container publishing workflow.
