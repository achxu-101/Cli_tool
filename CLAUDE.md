# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Build & Development

```bash
# Build for Linux/amd64 (the only supported target)
make build

# Install to /usr/local/bin
make install

# Lint
make lint          # requires golangci-lint

# Run directly (must be on Linux, requires root)
sudo go run ./cmd/upgrador [--dry-run] [--offline] [--version] [--update]
```

There are no tests. The binary targets Linux amd64 exclusively (`CGO_ENABLED=0 GOOS=linux GOARCH=amd64`).

## Architecture

`upgrador` is a terminal UI tool that discovers upgradeable software on a Linux system and performs the upgrades interactively. It requires root.

**Data flow:** `scanner` → `resolver` → TUI → `upgrader`

### Packages

| Package | Role |
|---|---|
| `cmd/upgrador/main.go` | Entry point: root check, flag parsing, self-update, launches Bubble Tea TUI |
| `internal/config/` | Persists user-taught upgrade methods to `~/.config/upgrador/known.json` |
| `internal/lookup/` | Static registry of known binaries and services with their upgrade methods |
| `internal/scanner/` | Discovers installed components (apt packages, binaries in `/usr/{local/,}{s,}bin`, active systemd services, Helm releases) and probes their versions |
| `internal/resolver/` | Concurrently resolves the latest available version for each component via GitHub API or `helm search` |
| `internal/upgrader/` | Executes upgrades, streaming output to an `io.Writer` used by the TUI |
| `internal/tui/` | Bubble Tea model for the 6-screen wizard; also contains self-update check logic |

### Upgrade Methods

Defined as constants in `internal/lookup/lookup.go`:

- `github_tarball` / `github_binary` — download asset from GitHub releases
- `rancher_script` — Docker via `releases.rancher.com`
- `k3s_script` — K3s via `get.k3s.io`
- `helm_script` — Helm via official script
- `apt` — `apt-get dist-upgrade`
- `helm_upgrade` — `helm upgrade --reuse-values`
- `custom_script` — user-specified URL or shell command
- `skip` — mark as managed elsewhere

### TUI Flow

Six screens in order: **Scan** → **Group Select** → **Component Select** (per group) → **Confirm** → **Upgrade** → **Summary**. The `screen` iota in `tui.go` controls which `update*`/`view*` methods are dispatched.

### Adding a Known Binary or Service

Edit the `knownBinaries` or `knownServices` slice in `internal/lookup/lookup.go`. The `init()` function builds lookup maps automatically. User overrides (stored in config) always take priority over the built-in table.

### Config File

`~/.config/upgrador/known.json` — stores user-taught entries (`UserBinary`) for binaries whose upgrade method isn't in the built-in lookup table, or overrides for known ones.

### Helm Chart Convention

In scanner/resolver/upgrader, `Component.GithubRepo` holds the **Helm repo name** and `Component.AptPackage` holds the **chart name** for Helm releases (field names are reused from the binary context).
