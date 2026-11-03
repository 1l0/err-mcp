//go:build !js

package mcp

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	// defaultWriteWait bounds one WebSocket write. Segment and snapshot
	// responses can be large, so it is generous but finite.
	defaultWriteWait = 15 * time.Second
	// defaultPongWait is how long the server waits for any frame (a pong
	// counts) before considering the app dead.
	defaultPongWait = 60 * time.Second
	// defaultPingPeriod must be shorter than pongWait.
	defaultPingPeriod = 25 * time.Second
	// defaultMaxMessage matches the largest app response we expect (a compacted
	// snapshot or an exported segment). The app limits segments itself.
	defaultMaxMessage  = 64 << 20
	defaultCallTimeout = 60 * time.Second
)

const (
	// CloseReplaced is the WebSocket close code sent to an app tab when a newer
	// tab takes over the bridge. The Dart client stops reconnecting on this code
	// so two tabs cannot steal the connection back and forth.
	CloseReplaced = 4001
	// CloseProtocolMismatch is sent when the app announces a bridge protocol
	// this server does not support. The Dart client stops retrying and tells the
	// user to update/restart one side.
	CloseProtocolMismatch = 4002
	// SupportedBridgeProtocol is the current app/server frame protocol. Bump it
	// when a frame shape changes incompatibly.
	SupportedBridgeProtocol = 1
)

// DefaultAllowedOrigins are browser origins that err-mcp trusts without any
// configuration. The hosted err web app is served from err.tokyo, and
// http://localhost[:port] is trusted for local development.
var DefaultAllowedOrigins = []string{"https://err.tokyo"}

// BridgeConfig configures the loopback app bridge.
type BridgeConfig struct {
	// Token, when non-empty, is required on the handshake (query parameter
	// "token" or "Authorization: Bearer"). Keep it non-empty on real installs:
	// it stops an arbitrary local page from driving the app.
	Token string
	// AllowedOrigins restricts browser handshakes. Empty means "any origin";
	// the token is then the only gate. DefaultAllowedOrigins are always allowed
	// in addition to this list.
	AllowedOrigins []string
	// PairFile, when non-empty, enables GET/POST /pair. The first app origin
	// (or an origin in AllowedOrigins) may fetch the token; the origin is
	// persisted at PairFile. Empty disables automatic pairing.
	PairFile string
	// EnforceLoopbackHost rejects a non-loopback Host header. It is on when the
	// server is bound to loopback, and is the DNS-rebinding guard for /app/ws.
	EnforceLoopbackHost bool
	Logger              *slog.Logger
}

// Bridge is the WebSocket hub for the err app and implements Backend.
//
// The app connects as a WebSocket client. Requests and responses are JSON
// frames with a base64 protobuf payload; each request has an id so several
// calls can be in flight while the app's engine serializes them.
type Bridge struct {
	token               string
	origins             map[string]struct{}
	trusted             map[string]struct{}
	pairFile            string
	pairedOrigin        string
	enforceLoopbackHost bool
	logger              *slog.Logger

	upgrader   websocket.Upgrader
	writeWait  time.Duration
	pongWait   time.Duration
	pingPeriod time.Duration
	maxMessage int64

	mu             sync.Mutex
	conn           *websocket.Conn
	connectedSince time.Time
	appName        string
	appVersion     string
	appProtocol    int
	appFingerprint string
	nextID         int64
	pending        map[int64]chan bridgeReply

	// writeMu serializes WriteMessage and WriteControl; gorilla allows only
	// one concurrent writer.
	writeMu sync.Mutex
}

type bridgeReply struct {
	payload []byte
	rpcErr  *RPCError
	cause   error
}

// bridgeFrame is the wire format for /app/ws. Payload is base64 in JSON
// (encoding/json handles []byte that way automatically).
type bridgeFrame struct {
	Type    string       `json:"type"`
	ID      int64        `json:"id,omitempty"`
	Method  string       `json:"method,omitempty"`
	Payload []byte       `json:"payload,omitempty"`
	Error   *bridgeError `json:"error,omitempty"`

	// hello
	Version           int    `json:"version,omitempty"` // legacy protocol field
	BridgeProtocol    int    `json:"bridgeProtocol,omitempty"`
	App               string `json:"app,omitempty"`
	AppVersion        string `json:"appVersion,omitempty"`
	SchemaFingerprint string `json:"schemaFingerprint,omitempty"`
}

type bridgeError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// normalizeOriginSet turns configured origins into normalized keys. Entries
// with a trailing slash still match the browser's serialized Origin; invalid
// entries are logged and skipped.
func normalizeOriginSet(list []string, logger *slog.Logger) map[string]struct{} {
	set := make(map[string]struct{}, len(list))
	for _, origin := range list {
		origin = strings.TrimSpace(origin)
		if origin == "" {
			continue
		}
		normalized, err := normalizeOrigin(origin)
		if err != nil {
			logger.Warn("ignoring invalid allowed origin", "origin", origin, "error", err)
			continue
		}
		set[normalized] = struct{}{}
	}
	return set
}

// isTrustedOrigin reports whether origin is a built-in origin that needs no
// pairing or allowlist entry. Local development origins (http://localhost with
// any port) are trusted alongside DefaultAllowedOrigins.
func (b *Bridge) isTrustedOrigin(origin string) bool {
	normalized, err := normalizeOrigin(origin)
	if err != nil {
		return false
	}
	if isLocalhostOrigin(normalized) {
		return true
	}
	_, ok := b.trusted[normalized]
	return ok
}

// isAllowedOrigin reports whether origin is in the configured allowlist.
func (b *Bridge) isAllowedOrigin(origin string) bool {
	normalized, err := normalizeOrigin(origin)
	if err != nil {
		return false
	}
	_, ok := b.origins[normalized]
	return ok
}

// NewBridge returns an empty bridge. No app is connected until ServeHTTP
// upgrades a handshake.
func NewBridge(cfg BridgeConfig) *Bridge {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	origins := normalizeOriginSet(cfg.AllowedOrigins, logger)
	trusted := normalizeOriginSet(DefaultAllowedOrigins, logger)
	b := &Bridge{
		token:               cfg.Token,
		origins:             origins,
		trusted:             trusted,
		pairFile:            cfg.PairFile,
		enforceLoopbackHost: cfg.EnforceLoopbackHost,
		logger:              logger,
		upgrader: websocket.Upgrader{
			ReadBufferSize:  32 << 10,
			WriteBufferSize: 32 << 10,
			// Authorization is done in authorize: the app is served from a
			// different origin than the loopback server, so the default
			// same-origin check would always fail.
			CheckOrigin: func(*http.Request) bool { return true },
		},
		writeWait:  defaultWriteWait,
		pongWait:   defaultPongWait,
		pingPeriod: defaultPingPeriod,
		maxMessage: defaultMaxMessage,
		pending:    make(map[int64]chan bridgeReply),
	}
	if cfg.PairFile != "" {
		if origin, err := ReadPairedOrigin(cfg.PairFile); err == nil {
			b.pairedOrigin = origin
		} else if !errors.Is(err, os.ErrNotExist) {
			logger.Warn("cannot read paired app origin", "file", cfg.PairFile, "error", err)
		}
	}
	return b
}

// ServeHTTP upgrades an app connection. It is mounted at /app/ws.
func (b *Bridge) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// The Dart client classifies a failed handshake with a plain cross-origin
	// GET on this path before opening the WebSocket. The response carries no
	// secrets (401/403/400), so it only needs to be readable from the app
	// origin. Without this header a browser sees an opaque network error and
	// cannot distinguish "token mismatch" from "server is down".
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Authorization")
	if r.Header.Get("Access-Control-Request-Private-Network") == "true" {
		w.Header().Set("Access-Control-Allow-Private-Network", "true")
	}
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if status, err := b.authorize(r); err != nil {
		http.Error(w, err.Error(), status)
		return
	}
	conn, err := b.upgrader.Upgrade(w, r, nil)
	if err != nil {
		// Upgrade already wrote an HTTP error. A plain GET (no Upgrade header)
		// lands here with 400 once the token and origin are accepted, which is
		// what the Dart preflight uses as "ready".
		b.logger.Debug("app bridge upgrade failed", "error", err)
		return
	}
	b.attach(conn)
}

func (b *Bridge) authorize(r *http.Request) (int, error) {
	if b.enforceLoopbackHost && !isLoopbackHost(r.Host) {
		return http.StatusForbidden, fmt.Errorf("invalid Host header %q", r.Host)
	}
	if origin := r.Header.Get("Origin"); origin != "" && len(b.origins) > 0 {
		if !b.isTrustedOrigin(origin) && !b.isAllowedOrigin(origin) {
			return http.StatusForbidden, fmt.Errorf("origin %q is not allowed", origin)
		}
	}
	if b.token != "" {
		got := r.URL.Query().Get("token")
		if got == "" {
			got = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		}
		if subtle.ConstantTimeCompare([]byte(got), []byte(b.token)) != 1 {
			return http.StatusUnauthorized, errors.New("invalid or missing bridge token")
		}
	}
	return 0, nil
}

// isLoopbackHost reports whether hostport names loopback (localhost, 127.0.0.0/8,
// ::1). DNS-rebinding requests carry the attacker's hostname here.
func isLoopbackHost(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (b *Bridge) attach(conn *websocket.Conn) {
	b.mu.Lock()
	old := b.conn
	oldApp, oldVersion := b.appName, b.appVersion
	b.conn = conn
	b.connectedSince = time.Now()
	b.appName, b.appVersion = "", ""
	b.appProtocol, b.appFingerprint = 0, ""
	if old != nil {
		// A second tab replaces the first. The old tab's in-flight calls can
		// never be answered, so fail them instead of waiting for the timeout.
		b.failPendingLocked(ErrAppNotConnected)
	}
	b.mu.Unlock()

	if old != nil {
		// Tell the replaced tab why it lost the bridge. The Dart client stops
		// reconnecting on this code instead of stealing the connection back.
		// A separate data frame precedes the close frame because some browsers
		// report an ambiguous 1006 when a connection is torn down immediately
		// after a close frame; the ordered data frame makes the takeover
		// unambiguous.
		deadline := time.Now().Add(b.writeWait)
		b.writeMu.Lock()
		_ = old.SetWriteDeadline(deadline)
		_ = old.WriteMessage(websocket.TextMessage, []byte(`{"type":"replaced"}`))
		_ = old.WriteControl(
			websocket.CloseMessage,
			websocket.FormatCloseMessage(CloseReplaced, "another tab is controlling MCP"),
			deadline,
		)
		b.writeMu.Unlock()
		_ = old.Close()
		b.logger.Info("new app tab replaced the previous MCP bridge connection",
			"previous_app", oldApp, "previous_app_version", oldVersion)
	}
	b.logger.Info("err app connected to MCP bridge")

	go b.readLoop(conn)
	go b.pingLoop(conn)
}

func (b *Bridge) readLoop(conn *websocket.Conn) {
	reason := "connection closed"
	code := 0
	defer func() { b.detach(conn, reason, code) }()

	conn.SetReadLimit(b.maxMessage)
	_ = conn.SetReadDeadline(time.Now().Add(b.pongWait))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(b.pongWait))
	})

	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			var closeErr *websocket.CloseError
			if errors.As(err, &closeErr) {
				code = closeErr.Code
				reason = closeErr.Text
				if reason == "" {
					reason = closeErr.Error()
				}
			} else {
				reason = err.Error()
			}
			if !websocket.IsCloseError(err,
				websocket.CloseNormalClosure,
				websocket.CloseGoingAway,
				CloseReplaced,
			) {
				b.logger.Debug("app bridge read ended", "error", err)
			}
			return
		}
		var frame bridgeFrame
		if err := json.Unmarshal(data, &frame); err != nil {
			b.logger.Warn("app bridge bad frame", "error", err)
			continue
		}
		switch frame.Type {
		case "hello":
			// The old app sent only `version`; treat it as the protocol for
			// backward compatibility. A new app sends bridgeProtocol and also
			// keeps `version` at 1.
			protocol := frame.BridgeProtocol
			legacy := protocol == 0
			if legacy {
				protocol = frame.Version
			}
			if protocol > SupportedBridgeProtocol {
				b.logger.Warn("app bridge protocol is unsupported",
					"app_protocol", protocol, "server_protocol", SupportedBridgeProtocol)
				b.closeProtocolMismatch(conn, protocol)
				return
			}
			b.mu.Lock()
			b.appName = frame.App
			b.appVersion = frame.AppVersion
			b.appProtocol = protocol
			b.appFingerprint = frame.SchemaFingerprint
			b.mu.Unlock()
			serverFingerprint := CatalogFingerprint()
			b.logger.Info("err app bridge hello",
				"app", frame.App, "app_version", frame.AppVersion,
				"app_protocol", protocol, "server_protocol", SupportedBridgeProtocol,
				"legacy_protocol", legacy,
				"app_schema", frame.SchemaFingerprint, "server_schema", serverFingerprint)
			if legacy {
				b.logger.Warn("app uses the legacy bridge protocol; update the app",
					"app_version", frame.AppVersion)
			}
			if frame.SchemaFingerprint != "" && frame.SchemaFingerprint != serverFingerprint {
				b.logger.Warn("app and server RPC schemas differ; update err-mcp or the app",
					"app_schema", frame.SchemaFingerprint, "server_schema", serverFingerprint)
			}
		case "response":
			b.deliver(frame)
		default:
			b.logger.Warn("app bridge unknown frame type", "type", frame.Type)
		}
	}
}

// closeProtocolMismatch sends a clear close frame so the browser can stop
// retrying and explain that one side needs an update.
func (b *Bridge) closeProtocolMismatch(conn *websocket.Conn, appProtocol int) {
	deadline := time.Now().Add(b.writeWait)
	b.writeMu.Lock()
	_ = conn.WriteControl(
		websocket.CloseMessage,
		websocket.FormatCloseMessage(CloseProtocolMismatch,
			fmt.Sprintf("unsupported bridge protocol %d; update err-mcp or the app", appProtocol)),
		deadline,
	)
	b.writeMu.Unlock()
	_ = conn.Close()
}

func (b *Bridge) deliver(frame bridgeFrame) {
	b.mu.Lock()
	ch := b.pending[frame.ID]
	delete(b.pending, frame.ID)
	b.mu.Unlock()
	if ch == nil {
		// Timed out or cancelled; the app's answer is no longer interesting.
		return
	}
	if frame.Error != nil {
		ch <- bridgeReply{rpcErr: &RPCError{Code: frame.Error.Code, Message: frame.Error.Message}}
		return
	}
	ch <- bridgeReply{payload: frame.Payload}
}

// detach clears the connection if it is still the current one. A replaced
// connection must not clear the new one's state.
func (b *Bridge) detach(conn *websocket.Conn, reason string, code int) {
	var app, version string
	var protocol int
	b.mu.Lock()
	current := b.conn == conn
	if current {
		app, version, protocol = b.appName, b.appVersion, b.appProtocol
		b.conn = nil
		b.connectedSince = time.Time{}
		b.appName, b.appVersion = "", ""
		b.appProtocol, b.appFingerprint = 0, ""
		b.failPendingLocked(ErrAppNotConnected)
	}
	b.mu.Unlock()

	_ = conn.Close()
	if current {
		b.logger.Info("err app disconnected from MCP bridge",
			"app", app, "app_version", version, "app_protocol", protocol,
			"reason", reason, "close_code", code)
	}
}

func (b *Bridge) failPendingLocked(err error) {
	for id, ch := range b.pending {
		delete(b.pending, id)
		select {
		case ch <- bridgeReply{cause: err}:
		default:
		}
	}
}

func (b *Bridge) pingLoop(conn *websocket.Conn) {
	ticker := time.NewTicker(b.pingPeriod)
	defer ticker.Stop()
	for range ticker.C {
		b.writeMu.Lock()
		err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(b.writeWait))
		b.writeMu.Unlock()
		if err != nil {
			return
		}
	}
}

// Call implements Backend: it forwards one request to the app and waits for
// the matching response.
func (b *Bridge) Call(ctx context.Context, method string, req []byte) ([]byte, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, defaultCallTimeout)
		defer cancel()
	}

	b.mu.Lock()
	conn := b.conn
	if conn == nil {
		b.mu.Unlock()
		return nil, ErrAppNotConnected
	}
	b.nextID++
	id := b.nextID
	ch := make(chan bridgeReply, 1)
	b.pending[id] = ch
	b.mu.Unlock()

	data, err := json.Marshal(bridgeFrame{Type: "request", ID: id, Method: method, Payload: req})
	if err != nil {
		b.dropPending(id)
		return nil, fmt.Errorf("encode bridge request: %w", err)
	}
	b.writeMu.Lock()
	_ = conn.SetWriteDeadline(time.Now().Add(b.writeWait))
	err = conn.WriteMessage(websocket.TextMessage, data)
	b.writeMu.Unlock()
	if err != nil {
		b.dropPending(id)
		return nil, fmt.Errorf("send to app: %w", err)
	}

	select {
	case <-ctx.Done():
		b.dropPending(id)
		return nil, ctx.Err()
	case reply := <-ch:
		if reply.cause != nil {
			return nil, reply.cause
		}
		if reply.rpcErr != nil {
			return nil, reply.rpcErr
		}
		return reply.payload, nil
	}
}

func (b *Bridge) dropPending(id int64) {
	b.mu.Lock()
	delete(b.pending, id)
	b.mu.Unlock()
}

// Status reports whether an app is connected, since when, and how many calls
// are waiting for it.
func (b *Bridge) Status() (connected bool, since time.Time, pending int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.conn != nil, b.connectedSince, len(b.pending)
}

// AppInfo reports the details the most recent hello frame announced. They are
// empty/zero until the app sends hello or after it disconnects.
func (b *Bridge) AppInfo() (name, version string, protocol int, fingerprint string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.appName, b.appVersion, b.appProtocol, b.appFingerprint
}

// Close disconnects the app. It is used on server shutdown.
func (b *Bridge) Close() {
	b.mu.Lock()
	conn := b.conn
	b.conn = nil
	b.connectedSince = time.Time{}
	b.appName, b.appVersion = "", ""
	b.appProtocol, b.appFingerprint = 0, ""
	b.failPendingLocked(ErrAppNotConnected)
	b.mu.Unlock()
	if conn != nil {
		_ = conn.Close()
	}
}
