# Releasing err-mcp

Releases are built by CI from tags. This document covers what the workflow
does, how to verify a release by hand, and the optional macOS signing step.

## Cut a release

Tags track the err app version, so the tag matches the app release it pairs
with (`v0.10.0` is the first standalone err-mcp release).

```sh
git tag v0.10.0
git push origin v0.10.0
```

`.github/workflows/release.yml` then:

1. runs `go vet ./...` and `go test ./...`;
2. collects the full license text of every linked module into
   `THIRD_PARTY_LICENSES.txt`;
3. builds `err-mcp` for darwin/linux/windows on arm64 and amd64 with
   `CGO_ENABLED=0` and `-X main.version=<tag>`;
4. packages each target as `err-mcp_<tag>_<os>_<arch>.tar.gz` (`.zip` on
   Windows) containing the binary, `LICENSE`, `README.txt` and
   `THIRD_PARTY_LICENSES.txt`;
5. runs the linux binary with `--version` and checks the stamped tag;
6. writes `SHA256SUMS` and publishes the GitHub release;
7. runs `go install github.com/1l0/err-mcp@<tag>` on a clean runner and checks
   `--version`.

Nothing is published from a branch push; `ci.yml` only vets, tests and builds.

## Build a single target by hand

```sh
GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 go build -trimpath \
  -ldflags "-s -w -X main.version=v0.10.0" -o err-mcp .
```

## Verify a published release

```sh
scripts/install.sh                       # latest, verifies SHA256SUMS
VERSION=v0.10.0 PREFIX=/usr/local scripts/install.sh
go install github.com/1l0/err-mcp@v0.10.0
```

## macOS signing and notarization (optional)

The CI tarballs are unsigned. Signing needs an Apple Developer ID Application
certificate, so it is a separate step:

```sh
codesign --force --options runtime --timestamp \
  --sign "Developer ID Application: Your Name (TEAMID)" err-mcp
codesign --verify --deep --strict --verbose=2 err-mcp

ditto -c -k --keepParent err-mcp_v0.10.0_darwin_arm64 \
  err-mcp_v0.10.0_darwin_arm64.tar.gz

xcrun notarytool submit err-mcp_v0.10.0_darwin_arm64.tar.gz \
  --apple-id "$APPLE_ID" --team-id "$APPLE_TEAM_ID" \
  --password "@keychain:AC_PASSWORD" --wait

xcrun stapler staple err-mcp_v0.10.0_darwin_arm64.tar.gz
```

Regenerate `SHA256SUMS` after signing and upload the signed tarballs instead of
the unsigned CI artifacts. A macOS runner with the signing identity and notary
credentials in secrets can run the same commands.

## Homebrew

`packaging/err-mcp.rb` is a template for a tap such as `1l0/homebrew-tap`. After
a release, set the version and the four `sha256` values from `SHA256SUMS`, then
commit it to the tap.
