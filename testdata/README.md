# Protocol v1 fixtures

`protocol-v1.json` contains fourteen responses produced by PCW's initial
protocol-v1 implementation (`0.2.0.dev0`) on 2026-10-15. The producer ran actual
synthetic generation, raw/prepared inspection, preprocessing, one epoch of
PyTorch 2.13 CPU training, completed resume, evaluation and prediction. It also
captured configuration validation, complete/incomplete run inspection, usage
and data errors.

The temporary artifact root is replaced with `/fixture/pcw` and the platform string
is normalized. These are response-decoding fixtures, not portable checkpoint or
artifact files; their recorded hashes are not hashes of bundled data.
`compatibility_test.go` consumes them without needing Python. The separate real
worker test exercises the PCW revision in `tests/pcw-requirements.txt` in CI.

Keep these initial fixtures when updating the worker pin, so compatibility with
the original protocol producer stays tested. Add fixtures for new behavior
rather than silently replacing the old contract.
