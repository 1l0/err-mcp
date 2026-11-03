//go:build !js

package mcp

import (
	"encoding/json"
	"net"
	"net/http"
	"strings"
)

// ServePair handles GET/POST /pair. It lets the browser app fetch the bridge
// token without the user copying it, but only from a loopback connection and
// from the origin that is allowed to pair:
//
//   - the built-in DefaultAllowedOrigins are always allowed and need no pairing;
//   - when AllowedOrigins is configured, the Origin must be in that list;
//   - otherwise the first Origin that pairs is persisted (trust on first use)
//     and all later requests must use the same origin.
//
// A custom X-Err-Pair header makes the request CORS-preflighted, so a link or
// image load cannot silently read the token. The token is still required on
// /app/ws; this endpoint only automates the handoff.
func (b *Bridge) ServePair(w http.ResponseWriter, r *http.Request) {
	if b.enforceLoopbackHost && !isLoopbackHost(r.Host) {
		http.Error(w, "invalid Host header", http.StatusForbidden)
		return
	}
	if !isLoopbackRemote(r.RemoteAddr) {
		http.Error(w, "pairing is only available from loopback", http.StatusForbidden)
		return
	}
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		http.Error(w, "an Origin header is required", http.StatusForbidden)
		return
	}
	normalized, err := normalizeOrigin(origin)
	if err != nil {
		http.Error(w, "invalid Origin header", http.StatusForbidden)
		return
	}
	origin = normalized

	b.mu.Lock()
	paired := b.pairedOrigin
	b.mu.Unlock()

	// Reject a wrong origin before the CORS preflight so an unpaired page
	// cannot probe the endpoint. Built-in trusted origins always pass and do
	// not need pairing.
	if !b.isTrustedOrigin(origin) {
		if len(b.origins) > 0 {
			if _, ok := b.origins[origin]; !ok {
				http.Error(w, "origin is not allowed", http.StatusForbidden)
				return
			}
		} else if paired != "" && paired != origin {
			http.Error(w, "origin is not paired", http.StatusForbidden)
			return
		}
	}

	w.Header().Set("Access-Control-Allow-Origin", origin)
	w.Header().Set("Vary", "Origin")
	w.Header().Set("Cache-Control", "no-store")

	if r.Method == http.MethodOptions {
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "X-Err-Pair")
		w.Header().Set("Access-Control-Max-Age", "600")
		// Chromium's Local Network Access preflight for a public page that
		// reaches loopback. The browser still asks the user for permission.
		if r.Header.Get("Access-Control-Request-Private-Network") == "true" {
			w.Header().Set("Access-Control-Allow-Private-Network", "true")
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, POST, OPTIONS")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if r.Header.Get("X-Err-Pair") != "1" {
		http.Error(w, "missing X-Err-Pair header", http.StatusForbidden)
		return
	}

	// Re-check under the lock because the first pairing may have happened
	// between the preflight check and this request.
	b.mu.Lock()
	paired = b.pairedOrigin
	switch {
	case b.isTrustedOrigin(origin):
		// Built-in origins need no pairing state and must not overwrite it.
	case len(b.origins) == 0 && paired == "":
		if b.pairFile == "" {
			b.mu.Unlock()
			http.Error(w, "automatic pairing is disabled", http.StatusForbidden)
			return
		}
		if err := WritePairedOrigin(b.pairFile, origin); err != nil {
			b.mu.Unlock()
			b.logger.Error("cannot persist paired origin", "file", b.pairFile, "error", err)
			http.Error(w, "cannot persist pairing", http.StatusInternalServerError)
			return
		}
		b.pairedOrigin = origin
		b.logger.Warn("paired app origin",
			"origin", origin,
			"file", b.pairFile,
			"note", "use --reset-pairing to pair a different origin")
	case len(b.origins) == 0 && paired != origin:
		b.mu.Unlock()
		http.Error(w, "origin is not paired", http.StatusForbidden)
		return
	}
	b.mu.Unlock()

	if b.token == "" {
		http.Error(w, "authentication is disabled", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"token": b.token})
}

// isLoopbackRemote reports whether the TCP peer is on this machine. Automatic
// pairing must never be reachable from another host, even when the operator
// binds the app bridge to a non-loopback address.
func isLoopbackRemote(remoteAddr string) bool {
	host := remoteAddr
	if h, _, err := net.SplitHostPort(remoteAddr); err == nil {
		host = h
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}
