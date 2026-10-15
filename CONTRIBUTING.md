# Contributing

Open an issue to discuss a new backend or API change before implementing it.
Keep the client focused on execution contracts; Python PCW owns numerical
processing and artifact verification. Preserve existing protocol fixtures and
add compatibility cases when changing command or response handling.

Use a topic branch and a pull request with a short title. Explain the changed
behavior and the checks you ran. Format Go code with `gofmt`, then run:

```sh
go vet ./...
go test -race ./...
python3 -B -m unittest discover -s scripts -p 'test_*.py'
```

The Go-only suite skips the real worker test unless `PCW_PYTHON` names an
interpreter with the pinned PCW environment installed. Follow the README to
prepare it, then run:

```sh
PCW_PYTHON=/absolute/path/to/python go test -race -count=1 ./...
```

CI tests Linux and macOS, Go 1.25/1.26, and the real CPU workflow with Python
3.11/3.12. All required checks must pass before merge. External contributor
workflows require maintainer approval before execution. Main requires a PR;
the solo-maintainer policy has no mandatory second approving reviewer. Maintainer
review, successful checks and resolution of review discussions remain required.

Use synthetic fixtures. Do not contribute research datasets, model weights,
credentials or private paths. Contributions are licensed under the repository's
MIT license. Report security issues through SECURITY.md.
