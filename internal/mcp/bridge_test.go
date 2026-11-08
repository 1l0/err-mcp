//go:build !js

package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newTestBridge(t *testing.T, cfg BridgeConfig) (*Bridge, *httptest.Server) {
	t.Helper()
	if cfg.Logger == nil {
		cfg.Logger = testLogger()
	}
	bridge := NewBridge(cfg)
	mux := http.NewServeMux()
	mux.Handle("/app/ws", bridge)
	srv := httptest.NewServer(mux)
	t.Cleanup(func() {
		srv.Close()
		bridge.Close()
	})
	return bridge, srv
}

func dialApp(t *testing.T, srv *httptest.Server, token, origin string) (*websocket.Conn, *http.Response) {
	t.Helper()
	u := "ws" + strings.TrimPrefix(srv.URL, "http") + "/app/ws"
	if token != "" {
		u += "?token=" + token
	}
	header := http.Header{}
	if origin != "" {
		header.Set("Origin", origin)
	}
	conn, resp, err := websocket.DefaultDialer.Dial(u, header)
	if err != nil {
		return nil, resp
	}
	return conn, resp
}

func waitConnected(t *testing.T, bridge *Bridge) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if ok, _, _ := bridge.Status(); ok {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("bridge did not report a connected app")
}

func TestBridgeCallSuccess(t *testing.T) {
	bridge, srv := newTestBridge(t, BridgeConfig{Token: "secret", EnforceLoopbackHost: true})
	conn, resp := dialApp(t, srv, "secret", "")
	if conn == nil {
		t.Fatalf("dial: %v", resp)
	}
	defer conn.Close()

	go func() {
		for {
			_, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var f bridgeFrame
			if err := json.Unmarshal(data, &f); err != nil {
				return
			}
			if f.Type != "request" {
				continue
			}
			_ = conn.WriteJSON(bridgeFrame{Type: "response", ID: f.ID, Payload: f.Payload})
		}
	}()
	waitConnected(t, bridge)

	got, err := bridge.Call(context.Background(), "/pb.ItemService/GetItem", []byte("hello"))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if string(got) != "hello" {
		t.Fatalf("payload = %q, want hello", got)
	}

	connected, _, pending := bridge.Status()
	if !connected || pending != 0 {
		t.Fatalf("status = connected:%v pending:%d", connected, pending)
	}
}

func TestBridgeCallRPCError(t *testing.T) {
	bridge, srv := newTestBridge(t, BridgeConfig{Token: "secret", EnforceLoopbackHost: true})
	conn, resp := dialApp(t, srv, "secret", "")
	if conn == nil {
		t.Fatalf("dial: %v", resp)
	}
	defer conn.Close()

	go func() {
		for {
			_, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var f bridgeFrame
			if err := json.Unmarshal(data, &f); err != nil {
				return
			}
			_ = conn.WriteJSON(bridgeFrame{
				Type: "response",
				ID:   f.ID,
				Error: &bridgeError{
					Code:    500,
					Message: "item not found",
				},
			})
		}
	}()
	waitConnected(t, bridge)

	_, err := bridge.Call(context.Background(), "/pb.ItemService/GetItem", nil)
	var rpcErr *RPCError
	if !errors.As(err, &rpcErr) {
		t.Fatalf("Call error = %v, want *RPCError", err)
	}
	if rpcErr.Code != 500 || rpcErr.Message != "item not found" {
		t.Fatalf("RPCError = %+v", rpcErr)
	}
}

func TestBridgeCallWithoutApp(t *testing.T) {
	bridge := NewBridge(BridgeConfig{Logger: testLogger()})
	defer bridge.Close()
	_, err := bridge.Call(context.Background(), "/pb.ItemService/GetItem", nil)
	if !errors.Is(err, ErrAppNotConnected) {
		t.Fatalf("Call error = %v, want ErrAppNotConnected", err)
	}
}

func TestBridgeRejectsBadToken(t *testing.T) {
	_, srv := newTestBridge(t, BridgeConfig{Token: "secret", EnforceLoopbackHost: true})
	conn, resp := dialApp(t, srv, "wrong", "")
	if conn != nil {
		conn.Close()
		t.Fatal("dial with a wrong token succeeded")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %v, want 401", resp)
	}
}

func TestBridgeRejectsBadOrigin(t *testing.T) {
	_, srv := newTestBridge(t, BridgeConfig{
		Token:               "secret",
		AllowedOrigins:      []string{"https://ok.example"},
		EnforceLoopbackHost: true,
	})
	conn, resp := dialApp(t, srv, "secret", "https://evil.example")
	if conn != nil {
		conn.Close()
		t.Fatal("dial with a bad origin succeeded")
	}
	if resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %v, want 403", resp)
	}
}

func TestBridgeAcceptsAllowedOrigin(t *testing.T) {
	_, srv := newTestBridge(t, BridgeConfig{
		Token:               "secret",
		AllowedOrigins:      []string{"https://ok.example"},
		EnforceLoopbackHost: true,
	})
	conn, resp := dialApp(t, srv, "secret", "https://ok.example")
	if conn == nil {
		t.Fatalf("dial: %v", resp)
	}
	conn.Close()
}

func TestBridgeNormalizesAllowedOrigin(t *testing.T) {
	_, srv := newTestBridge(t, BridgeConfig{
		Token:               "secret",
		AllowedOrigins:      []string{" https://ok.example/ "},
		EnforceLoopbackHost: true,
	})
	conn, resp := dialApp(t, srv, "secret", "https://ok.example")
	if conn == nil {
		t.Fatalf("dial: %v", resp)
	}
	conn.Close()
}

func TestIsLocalhostOrigin(t *testing.T) {
	trusted := []string{
		"http://localhost",
		"http://localhost:3000",
		"http://localhost:5173",
		"http://LOCALHOST:8080",
	}
	for _, origin := range trusted {
		if !isLocalhostOrigin(origin) {
			t.Errorf("isLocalhostOrigin(%q) = false, want true", origin)
		}
	}
	rejected := []string{
		"https://localhost:3000",
		"http://localhost.evil.com",
		"http://localhost.:3000",
		"http://127.0.0.1:3000",
		"http://localhost/path",
		"ftp://localhost",
		"not an origin",
	}
	for _, origin := range rejected {
		if isLocalhostOrigin(origin) {
			t.Errorf("isLocalhostOrigin(%q) = true, want false", origin)
		}
	}
}

func TestBridgeAcceptsLocalhostOriginWithAllowlist(t *testing.T) {
	_, srv := newTestBridge(t, BridgeConfig{
		Token:               "secret",
		AllowedOrigins:      []string{"https://app.example"},
		EnforceLoopbackHost: true,
	})

	conn, resp := dialApp(t, srv, "secret", "http://localhost:3000")
	if conn == nil {
		t.Fatalf("localhost origin was rejected: %v", resp)
	}
	_ = conn.Close()
}

func TestBridgeRejectsLocalhostLookalikes(t *testing.T) {
	_, srv := newTestBridge(t, BridgeConfig{
		Token:               "secret",
		AllowedOrigins:      []string{"https://app.example"},
		EnforceLoopbackHost: true,
	})

	for _, origin := range []string{
		"http://localhost.evil.com",
		"https://localhost:3000",
	} {
		conn, resp := dialApp(t, srv, "secret", origin)
		if conn != nil {
			_ = conn.Close()
			t.Fatalf("origin %q was accepted", origin)
		}
		if resp == nil || resp.StatusCode != http.StatusForbidden {
			t.Fatalf("origin %q status = %v, want 403", origin, resp)
		}
	}
}

func TestBridgeAcceptsDefaultOrigin(t *testing.T) {
	_, srv := newTestBridge(t, BridgeConfig{
		Token:               "secret",
		AllowedOrigins:      []string{"https://other.example"},
		EnforceLoopbackHost: true,
	})
	conn, resp := dialApp(t, srv, "secret", "https://err.tokyo")
	if conn == nil {
		t.Fatalf("dial: %v", resp)
	}
	conn.Close()
}

func TestBridgeAuthorizeRejectsNonLoopbackHost(t *testing.T) {
	bridge := NewBridge(BridgeConfig{Token: "secret", EnforceLoopbackHost: true, Logger: testLogger()})
	req := httptest.NewRequest(http.MethodGet, "/app/ws?token=secret", nil)
	req.Host = "evil.example"
	status, err := bridge.authorize(req)
	if err == nil {
		t.Fatal("authorize accepted a non-loopback Host")
	}
	if status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", status)
	}
}

func TestIsLoopbackHost(t *testing.T) {
	for host, want := range map[string]bool{
		"127.0.0.1:8787": true,
		"localhost:8787": true,
		"[::1]:8787":     true,
		"evil.example":   false,
		"10.0.0.1:80":    false,
	} {
		if got := isLoopbackHost(host); got != want {
			t.Errorf("isLoopbackHost(%q) = %v, want %v", host, got, want)
		}
	}
}

func TestBridgeReplacesOldApp(t *testing.T) {
	bridge, srv := newTestBridge(t, BridgeConfig{Token: "secret", EnforceLoopbackHost: true})
	first, resp := dialApp(t, srv, "secret", "")
	if first == nil {
		t.Fatalf("dial first: %v", resp)
	}
	defer first.Close()
	waitConnected(t, bridge)

	second, resp := dialApp(t, srv, "secret", "")
	if second == nil {
		t.Fatalf("dial second: %v", resp)
	}
	defer second.Close()

	// The replaced tab must receive the custom close code (and the preceding
	// takeover frame) so the Dart client can stop reconnecting instead of
	// stealing the bridge back.
	_ = first.SetReadDeadline(time.Now().Add(2 * time.Second))
	sawReplaced := false
	var closeErr *websocket.CloseError
	for {
		_, data, err := first.ReadMessage()
		if err != nil {
			if !errors.As(err, &closeErr) {
				t.Fatalf("first tab read = %v, want a close error", err)
			}
			break
		}
		var frame bridgeFrame
		if json.Unmarshal(data, &frame) == nil && frame.Type == "replaced" {
			sawReplaced = true
		}
	}
	if !sawReplaced {
		t.Fatal("replaced tab did not receive the takeover frame")
	}
	if closeErr.Code != CloseReplaced {
		t.Fatalf("close code = %d, want %d", closeErr.Code, CloseReplaced)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		connected, since, _ := bridge.Status()
		if connected && time.Since(since) < time.Second {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("second app did not replace the first")
}

func TestBridgeHelloRecordsAppInfo(t *testing.T) {
	bridge, srv := newTestBridge(t, BridgeConfig{Token: "secret", EnforceLoopbackHost: true})
	conn, resp := dialApp(t, srv, "secret", "")
	if conn == nil {
		t.Fatalf("dial: %v", resp)
	}
	defer conn.Close()
	waitConnected(t, bridge)

	fingerprint := CatalogFingerprint()
	if err := conn.WriteJSON(bridgeFrame{
		Type:              "hello",
		Version:           1,
		BridgeProtocol:    SupportedBridgeProtocol,
		App:               "err",
		AppVersion:        "1.2.3",
		SchemaFingerprint: fingerprint,
	}); err != nil {
		t.Fatalf("hello: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		app, version, protocol, gotFingerprint := bridge.AppInfo()
		if app == "err" && version == "1.2.3" &&
			protocol == SupportedBridgeProtocol && gotFingerprint == fingerprint {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("hello did not record the app version, protocol and schema")
}

func TestBridgeAcceptsLegacyHello(t *testing.T) {
	bridge, srv := newTestBridge(t, BridgeConfig{Token: "secret", EnforceLoopbackHost: true})
	conn, resp := dialApp(t, srv, "secret", "")
	if conn == nil {
		t.Fatalf("dial: %v", resp)
	}
	defer conn.Close()
	waitConnected(t, bridge)

	// The pre-T-8 app sends only `version: 1`.
	if err := conn.WriteJSON(bridgeFrame{Type: "hello", Version: 1, App: "err", AppVersion: "0.9.0"}); err != nil {
		t.Fatalf("hello: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		app, version, protocol, _ := bridge.AppInfo()
		if app == "err" && version == "0.9.0" && protocol == 1 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("legacy hello was not accepted as protocol 1")
}

func TestBridgeRejectsUnsupportedProtocol(t *testing.T) {
	bridge, srv := newTestBridge(t, BridgeConfig{Token: "secret", EnforceLoopbackHost: true})
	conn, resp := dialApp(t, srv, "secret", "")
	if conn == nil {
		t.Fatalf("dial: %v", resp)
	}
	defer conn.Close()
	waitConnected(t, bridge)

	if err := conn.WriteJSON(bridgeFrame{
		Type:           "hello",
		BridgeProtocol: SupportedBridgeProtocol + 1,
		App:            "err",
	}); err != nil {
		t.Fatalf("hello: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, _, err := conn.ReadMessage()
	var closeErr *websocket.CloseError
	if !errors.As(err, &closeErr) {
		t.Fatalf("read after mismatch = %v, want a close error", err)
	}
	if closeErr.Code != CloseProtocolMismatch {
		t.Fatalf("close code = %d, want %d", closeErr.Code, CloseProtocolMismatch)
	}
	if !strings.Contains(closeErr.Text, "bridge protocol") {
		t.Fatalf("close reason = %q", closeErr.Text)
	}
}

func TestCatalogFingerprintMatchesDartSchema(t *testing.T) {
	// The Dart client computes the same value from domain.pbjson.dart. Keep
	// both expected hashes in sync when domain.proto changes.
	const want = "9fc813930b8a8812"
	if got := CatalogFingerprint(); got != want {
		t.Fatalf("CatalogFingerprint() = %q, want %q", got, want)
	}
}

func TestBridgePreflightExposesHandshakeStatus(t *testing.T) {
	_, srv := newTestBridge(t, BridgeConfig{Token: "secret", EnforceLoopbackHost: true})

	// A plain GET (no Upgrade header) is how the Dart client distinguishes a
	// rejected token from a rejected origin or a stopped server. The response
	// must be readable cross-origin, hence the CORS header.
	get := func(token string) *http.Response {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, srv.URL+"/app/ws?token="+token, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	bad := get("wrong")
	defer bad.Body.Close()
	if bad.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong token status = %d, want 401", bad.StatusCode)
	}
	if got := bad.Header.Get("Access-Control-Allow-Origin"); got != "*" {
		t.Fatalf("CORS header = %q, want *", got)
	}

	good := get("secret")
	defer good.Body.Close()
	if good.StatusCode != http.StatusBadRequest {
		t.Fatalf("good token without Upgrade status = %d, want 400", good.StatusCode)
	}
}
