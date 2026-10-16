# HTTP workflow service

This runnable Go service uses `pcw-go` to process synthetic or registered point clouds
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
The optional `dataset_id` selects a registered dataset as described below.
Requests cannot supply file paths, URLs, executables, devices or other training parameters.

The workflow generates synthetic data, inspects it, preprocesses and inspects
the voxels, trains the Torch backend on CPU for one epoch, verifies the completed
training run, evaluates the test split and predicts the prediction split.
The model uses the requested seed and a fixed 4,096-voxel limit. No GPU is needed.

`GET /healthz` is liveness only. It does not start Python or check model/runtime
readiness. The first workflow request will report an incorrectly installed worker.

## Registered datasets

Register existing raw datasets to run this workflow on your own point clouds.
They must follow the [PCW raw dataset contract](https://github.com/ajavanma/PointCloud-Workflows/blob/main/docs/datasets.md):
a JSON manifest plus checksum-identified NPZ or PLY scene files. Include `train`,
`test` and `predict` scenes; `val` scenes are optional. Train/test/validation need
labels. Prediction scenes may be unlabeled. Split whole source scenes before
converting them. This service does not convert arbitrary LiDAR file formats or
choose a split automatically.

For PLY scenes, also install PCW's optional PLY reader in the worker environment:

```sh
.venv/bin/python -m pip install 'plyfile>=1.1,<2'
```

The operator supplies a registry file, separate from dataset manifests:

```json
{
  "schema_version": 1,
  "datasets": [
    {
      "id": "site-a",
      "manifest": "datasets/site-a/manifest.json",
      "sha256": "REPLACE_WITH_SHA256_OF_MANIFEST_BYTES"
    }
  ]
}
```

IDs match `[A-Za-z0-9][A-Za-z0-9_-]{0,63}` and are unique. Manifest paths may be
absolute or relative to the resolved registry file's directory (after symlink
resolution). Relative paths inside a
dataset manifest continue to resolve against that manifest, as PCW specifies.
Registry JSON is strict, versioned and limited to 1 MiB; each regular manifest
file is limited to 8 MiB. Replace the checksum placeholder with 64 lowercase
hexadecimal characters. Unknown fields, duplicate keys/IDs and missing or changed
manifests prevent startup.

For a small reproducible registration demo, create a fixture once, before
starting the service. For your own data, substitute its manifest path and ID:

```sh
.venv/bin/pcw synthetic --output work/datasets/demo --seed 42
.venv/bin/python - <<'PY'
import hashlib
import json
from pathlib import Path

manifest = Path("work/datasets/demo/manifest.json").resolve()
registry = {"schema_version": 1, "datasets": [{
    "id": "demo",
    "manifest": str(manifest),
    "sha256": hashlib.sha256(manifest.read_bytes()).hexdigest(),
}]}
Path("work/datasets.json").write_text(json.dumps(registry, indent=2) + "\n")
PY
PCW_PYTHON="$PWD/.venv/bin/python" ./work/pcw-service \
  -datasets work/datasets.json -output work/service
```

Submit the registered ID:

```sh
curl --fail-with-body --max-time 330 \
  -H 'Content-Type: application/json' \
  -d '{"dataset_id":"demo","seed":42,"voxel_size":0.2}' \
  http://127.0.0.1:8080/v1/workflows
```

Here `seed` controls training; existing input data is reused unchanged.
Omitting `dataset_id` preserves the synthetic workflow. Empty or malformed IDs
return `400`; unknown IDs return `404` before creating outputs or starting work.
There is no fallback to generated data for a failed registered input.

The registry is loaded once and copied into the service. Restart to change it.
Input dataset directories and the output root must be disjoint, including after
symlink resolution. Keep registered files immutable and operator-controlled for
the process lifetime; the service reads them without copying or modifying them.
It verifies the manifest pin at startup and around preprocessing, and checks
the prepared dataset's source hash before training. PCW checks scene hashes and
arrays. These checks detect drift, but are not a transactional snapshot or a
memory/disk quota for large datasets. The model's voxel limit applies after
preprocessing; choose inputs that fit the machine and workflow deadline.

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

Registered workflows omit `raw/` and its artifact reference. Their response and
saved receipt add `dataset_id` and `source_manifest_sha256`; `input.dataset_id`
also records the selection. Source paths never appear in HTTP results. The
remaining artifacts belong to the new workflow directory, independently of the
reused input dataset.

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
| `-datasets` | unset | Operator dataset registry; unset allows synthetic inputs only |
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
| `404` | Unknown dataset ID or route |
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
the integration tests send HTTP requests through the real client and installed
Python worker, then check the generated artifacts. Registered-input coverage
reuses an independently created fixture, verifies source bytes remain unchanged,
and exercises corrupt input failures. Existing compatibility CI runs these tests
on Python 3.11 and 3.12; they explicitly skip without the variable. Fixtures
establish workflow correctness, not real-dataset model quality or scale.

`main.go` configures the process and HTTP lifetime, `service.go` owns the request
boundary and receipts, the registry loader checks operator configuration, and
`workflow.go` sequences typed `pcw-go` calls. Keep
application policy here rather than adding HTTP concerns to the client library.
