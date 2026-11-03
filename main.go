// Command err-mcp is the local MCP server for the err web app.
//
// The server holds no vault data. The app owns the vault and connects to
// /app/ws; tool calls are forwarded to it over a loopback WebSocket. MCP
// clients talk to this process over stdin/stdout.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/1l0/err-mcp/internal/mcp"
)

var version = "dev"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "err-mcp:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("err-mcp", flag.ContinueOnError)
	port := fs.Int("port", envInt("MCP_PORT", 8787), "app-bridge HTTP port (env MCP_PORT)")
	host := fs.String("host", envString("MCP_HOST", "127.0.0.1"), "bind address; keep loopback unless you understand the risk")
	tokenFile := fs.String("token-file", envString("MCP_TOKEN_FILE", ""), "token file path (env MCP_TOKEN_FILE); default <user config>/err/mcp-token")
	originFile := fs.String("origin-file", envString("MCP_ORIGIN_FILE", ""), "paired app-origin file (env MCP_ORIGIN_FILE); default <user config>/err/mcp-origin")
	rotateToken := fs.Bool("rotate-token", false, "generate a new app-bridge token, save it, print it once and exit")
	showToken := fs.Bool("show-token", false, "print the app-bridge token and exit (explicit opt-in)")
	resetPairing := fs.Bool("reset-pairing", false, "forget the paired app origin and exit")
	origins := fs.String("origins", os.Getenv("MCP_ALLOWED_ORIGINS"), "comma-separated allowed browser origins for /app/ws; https://err.tokyo and http://localhost[:port] are always allowed (env MCP_ALLOWED_ORIGINS)")
	toolset := fs.String("toolset", envString("MCP_TOOLSET", "safe"), `advertised tool tiers: "safe" (default) or "all"`)
	allowSync := fs.Bool("allow-sync", false, "also expose non-destructive sync tools (segment export, snapshot)")
	allowDestructive := fs.Bool("allow-destructive", false, "also expose destructive sync tools (import_segment, adopt_vault_key, run_gc); requires --allow-sync")
	allowSecrets := fs.Bool("allow-secrets", false, "also expose get_vault_key (sensitive)")
	auditLog := fs.String("audit-log", os.Getenv("MCP_AUDIT_LOG"), "optional JSONL file for one audit line per tool call (env MCP_AUDIT_LOG); stderr is always used")
	rateLimit := fs.Int("rate-limit", envInt("MCP_RATE_LIMIT", 0), "per-tool call limit per minute; 0 disables (env MCP_RATE_LIMIT)")
	staticDir := fs.String("static", os.Getenv("MCP_STATIC_DIR"), "optional directory of a built web app to serve (env MCP_STATIC_DIR)")
	showVersion := fs.Bool("version", false, "print the version and exit")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *showVersion {
		fmt.Println(version)
		return nil
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	if *port < 0 || *port > 65535 {
		return fmt.Errorf("invalid port %d", *port)
	}
	if *toolset != "safe" && *toolset != "all" {
		return fmt.Errorf(`invalid --toolset %q: use "safe" or "all"`, *toolset)
	}
	if *rateLimit < 0 {
		return fmt.Errorf("invalid --rate-limit %d: must not be negative", *rateLimit)
	}
	if *rotateToken && *showToken {
		return fmt.Errorf("--rotate-token and --show-token are mutually exclusive")
	}
	if *resetPairing && (*rotateToken || *showToken) {
		return fmt.Errorf("--reset-pairing cannot be combined with --rotate-token or --show-token")
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	audit, err := mcp.NewAuditLog(mcp.AuditConfig{Path: *auditLog, Logger: logger})
	if err != nil {
		return err
	}
	defer audit.Close()
	if *auditLog != "" {
		logger.Info("audit log enabled", "file", *auditLog)
	}
	if *allowDestructive && !*allowSync && *toolset != "all" {
		logger.Warn("--allow-destructive has no effect without --allow-sync")
	}

	enforceLoopbackHost := isLoopback(*host)
	if !enforceLoopbackHost {
		logger.Warn("binding a non-loopback address exposes the app bridge and the vault surface; use a token and an origin allowlist",
			"host", *host)
	}

	tokenPath := *tokenFile
	if tokenPath == "" {
		if p, err := mcp.DefaultTokenPath(); err == nil {
			tokenPath = p
		} else {
			logger.Warn("cannot locate a per-user config directory; a generated token will not persist", "error", err)
		}
	}
	originPath := *originFile
	if originPath == "" && tokenPath != "" {
		originPath = filepath.Join(filepath.Dir(tokenPath), mcp.OriginFileName)
	}
	if originPath == "" {
		if p, err := mcp.DefaultOriginPath(); err == nil {
			originPath = p
		}
	}
	if *resetPairing {
		if originPath == "" {
			return fmt.Errorf("no paired-origin path is available")
		}
		if err := mcp.RemovePairedOrigin(originPath); err != nil {
			return err
		}
		fmt.Printf("removed paired app origin: %s\n", originPath)
		return nil
	}

	var tok tokenState
	switch {
	case *rotateToken:
		if tokenPath == "" {
			return fmt.Errorf("cannot rotate the token: no token file path is available")
		}
		rotated, err := mcp.RotateToken(tokenPath)
		if err != nil {
			return err
		}
		fmt.Printf(`rotated app-bridge token
  token  %s
  file   %s

Restart err-mcp and re-open the web app with the new #mcpToken=... URL.
`, rotated, tokenPath)
		return nil
	case *showToken:
		if tokenPath == "" {
			return fmt.Errorf("no token file path is available")
		}
		effective, err := mcp.LoadToken(tokenPath)
		if err != nil {
			return fmt.Errorf("%w (start err-mcp once to create it)", err)
		}
		fmt.Println(effective)
		return nil
	case tokenPath != "":
		effective, created, err := mcp.LoadOrCreateToken(tokenPath)
		if err != nil {
			return err
		}
		tok = tokenState{value: effective, file: tokenPath, created: created}
	default:
		effective, err := mcp.GenerateToken()
		if err != nil {
			return err
		}
		tok = tokenState{value: effective, created: true}
	}
	bridgeToken := tok.value

	originList := splitList(*origins)

	addr := net.JoinHostPort(*host, strconv.Itoa(*port))
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", addr, err)
	}
	actualPort := ln.Addr().(*net.TCPAddr).Port
	base := fmt.Sprintf("%s:%d", displayHost(*host), actualPort)

	bridge := mcp.NewBridge(mcp.BridgeConfig{
		Token:               bridgeToken,
		AllowedOrigins:      originList,
		PairFile:            originPath,
		EnforceLoopbackHost: enforceLoopbackHost,
		Logger:              logger,
	})
	defer bridge.Close()
	if originPath != "" {
		logger.Info("automatic app pairing enabled", "origin_file", originPath)
	} else {
		logger.Warn("automatic app pairing disabled; open the web app with the token URL")
	}

	server := mcp.NewServer(mcp.ServerConfig{
		Backend:          bridge,
		Bridge:           bridge,
		Version:          version,
		Port:             actualPort,
		Toolset:          *toolset,
		AllowSync:        *allowSync,
		AllowDestructive: *allowDestructive,
		AllowSecrets:     *allowSecrets,
		Audit:            audit,
		RateLimit:        *rateLimit,
		Logger:           logger,
	})

	mux := http.NewServeMux()
	mux.HandleFunc("/pair", bridge.ServePair)
	mux.Handle("/app/ws", bridge)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		// Unauthenticated on purpose: the app uses it to tell "err-mcp is not
		// running" from "the handshake was rejected". It exposes no vault data.
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
		if r.Header.Get("Access-Control-Request-Private-Network") == "true" {
			w.Header().Set("Access-Control-Allow-Private-Network", "true")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		connected, since, pending := bridge.Status()
		body := map[string]any{
			"ok":                  true,
			"transport":           "stdio",
			"app_connected":       connected,
			"pending_calls":       pending,
			"bridge_port":         actualPort,
			"toolset":             server.Toolset(),
			"tools":               len(server.ToolNames()),
			"version":             version,
			"bridge_protocol":     mcp.SupportedBridgeProtocol,
			"catalog_fingerprint": mcp.CatalogFingerprint(),
		}
		if app, appVersion, appProtocol, appSchema := bridge.AppInfo(); app != "" {
			body["app"] = app
			body["app_version"] = appVersion
			body["app_protocol"] = appProtocol
			body["app_schema"] = appSchema
		}
		if connected {
			body["connected_since"] = since.UTC().Format(time.RFC3339)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	})
	if *staticDir != "" {
		static, err := staticHandler(*staticDir)
		if err != nil {
			return err
		}
		mux.Handle("/", static)
	}

	httpSrv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}

	printBanner(os.Stderr, logger, base, tok, originPath != "", *staticDir != "")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		errCh <- httpSrv.Serve(ln)
	}()

	stdioErrCh := make(chan error, 1)
	go func() {
		stdioErrCh <- server.RunStdio(ctx)
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdownCtx)
		return nil
	case err := <-errCh:
		if err == http.ErrServerClosed {
			return nil
		}
		return err
	case err := <-stdioErrCh:
		// The MCP client closed stdin. Its process lifetime owns ours, so stop
		// the app bridge as well.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdownCtx)
		if err != nil && !errors.Is(err, context.Canceled) {
			return fmt.Errorf("stdio MCP: %w", err)
		}
		return nil
	}
}

// tokenState is the app-bridge token and where it came from. The secret is
// printed only when it was just generated, so repeated starts stay safe to
// paste into logs.
type tokenState struct {
	value   string
	file    string
	created bool
}

func (t tokenState) summary() string {
	if t.file != "" {
		return mcp.TokenFingerprint(t.value) + " (stored in " + t.file + ")"
	}
	return mcp.TokenFingerprint(t.value)
}

func printBanner(w io.Writer, logger *slog.Logger, base string, tok tokenState, pairing, static bool) {
	logger.Info("err MCP server listening",
		"mcp", "stdio (stdin/stdout)",
		"app_bridge", "ws://"+base+"/app/ws",
		"health", "http://"+base+"/healthz",
		"static", static,
		"pairing", pairing,
	)
	if tok.created {
		logger.Info("generated and saved a new app-bridge token",
			"fingerprint", mcp.TokenFingerprint(tok.value), "file", tok.file)
	} else {
		logger.Info("using the app-bridge token", "fingerprint", mcp.TokenFingerprint(tok.value), "source", tok.file)
	}

	setup := "open the web app, then Settings at the bottom of the tree pane;\n                 enable Local MCP and use port " + portOf(base)
	tokenSummary := tok.summary()
	if !pairing {
		// Without the pairing endpoint the only way in is the token URL.
		setup = "open with #mcpPort=" + portOf(base) + "&mcpToken=" + tok.value
		tokenSummary = tok.value
	}
	fmt.Fprintf(w, `
err-mcp %s
  MCP transport  stdio (stdin/stdout)
  App bridge     ws://%s/app/ws
  Token          %s
  App setup      %s
`, version, base, tokenSummary, setup)
}

func portOf(base string) string {
	_, port, err := net.SplitHostPort(base)
	if err != nil {
		return strconv.Itoa(8787)
	}
	return port
}

func displayHost(host string) string {
	if host == "" || host == "0.0.0.0" || host == "::" {
		return "127.0.0.1"
	}
	if strings.Contains(host, ":") {
		return "[" + host + "]"
	}
	return host
}

func isLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

func splitList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

func envString(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	v, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(v) == "" {
		return fallback
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return fallback
	}
	return n
}

// staticHandler serves a built web app with the cross-origin isolation headers
// OPFS/SharedArrayBuffer need, and falls back to index.html for SPA routes.
func staticHandler(dir string) (http.Handler, error) {
	info, err := os.Stat(dir)
	if err != nil {
		return nil, fmt.Errorf("static directory: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("static path %s is not a directory", dir)
	}
	root, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	fs := http.FileServer(http.Dir(root))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cross-Origin-Opener-Policy", "same-origin")
		w.Header().Set("Cross-Origin-Embedder-Policy", "require-corp")
		if strings.HasSuffix(r.URL.Path, ".wasm") {
			w.Header().Set("Content-Type", "application/wasm")
		}
		cleaned := filepath.Clean("/" + r.URL.Path)
		if cleaned == "/" {
			http.ServeFile(w, r, filepath.Join(root, "index.html"))
			return
		}
		target := filepath.Join(root, cleaned)
		if info, err := os.Stat(target); err != nil || info.IsDir() {
			http.ServeFile(w, r, filepath.Join(root, "index.html"))
			return
		}
		fs.ServeHTTP(w, r)
	}), nil
}
