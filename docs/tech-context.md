# Tech Context: Warpbox

## Development Environment

- Local OS: Windows (primary debugging and testing environment)
- Toolchain: Go (Golang) latest stable version
- Execution: Commands run locally via `go run` or compiled via `go build` for Windows testing
- IDE: VS Code with Cline extension

## Local Debugging

Although the CI/CD pipeline is set up, it takes time to build, release, and deploy into Docker. For most iterative development, use a locally built .exe and rclone.exe to test basic WebDAV behaviour without touching production.

Include a UTC build timestamp in the version string:

```powershell
$ts = (Get-Date).ToUniversalTime().ToString('yyyy-MM-ddTHH:mm:ss\Z')
go build -ldflags="-X main.Version=dev-$ts" -o warpbox.exe ./cmd/warpbox/
.\warpbox.exe --config config.yml --db test.db
```

For integration testing that requires the real Plex/rclone stack on REDACTED (WebDAV performance, VFS caching, CDN hot-swap timing), use the dev-deploy hot-swap script:

```
.\dev-deploy script
```

## Build Targets & Cross-Compilation

- Go's native cross-compilation generates standalone executables
- Target Architectures: amd64 (x64), 386 (x86), arm64
- Target Operating Systems: windows, linux, darwin (macOS)

## CI/CD Pipeline

- Platform: GitHub Actions
- Workflow: Automated linting, testing, and compilation triggered upon tagging a release
- Artefacts: Standalone binaries outputted for all target OS/Architecture combinations
