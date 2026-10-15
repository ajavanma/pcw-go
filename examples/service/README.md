# HTTP workflow service

This runnable Go service uses `pcw-go` to process a small synthetic point cloud
through preprocessing, CPU training, evaluation and prediction. The Python
worker performs the computation and validates its artifacts. Go owns HTTP
requests, execution limits, cancellation and each workflow's output directory.

## Start locally

From the repository root, install the pinned Python worker using the
[root README](../../README.md#start-locally). Then build and start the service:

```sh
mkdir -p work
go build -o work/pcw-service ./examples/service
PCW_PYTHON="$PWD/.venv/bin/python" ./work/pcw-service \
  -output "$PWD/work/service"
```

The default listener is `127.0.0.1:8080`. In another terminal:

```sh
curl --fail-with-body http://127.0.0.1:8080/healthz
curl --fail-with-body --max-time 330 \
  -H 'Content-Type: application/json' \
  -d '{"seed":42,"voxel_size":0.2}' \
  http://127.0.0.1:8080/v1/workflows
```

The POST stays open while the workflow runs and returns `200` after success.
`{}` selects seed `42` and voxel size `0.2`. The seed must be an integer from
`0` through `4294967295`; voxel size must be between `0.001` and `10`, inclusive.
The request must be a single JSON object, at most 4 KiB, with no unknown fields.
It cannot select files, URLs, executables, devices or training parameters.

The workflow generates synthetic data, inspects it, preprocesses and inspects
the voxels, trains the Torch backend on CPU for one epoch, verifies the completed
training run, evaluates the test split and predicts the prediction split.
The model uses the requested seed and a fixed 4,096-voxel limit. No GPU is needed.

`GET /healthz` is liveness only. It does not start Python or check model/runtime
readiness. The first workflow request will report an incorrectly installed worker.

## Results and ownership

A successful response includes `workflow_id`, the normalized `input`,
`pcw_version`, `input_points`, `voxels`, `run_state`, `prediction_scenes`, and
`artifacts`. Each artifact reference is relative to the configured output root:

```text
work/service/
  workflow-<generated-id>/
    raw/
    prepared/
    model/
    evaluation/
    predictions/
    result.json
```

The service writes the same JSON response into `result.json` with an atomic
rename before reporting success. The receipt contains summary metadata; point
arrays, reports and model weights remain in their PCW directories. The receipt
does not replace PCW's checksummed completion records or checkpoint validation.
The service does not expose a file-download or status endpoint.

An existing output root must be a private directory without group/world
permissions. A missing root and each workflow directory are created with mode
`0700`. Keep this root on operator-controlled local storage. Successful and
partial outputs are retained; monitor disk usage and remove artifacts deliberately.
There is no automatic retention or recovery policy.

## Execution limits and errors

| Flag | Default | Purpose |
| --- | --- | --- |
| `-listen` | `127.0.0.1:8080` | HTTP listener |
| `-output` | `work/service` | Artifact root |
| `-python` | `PCW_PYTHON` | Interpreter with PCW installed; overrides `-pcw` |
| `-pcw` | `pcw` | Installed CLI executable |
| `-max-concurrent` | `1` | Maximum complete workflows executing together |
| `-timeout` | `5m` | Deadline for an accepted workflow |

Admission covers the whole workflow, including receipt persistence. A saturated
service immediately returns `429` without creating an output directory or queuing
work. Request deadlines, client disconnects and SIGINT/SIGTERM propagate into
worker contexts. The process waits up to ten seconds for shutdown cleanup.

Errors use JSON with an `error` message and, once an output directory exists, a
`workflow_id`. Responses do not include worker stderr or private filesystem paths.
Operator logs identify the workflow and underlying failure.

| Status | Meaning |
| --- | --- |
| `400` | Invalid JSON or workflow options |
| `413` | Request body exceeds 4 KiB |
| `415` | Content type is not `application/json` |
| `429` | All workflow slots are occupied |
| `503` | Request or service was canceled |
| `504` | Workflow deadline expired |
| `500` | Worker or local storage failed |

Cancellation can leave partial artifacts. It is immediate process cancellation,
not a graceful checkpoint request. A lost response can also follow a completed
workflow: inspect the retained directory before deciding what to do next.
Every new POST creates a new workflow. There is no automatic retry, idempotency
key, resume endpoint or durable background queue.

The example is intended for local use. Its HTTP API is experimental and has no
authentication or TLS. Define authentication, caller ownership, quotas and
retention before exposing an adapted service to other users. It does not install
a daemon or deploy infrastructure.

## Tests and structure

```sh
go test -race ./examples/service
PCW_PYTHON="$PWD/.venv/bin/python" go test -race -count=1 ./examples/service
```

Go-only tests inject a workflow runner to check HTTP validation, admission,
isolated outputs, receipts, failure handling and cancellation. With `PCW_PYTHON`,
the integration test sends an HTTP request through the real client and installed
Python worker, then checks the generated artifacts. Existing compatibility CI
runs this test on Python 3.11 and 3.12; it explicitly skips without the variable.

`main.go` configures the process and HTTP lifetime, `service.go` owns the request
boundary and receipts, and `workflow.go` sequences typed `pcw-go` calls. Keep
application policy here rather than adding HTTP concerns to the client library.
