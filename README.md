# err-mcp

A local [MCP](https://modelcontextprotocol.io) server for the **err** app — a
local-first structured editor whose vault lives in the browser (OPFS).

`err-mcp` holds no vault data. It is a relay: MCP clients talk JSON-RPC to this
process over stdin/stdout, and the process forwards each call over a loopback
WebSocket to the open err web app, which executes it in its own engine. Nothing
is reimplemented here and nothing is stored here.

```
MCP client  ──stdio──▶  err-mcp  ──ws://127.0.0.1:8787/app/ws──▶  err web app (vault)
```

## Install

With Go 1.25 or newer — this is the path an agent can run unattended:

```sh
go install github.com/1l0/err-mcp@latest
```

Prebuilt binaries for macOS, Linux and Windows (arm64/amd64) are attached to
every [release](https://github.com/1l0/err-mcp/releases). The release script
verifies `SHA256SUMS`:

```sh
scripts/install.sh                 # latest release into ~/.local/bin
PREFIX=/usr/local scripts/install.sh
VERSION=v0.10.0 scripts/install.sh
```

## Configure your MCP client

`err-mcp` speaks MCP over stdin/stdout, so the client launches it. For OpenCode:

```jsonc
{
  "$schema": "https://opencode.ai/config.json",
  "mcp": {
    "servers": {
      "err": {
        "type": "local",
        "command": ["err-mcp"],
        "timeout": { "startup": 60000, "execution": 120000 }
      }
    }
  }
}
```

Then open the err web app, click the **MCP** button in the header, enable
**Local MCP** and confirm the port (default `8787`). The app pairs
automatically. Stop any other `err-mcp` first: the launched process owns the
bridge port.

Check the connection with `curl -s http://127.0.0.1:8787/healthz` or call
`err_status` from the MCP client.

## Flags

| Flag | Meaning |
| --- | --- |
| `--port <n>` | App-bridge HTTP port (`MCP_PORT`, default `8787`). |
| `--host <addr>` | Bind address (`MCP_HOST`, default `127.0.0.1`). Keep it loopback. |
| `--rotate-token` | Generate a new app-bridge token, save it, print it once and exit. |
| `--show-token` | Print the app-bridge token and exit (explicit opt-in). |
| `--reset-pairing` | Forget the paired app origin and exit. |
| `--token-file <path>` | Override the token file (`MCP_TOKEN_FILE`). |
| `--origin-file <path>` | Override the paired-origin file (`MCP_ORIGIN_FILE`). |
| `--toolset=all` | Advertise every tier (sync, destructive and secret). |
| `--allow-sync` | Add segment/snapshot tools. |
| `--allow-destructive` | Add `import_segment`, `adopt_vault_key` and `run_gc`; requires `--allow-sync`. |
| `--allow-secrets` | Add `get_vault_key`. |
| `--audit-log <path>` | Also append one JSONL audit entry per tool call (`MCP_AUDIT_LOG`). |
| `--rate-limit <n>` | Per-tool call limit per minute; 0 disables (`MCP_RATE_LIMIT`). |
| `--static <dir>` | Serve a built web app on the same origin (no loopback permission prompt). |
| `--origins <list>` | Comma-separated allowed browser origins for `/app/ws`; `https://err.tokyo` and `http://localhost[:port]` are always allowed. |

## Security model

- The MCP side has no token, OAuth or remote endpoint: the client owns the
  process and its stdio.
- The app bridge is token-authenticated and bound to loopback by default. The
  token is generated on first start and stored at
  `os.UserConfigDir()/err/mcp-token` (`~/Library/Application Support/err/` on
  macOS, `~/.config/err/` on Linux), directory `0700`, file `0600`.
- `GET /pair` hands the token to the browser only for the paired origin, which
  is recorded in `os.UserConfigDir()/err/mcp-origin`.
- Tools are tiered: the default toolset cannot rekey the vault, import
  segments, run GC or disclose the vault key.
- `--audit-log` records one JSONL line per call; stderr always gets the log.

## Tools

The catalog is generated from the app's protocol descriptor, which is embedded
at build time (see [Development](#development)), so it always matches the app's
RPC surface. `err_status` reports the tool count, the toolset and the
`catalog_fingerprint` the app also computes; a mismatch means the app and the
server were built from different protocol versions.

The default `safe` tier covers reading and editing Items, the parent/child
graph, tree views and undo/redo. Three projection tools keep results small:
`list_titles`, `outline` and `search_items`.

Bulk import is the `create_tree` tool: it creates a nested tree of Items in one
call and one undo step. An agent that can read local files can build the
arguments itself.

## Development

```sh
go vet ./...
go test ./...
go build .
```

Layout: `main.go` is the command; `internal/mcp` is the server (catalog, tool
handlers, app bridge, audit, tokens, embedded descriptor).

`internal/mcp/domain.fds` is a compiled `FileDescriptorSet` of the app's RPC
protocol, embedded with `go:embed`. It is committed on purpose: that is what
lets anyone run `go install` with no code generation step and no dependency on
generated Go packages. It is produced by the app's protocol tooling and never
edited by hand. `TestCatalogFingerprint` pins the expected descriptor, so a
stale `.fds` fails the build instead of silently advertising the wrong schema.

## Releasing

Tags track the err app version — `v0.10.0` is the first standalone release — so
comparing `err-mcp --version` with the app's `app_version` stays meaningful.
Release builds stamp the tag with `-ldflags`; a binary installed with
`go install ...@v0.10.0` reports the module version Go recorded, and a build
from a checkout reports its pseudo-version.

Pushing a `v*` tag runs `.github/workflows/release.yml`: it vets, tests, builds
all six platform targets, packages them with `LICENSE` and
`THIRD_PARTY_LICENSES.txt`, writes `SHA256SUMS`, publishes the GitHub release,
and then verifies `go install github.com/1l0/err-mcp@<tag>` on a clean runner.
Branch pushes only run `ci.yml`.

macOS tarballs are unsigned; sign and notarize them separately if you publish
them to a Homebrew tap (`packaging/err-mcp.rb` is a template). See
[`docs/RELEASING.md`](docs/RELEASING.md).

## License

MIT — see [LICENSE](LICENSE).
