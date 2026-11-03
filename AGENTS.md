# err-mcp — AGENTS.md

## What this is

`err-mcp` is a local stdio MCP server for the err web app. It stores no vault
data: it relays MCP tool calls over a loopback WebSocket to the open app, which
owns the vault (SQLite in OPFS).

One command: `main.go` → `err-mcp`, the MCP server (stdio) plus the app bridge
(HTTP/WS).

This module is deliberately standalone: no code generation, no dependency on
generated Go packages, installable with
`go install github.com/1l0/err-mcp@latest`.

## Layout

- `main.go` — flags, token/origin files, HTTP bridge wiring, stdio run loop.
- `internal/mcp/` — the server: `server.go` (catalog + tool dispatch),
  `catalog.go` (tools derived from descriptors), `convenience.go`
  (`list_titles`/`outline`/`search_items`), `bridge.go` (app WebSocket),
  `descriptor.go` + `domain.fds` (embedded protocol descriptor), `audit.go`,
  `token.go`, `origin.go`, `pair.go`.
- `scripts/install.sh` — verified download of a release tarball.
- `scripts/third-party-licenses.sh` — full license texts for the release.
- `.github/workflows/` — `ci.yml` (push/PR), `release.yml` (tags only).

## Rules

- **No generated Go types.** The protocol schema comes from the embedded
  `internal/mcp/domain.fds`. Do not add a dependency on generated protobuf
  packages from the app; keeping this module portable is the point.
- **The descriptor is committed.** It is produced by the app's protocol
  tooling. `TestCatalogFingerprint` pins the expected value (it mirrors the
  fingerprint the app computes), so never edit `domain.fds` by hand and update
  the test only when the protocol actually changes.
- **Tools are derived, not hand-listed.** A method in the protocol becomes a
  tool; `methodMeta` in `catalog.go` supplies the description, required fields,
  tier and safety hints. A new method without an entry is advertised only in the
  sync tier on purpose.
- **Tiers are the security boundary.** `safe` (default) must never be able to
  rekey a vault, import segments, GC or disclose the vault key. Destructive sync
  tools need both `--allow-sync` and `--allow-destructive`.
- **stdout is MCP.** All logging goes to stderr or the optional audit file.
- **CGO-free cross builds.** The release matrix is darwin/linux/windows on
  arm64 and amd64 with `CGO_ENABLED=0`; keep it that way.
- **License:** MIT (see `LICENSE`). Every release tarball ships `LICENSE` and
  `THIRD_PARTY_LICENSES.txt`; tags track the err app version.
- **Language:** documents and code comments in English.
- **Commits:** only when explicitly asked.

## Checks

```sh
go vet ./... && go test ./...
```
