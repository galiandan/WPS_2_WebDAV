# Repository Guidelines

## Project Structure & Module Organization

- `go/` contains the production Go module. `cmd/wps-adapter/` provides the CLI; `internal/` separates configuration, authentication, WPS requests, storage, HTTP/WebDAV, and updates.
- `go/web/` contains embedded HTML, CSS, JavaScript, and images. No Node.js build is required. OpenList reference components and licenses live in `go/web/upstream/openlist/`.
- Go tests sit beside implementation files. `contract_tests/results/` holds sanitized JSON fixtures; `test/ui-offline/` contains browser regressions.
- `wps_login.py` is the standalone desktop login helper. `scripts/` and `deploy/` cover installation; `docs/` describes APIs, architecture, and research.

## Build, Test, and Development Commands

Use Go 1.25+. From `go/`:

```sh
go fmt ./...                 # Format Go sources
go test ./...                # Run Go regressions
go vet ./...                 # Run static analysis
go test -race ./...          # Check concurrency changes
CGO_ENABLED=0 go build -trimpath -o /tmp/wps-adapter ./cmd/wps-adapter
```

Configure using `.env.example` and `docs/deployment.md`, then run:

```sh
/tmp/wps-adapter check-config
/tmp/wps-adapter serve --bind 127.0.0.1 --port 54321
```

From the repository root, validate installers with `bash -n scripts/install-native.sh scripts/install-docker.sh scripts/uninstall.sh`. After login-helper changes, run `python3 -m py_compile wps_login.py`.

## Coding Style & Naming Conventions

Use UTF-8, LF endings, and final newlines. `.editorconfig` defaults to four spaces; use `gofmt` for Go and preserve existing two-space frontend indentation. Use lowercase Go package names, `*_test.go` test files, and descriptive `Test...` functions. Keep WPS transport logic separate from storage mapping and HTTP handlers. Preserve upstream license notices.

## Testing Guidelines

Use Go's standard testing tools and sanitized fixtures; avoid real WPS access in automated tests. No numeric coverage threshold is configured. Cover changed behavior, failures, and relevant concurrency boundaries.

For frontend changes, install Python Playwright and Chromium, then run:

```sh
python3 test/ui-offline/space_navigation.py
python3 test/ui-offline/upload_and_paths.py
```

Set `WPS_UI_BROWSER` to use an existing browser executable.

## Commit & Pull Request Guidelines

Recent commits use `feat(web): ...`, `fix(web): ...`, and `chore(release): ...`. Keep commits focused and describe the behavior change. PRs should explain the problem, resulting behavior, validation, and relevant issues; include screenshots for visual changes. Update `CHANGELOG.md` for user-facing changes and run `git diff --check`.

## Security & Configuration

Never commit credentials, signed URLs, raw captures, or personal deployment details. Use sanitized examples. Record verified WPS behavior in `docs/research/findings.md`; follow `SECURITY.md` and `CONTRIBUTING.md`.

## Mandatory Release Workflow

Every completed change, including documentation or UI edits, must ship in a new version. Treat release as part of completion; no separate permission is needed unless the user explicitly defers it. Update the changelog and installer pins, run applicable checks, commit, push, and tag a new version. Verify CI, GitHub Release assets, and release notes before reporting completion. Report blockers honestly; never claim an unfinished release succeeded.
