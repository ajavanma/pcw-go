# Security

Report vulnerabilities privately through
[GitHub vulnerability reporting](https://github.com/ajavanma/pcw-go/security/advisories/new).
Include the affected revision, a minimal reproduction, and the expected impact.
Do not disclose vulnerabilities, credentials or private datasets in public
issues, logs or fixtures.

The API is experimental with no tagged release. Security fixes currently target
the latest `main`; pin reviewed revisions and update when fixes are available.
For issues in the Python worker, use
[PCW's reporting policy](https://github.com/ajavanma/PointCloud-Workflows/blob/main/SECURITY.md).

The executable and environment supplied to the client are trusted configuration.
PCW is a subprocess with the caller's permissions, not a sandbox. Prefer a
dedicated environment, bounded contexts, explicit directories and pinned worker
versions. Treat captured diagnostics as potentially sensitive application data.
