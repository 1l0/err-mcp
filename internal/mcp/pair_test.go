//go:build !js

package mcp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func pairRequest(method, origin, pairHeader string) *http.Request {
	req := httptest.NewRequest(method, "/pair", nil)
	req.Host = "127.0.0.1:8787"
	req.RemoteAddr = "127.0.0.1:54321"
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	if pairHeader != "" {
		req.Header.Set("X-Err-Pair", pairHeader)
	}
	return req
}

func pairToken(t *testing.T, b *Bridge, req *http.Request) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	b.ServePair(rec, req)
	var body map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return rec.Code, body["token"]
}

func TestPairFirstOriginIsPersisted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "err", OriginFileName)
	bridge := NewBridge(BridgeConfig{
		Token:               "secret",
		PairFile:            path,
		EnforceLoopbackHost: true,
		Logger:              testLogger(),
	})

	status, token := pairToken(t, bridge, pairRequest(http.MethodGet, "https://app.example", "1"))
	if status != http.StatusOK || token != "secret" {
		t.Fatalf("pair = %d %q, want 200 secret", status, token)
	}

	origin, err := ReadPairedOrigin(path)
	if err != nil {
		t.Fatalf("ReadPairedOrigin: %v", err)
	}
	if origin != "https://app.example" {
		t.Fatalf("paired origin = %q", origin)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("origin permissions = %o, want 0600", perm)
	}

	// The same origin keeps working.
	if status, _ := pairToken(t, bridge, pairRequest(http.MethodGet, "https://app.example", "1")); status != http.StatusOK {
		t.Fatalf("second pair = %d, want 200", status)
	}
	// A different origin is rejected and does not overwrite the file.
	if status, _ := pairToken(t, bridge, pairRequest(http.MethodGet, "https://evil.example", "1")); status != http.StatusForbidden {
		t.Fatalf("other origin = %d, want 403", status)
	}
	origin, _ = ReadPairedOrigin(path)
	if origin != "https://app.example" {
		t.Fatalf("paired origin changed to %q", origin)
	}
}

func TestPairLocalhostOriginNeedsNoPairing(t *testing.T) {
	path := filepath.Join(t.TempDir(), OriginFileName)
	if err := WritePairedOrigin(path, "https://app.example"); err != nil {
		t.Fatal(err)
	}
	bridge := NewBridge(BridgeConfig{
		Token:               "secret",
		AllowedOrigins:      []string{"https://app.example"},
		PairFile:            path,
		EnforceLoopbackHost: true,
		Logger:              testLogger(),
	})

	for _, origin := range []string{"http://localhost", "http://localhost:5173"} {
		status, token := pairToken(t, bridge, pairRequest(http.MethodGet, origin, "1"))
		if status != http.StatusOK || token != "secret" {
			t.Fatalf("pair %q = %d %q, want 200 secret", origin, status, token)
		}
	}

	// A trusted localhost origin must not replace the paired origin.
	paired, err := ReadPairedOrigin(path)
	if err != nil {
		t.Fatal(err)
	}
	if paired != "https://app.example" {
		t.Fatalf("paired origin = %q", paired)
	}
}

func TestPairRejectsLocalhostLookalikes(t *testing.T) {
	bridge := NewBridge(BridgeConfig{
		Token:               "secret",
		AllowedOrigins:      []string{"https://app.example"},
		EnforceLoopbackHost: true,
		Logger:              testLogger(),
	})

	for _, origin := range []string{
		"http://localhost.evil.com",
		"https://localhost:3000",
	} {
		if status, _ := pairToken(t, bridge, pairRequest(http.MethodGet, origin, "1")); status != http.StatusForbidden {
			t.Fatalf("pair %q = %d, want 403", origin, status)
		}
	}
}

func TestPairRejectsMissingOriginAndHeader(t *testing.T) {
	path := filepath.Join(t.TempDir(), OriginFileName)
	bridge := NewBridge(BridgeConfig{Token: "secret", PairFile: path, EnforceLoopbackHost: true, Logger: testLogger()})

	if status, _ := pairToken(t, bridge, pairRequest(http.MethodGet, "", "1")); status != http.StatusForbidden {
		t.Fatalf("missing origin = %d, want 403", status)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("missing-origin request created the pairing file: %v", err)
	}

	if status, _ := pairToken(t, bridge, pairRequest(http.MethodGet, "https://app.example", "")); status != http.StatusForbidden {
		t.Fatalf("missing pair header = %d, want 403", status)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("header-less request created the pairing file: %v", err)
	}
}

func TestPairRequiresLoopbackPeer(t *testing.T) {
	bridge := NewBridge(BridgeConfig{
		Token:               "secret",
		PairFile:            filepath.Join(t.TempDir(), OriginFileName),
		EnforceLoopbackHost: true,
		Logger:              testLogger(),
	})
	req := pairRequest(http.MethodGet, "https://app.example", "1")
	req.RemoteAddr = "203.0.113.10:54321"
	if status, _ := pairToken(t, bridge, req); status != http.StatusForbidden {
		t.Fatalf("remote peer = %d, want 403", status)
	}
}

func TestPairPreflightDoesNotWriteTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), OriginFileName)
	bridge := NewBridge(BridgeConfig{Token: "secret", PairFile: path, EnforceLoopbackHost: true, Logger: testLogger()})

	preflight := pairRequest(http.MethodOptions, "https://app.example", "")
	preflight.Header.Set("Access-Control-Request-Private-Network", "true")
	rec := httptest.NewRecorder()
	bridge.ServePair(rec, preflight)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("preflight = %d, want 204", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Private-Network"); got != "true" {
		t.Fatalf("private-network preflight header = %q, want true", got)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("preflight created the pairing file: %v", err)
	}

	status, token := pairToken(t, bridge, pairRequest(http.MethodGet, "https://app.example", "1"))
	if status != http.StatusOK || token != "secret" {
		t.Fatalf("pair after preflight = %d %q", status, token)
	}
}

func TestPairAllowedOriginsDoNotUseTOFU(t *testing.T) {
	path := filepath.Join(t.TempDir(), OriginFileName)
	bridge := NewBridge(BridgeConfig{
		Token:               "secret",
		PairFile:            path,
		AllowedOrigins:      []string{"https://app.example"},
		EnforceLoopbackHost: true,
		Logger:              testLogger(),
	})

	if status, token := pairToken(t, bridge, pairRequest(http.MethodGet, "https://app.example", "1")); status != http.StatusOK || token != "secret" {
		t.Fatalf("allowed origin = %d %q", status, token)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("allowlisted pairing wrote the TOFU file: %v", err)
	}
	if status, _ := pairToken(t, bridge, pairRequest(http.MethodGet, "https://bad.example", "1")); status != http.StatusForbidden {
		t.Fatalf("other origin = %d, want 403", status)
	}
}

func TestPairDefaultOriginNeedsNoConfiguration(t *testing.T) {
	path := filepath.Join(t.TempDir(), OriginFileName)
	bridge := NewBridge(BridgeConfig{
		Token:               "secret",
		PairFile:            path,
		EnforceLoopbackHost: true,
		Logger:              testLogger(),
	})

	if status, token := pairToken(t, bridge, pairRequest(http.MethodGet, "https://err.tokyo", "1")); status != http.StatusOK || token != "secret" {
		t.Fatalf("default origin = %d %q, want 200 secret", status, token)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("default origin wrote the TOFU pairing file: %v", err)
	}
}

func TestPairDefaultOriginWorksWithAnotherPairedOrigin(t *testing.T) {
	path := filepath.Join(t.TempDir(), OriginFileName)
	bridge := NewBridge(BridgeConfig{
		Token:               "secret",
		PairFile:            path,
		EnforceLoopbackHost: true,
		Logger:              testLogger(),
	})

	if status, _ := pairToken(t, bridge, pairRequest(http.MethodGet, "https://app.example", "1")); status != http.StatusOK {
		t.Fatalf("first pair = %d", status)
	}
	if status, _ := pairToken(t, bridge, pairRequest(http.MethodGet, "https://err.tokyo", "1")); status != http.StatusOK {
		t.Fatalf("default origin after another pairing = %d, want 200", status)
	}
	origin, err := ReadPairedOrigin(path)
	if err != nil {
		t.Fatalf("ReadPairedOrigin: %v", err)
	}
	if origin != "https://app.example" {
		t.Fatalf("paired origin changed to %q", origin)
	}
}

func TestPairReloadsThePersistedOrigin(t *testing.T) {
	path := filepath.Join(t.TempDir(), OriginFileName)
	first := NewBridge(BridgeConfig{Token: "secret", PairFile: path, EnforceLoopbackHost: true, Logger: testLogger()})
	if status, _ := pairToken(t, first, pairRequest(http.MethodGet, "https://app.example", "1")); status != http.StatusOK {
		t.Fatalf("first pair = %d", status)
	}

	second := NewBridge(BridgeConfig{Token: "secret", PairFile: path, EnforceLoopbackHost: true, Logger: testLogger()})
	if status, _ := pairToken(t, second, pairRequest(http.MethodGet, "https://app.example", "1")); status != http.StatusOK {
		t.Fatalf("paired origin after restart = %d, want 200", status)
	}
	if status, _ := pairToken(t, second, pairRequest(http.MethodGet, "https://other.example", "1")); status != http.StatusForbidden {
		t.Fatalf("other origin after restart = %d, want 403", status)
	}
}

func TestPairDisabledWithoutFile(t *testing.T) {
	bridge := NewBridge(BridgeConfig{Token: "secret", EnforceLoopbackHost: true, Logger: testLogger()})
	if status, _ := pairToken(t, bridge, pairRequest(http.MethodGet, "https://app.example", "1")); status != http.StatusForbidden {
		t.Fatalf("pair without a file = %d, want 403", status)
	}
}

func TestPairRejectsInvalidOrigin(t *testing.T) {
	bridge := NewBridge(BridgeConfig{
		Token:               "secret",
		PairFile:            filepath.Join(t.TempDir(), OriginFileName),
		EnforceLoopbackHost: true,
		Logger:              testLogger(),
	})
	for _, origin := range []string{"file:///tmp/app", "https://app.example/path", "not-an-origin"} {
		if status, _ := pairToken(t, bridge, pairRequest(http.MethodGet, origin, "1")); status != http.StatusForbidden {
			t.Errorf("origin %q = %d, want 403", origin, status)
		}
	}
}

func TestIsLoopbackRemote(t *testing.T) {
	for addr, want := range map[string]bool{
		"127.0.0.1:1234": true,
		"[::1]:1234":     true,
		"203.0.113.10:1": false,
		"":               false,
	} {
		if got := isLoopbackRemote(addr); got != want {
			t.Errorf("isLoopbackRemote(%q) = %v, want %v", addr, got, want)
		}
	}
}
