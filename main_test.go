package main

import (
	"bytes"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/1l0/err-mcp/internal/mcp"
)

func TestPrintBannerHidesTheToken(t *testing.T) {
	secret := strings.Repeat("a", 64)
	for _, created := range []bool{false, true} {
		var out bytes.Buffer
		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		printBanner(&out, logger, "127.0.0.1:8787", tokenState{
			value:   secret,
			file:    "/tmp/err/mcp-token",
			created: created,
		}, true, false)

		text := out.String()
		if strings.Contains(text, secret) {
			t.Fatalf("banner printed the token (created=%v):\n%s", created, text)
		}
		if !strings.Contains(text, mcp.TokenFingerprint(secret)) {
			t.Fatalf("banner lacks the fingerprint (created=%v):\n%s", created, text)
		}
		if !strings.Contains(text, "/tmp/err/mcp-token") {
			t.Fatalf("banner lacks the token file path (created=%v):\n%s", created, text)
		}
	}
}

func TestResolveVersion(t *testing.T) {
	cases := []struct {
		name     string
		stamped  string
		module   string
		expected string
	}{
		{"release build wins", "v0.10.0", "v0.9.0", "v0.10.0"},
		{"go install records the tag", "dev", "v0.10.0", "v0.10.0"},
		{"local build", "dev", "(devel)", "dev"},
		{"no build info", "dev", "", "dev"},
		{"empty stamp", "", "v0.10.0", "v0.10.0"},
	}
	for _, tc := range cases {
		if got := resolveVersion(tc.stamped, tc.module); got != tc.expected {
			t.Errorf("%s: resolveVersion(%q, %q) = %q, want %q", tc.name, tc.stamped, tc.module, got, tc.expected)
		}
	}
}

func TestPrintBannerWithoutPairingShowsTheToken(t *testing.T) {
	secret := strings.Repeat("b", 64)
	var out bytes.Buffer
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	printBanner(&out, logger, "127.0.0.1:8787", tokenState{value: secret}, false, false)

	// Without /pair the token URL is the only way in, so printing it is
	// intentional.
	if !strings.Contains(out.String(), secret) {
		t.Fatalf("banner without pairing does not show the token:\n%s", out.String())
	}
}
