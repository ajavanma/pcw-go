# Security

This private repository accepts security reports through its owner and existing
private collaboration channels. Do not put credentials or private datasets in
issues, logs or fixtures. A public vulnerability-reporting policy must be
configured before any future public release.

The executable and environment supplied to the client are trusted configuration.
PCW is a subprocess with the caller's permissions, not a sandbox. Prefer a
dedicated environment, bounded contexts, explicit directories and pinned worker
versions. Treat captured diagnostics as potentially sensitive application data.
