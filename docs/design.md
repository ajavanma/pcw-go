# Design and compatibility

`application → Go client → local PCW process → Python computation → files`

The client is an embeddable execution boundary, not a scheduler or numerical
port. Its runtime dependencies are the Go standard library and a separately
installed PCW worker. Constructing a client starts no process. Multiple clients
have independent concurrency limits; they do not coordinate output ownership.

## Protocol

Each invocation selects `--protocol-version 1`. The client checks the envelope
version, canonical command, required result fields, report schema/kind, and
success/failure agreement with the process exit. It rejects missing, duplicate,
truncated or multiple JSON responses. Additional fields remain accessible in
`Result.Raw`. Python's artifact validators remain authoritative; Go performs no
independent hash traversal or checkpoint deserialization.

Toolkit versions are reported, not compared lexicographically. Protocol version
1 and each report's schema/kind determine decoding compatibility. PCW's own
strict runtime comparisons still determine whether training can resume.

Dataset paths, scene metadata and run identities cross the protocol. Large
arrays and weights stay in files. The runner does not rewrite artifact schemas,
class IDs, point mappings or legacy `rptu-*` kinds.

## Process lifetime

A context covers queue wait and execution. `os/exec` starts the executable
directly with arguments. Each Linux/macOS worker receives a separate process
group, canceled with SIGKILL. After worker exit, remaining group members are
cleaned up. A default two-second `WaitDelay` bounds waiting for inherited pipes;
its expiration is a transport failure even if a success envelope was written.

Both output buffers have explicit limits and exceeding a limit terminates the
call. Outputs may exist after failure, interruption or protocol rejection. The
library therefore has no implicit retry, deletion or output repair policy.

Training resume is an explicit call using the original immutable run settings.
Completed-run reuse is checked by Python; a GPU checkpoint is not silently
converted into a CPU run. Local POSIX locks do not provide distributed ownership.

## Verification

- Go-only tests use real subprocesses to exercise execution boundaries and
  invalid responses, including concurrent calls and descendant cancellation.
- Frozen Python-generated envelopes cover every protocol command and representative
  failures. They establish decoding compatibility with the initial producer.
- Real-worker CI installs a pinned PCW revision and runs the complete synthetic
  CPU pipeline on Python 3.11 and 3.12. It checks metadata correspondence,
  completed resume and output ownership failures.
- PCW separately tests legacy CLI payloads, artifact readers and historical
  training compatibility. The Go module does not replace those tests.

## Service consumer

The [HTTP service example](../examples/service/README.md) consumes the client
without changing its API. One synchronous request owns a fresh artifact
directory and a complete CPU workflow. It uses either generated synthetic data
or an operator-registered raw dataset selected by ID. A service-level admission
limit covers the whole workflow; the client's process limit bounds individual
commands. The request context carries cancellation and a deadline into each
worker process.

The registry is loaded at startup and maps IDs to canonical local manifest paths
and SHA-256 pins. Source directories are separate from output storage. The service
rechecks manifest identity around preprocessing and compares the prepared source
hash before training. PCW remains responsible for scene checksums, formats,
labels and point mappings. Registered data must have train, test and predict
splits for this complete workflow; validation scenes are optional.

The operator keeps input files immutable while workers use them. Hash checks
detect drift, but do not provide a transactional filesystem snapshot. Inputs
are neither copied into each workflow nor exposed as HTTP artifact references.
Registry membership is an allowlist of inputs, not per-caller authorization.

The service saves a successful JSON receipt before sending its HTTP response.
This receipt is application metadata, separate from PCW's checksummed artifact
and checkpoint contracts. A lost response can follow completed work. Retrying
creates a new directory; the service has no retry, resume or idempotency policy.
Retained partial artifacts require deliberate operator inspection and cleanup.

## Next steps

1. Refine the service around a concrete consumer's data source, ownership and
   retention needs before stabilizing a v1 API.
2. Add a streaming log/progress interface only with an explicit framing contract.
3. Consider a PDAL adapter if users need mixed processing stages.
4. Add an optional container or Kubernetes runner after defining durable
   ownership, storage and artifact publication across workers.

Kubernetes, Temporal, Argo, Helm and Fleet are later integrations. This local
runner makes no exactly-once, distributed retry or GPU-validation claim.
