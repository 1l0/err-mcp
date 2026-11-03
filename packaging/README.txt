err-mcp
=======

err-mcp is a local stdio MCP server for the err web app. Start it from your MCP
client (OpenCode, Claude Desktop, Cursor) with the app open and Local MCP
enabled in the app's Settings.

Install with Go:

  go install github.com/1l0/err-mcp@latest

Or use scripts/install.sh from the release, which verifies SHA256SUMS.

The err vault stays in the app (OPFS); err-mcp only relays MCP tool calls to
the open app over a loopback bridge and never stores vault data.

err-mcp is MIT licensed (see LICENSE). The full license text of every linked
Go module is in THIRD_PARTY_LICENSES.txt.

See https://github.com/1l0/err-mcp for the runbook, flags and security model.
The tarballs in this release are unsigned.
