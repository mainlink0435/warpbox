# Contributing to Warpbox

Thank you for considering contributing! This document outlines how to get started.

## Reporting Issues

Report bugs and feature requests via [Gitea Issues](http://REDACTED/ben/warpbox/issues).

When filing a bug, include:
- The version you're running (`warpbox --version` or check the startup log)
- Your operating system and architecture
- Steps to reproduce
- Relevant log output (available at `/logs/` when the server is running)

## Development Setup

1. **Prerequisites:**
   - Go 1.26+
   - MinGW-w64 GCC (Windows) or GCC (Linux/macOS) — required by the cgo-based SQLite driver
   - A TorBox API key for testing

2. **Clone and build:**
   ```bash
   git clone http://REDACTED/ben/warpbox.git
   cd warpbox
   go build -o warpbox.exe ./cmd/warpbox/
   ```

3. **Run tests:**
   ```bash
   go test ./... -count=1
   ```

4. **Local testing:**
   ```bash
   warpbox.exe --config config.yml --db warpbox.db
   ```
   Then browse to `http://localhost:1412/` and `http://localhost:1412/http/`.

## Code Style

- Follow standard Go conventions (`gofmt`, `go vet`).
- Use [conventional commits](https://www.conventionalcommits.org/) for commit messages (`feat:`, `fix:`, `refactor:`, `docs:`, `chore:`).
- Include `refs #N` in commit messages to reference issues (Gitea does not support auto-close keywords).
- Run `go vet ./...` before committing.

## Pull Request Process

1. Create a feature branch from `main`.
2. Make your changes, ensuring all tests pass.
3. Open a pull request against `main`.
4. A maintainer will review and merge.

## License

By contributing, you agree that your contributions will be licensed under the GNU General Public License v3.0.