# Homebrew formula for err-mcp.
#
# Publish this in a tap (for example `1l0/homebrew-tap`) after a release:
#   1. Push a `vX.Y.Z` tag; the release workflow publishes the tarballs.
#   2. Replace the version and the four sha256 values from the release's
#      SHA256SUMS.
#   3. Commit the formula to the tap.
#
# Tags are `vX.Y.Z` and the asset names keep the `v`.
class ErrMcp < Formula
  desc "Local stdio MCP server for the err web app"
  homepage "https://github.com/1l0/err-mcp"
  version "0.10.1"

  on_macos do
    if Hardware::CPU.arm?
      url "https://github.com/1l0/err-mcp/releases/download/v#{version}/err-mcp_v#{version}_darwin_arm64.tar.gz"
      sha256 "REPLACE_WITH_DARWIN_ARM64_SHA256"
    else
      url "https://github.com/1l0/err-mcp/releases/download/v#{version}/err-mcp_v#{version}_darwin_amd64.tar.gz"
      sha256 "REPLACE_WITH_DARWIN_AMD64_SHA256"
    end
  end

  on_linux do
    if Hardware::CPU.arm?
      url "https://github.com/1l0/err-mcp/releases/download/v#{version}/err-mcp_v#{version}_linux_arm64.tar.gz"
      sha256 "REPLACE_WITH_LINUX_ARM64_SHA256"
    else
      url "https://github.com/1l0/err-mcp/releases/download/v#{version}/err-mcp_v#{version}_linux_amd64.tar.gz"
      sha256 "REPLACE_WITH_LINUX_AMD64_SHA256"
    end
  end

  def install
    bin.install "err-mcp"
  end

  test do
    assert_match version.to_s, shell_output("#{bin}/err-mcp --version")
  end
end
