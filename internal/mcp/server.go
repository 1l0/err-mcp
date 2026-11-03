//go:build !js

package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/dynamicpb"
)

// ServerConfig configures the MCP adapter.
type ServerConfig struct {
	Backend Backend
	// Bridge is only used by err_status. It may be nil in tests.
	Bridge           *Bridge
	Version          string
	Port             int
	Toolset          string // "safe" (default) or "all"
	AllowSync        bool
	AllowDestructive bool // required, together with AllowSync, for destructive sync tools
	AllowSecrets     bool
	// Audit receives one entry per tool call. When nil, stderr logging is used.
	Audit *AuditLog
	// RateLimit caps calls per tool per minute. Zero disables the limit.
	RateLimit int
	Logger    *slog.Logger
}

// Server is the MCP adapter over a Backend. The tool list is generated from the
// generated protobuf descriptors, so it always matches the app's RPC surface.
type Server struct {
	backend Backend
	bridge  *Bridge
	sdk     *sdk.Server
	logger  *slog.Logger
	audit   *AuditLog
	limiter *rateLimiter
	version string
	port    int
	toolset string
	names   []string
}

// NewServer builds the MCP server and registers the visible tools.
func NewServer(cfg ServerConfig) *Server {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	version := cfg.Version
	if version == "" {
		version = "dev"
	}
	toolset := cfg.Toolset
	if toolset == "" {
		toolset = "safe"
	}

	audit := cfg.Audit
	if audit == nil {
		audit, _ = NewAuditLog(AuditConfig{Logger: logger})
	}

	s := &Server{
		backend: cfg.Backend,
		bridge:  cfg.Bridge,
		logger:  logger,
		audit:   audit,
		limiter: newRateLimiter(cfg.RateLimit),
		version: version,
		port:    cfg.Port,
	}

	var tiers []string
	includeSync := cfg.AllowSync || toolset == "all"
	includeDestructive := cfg.AllowDestructive || toolset == "all"
	includeSecret := cfg.AllowSecrets || toolset == "all"
	tiers = append(tiers, "safe")
	if includeSync {
		tiers = append(tiers, "sync")
	}
	if includeSync && includeDestructive {
		tiers = append(tiers, "destructive")
	}
	if includeSecret {
		tiers = append(tiers, "secret")
	}
	s.toolset = strings.Join(tiers, "+")

	s.sdk = sdk.NewServer(
		&sdk.Implementation{
			Name:        "err",
			Title:       "err",
			Version:     version,
			Description: "A local-first structured editor: one graph of Items, ordered parent/child edges, links, computed properties and Tree Views.",
		},
		&sdk.ServerOptions{
			Instructions: "These tools operate on the user's err vault through the open err web app. Call err_status first. Item ids are opaque; discover them with list_titles, outline or search_items (small results), and use list_items or get_item only when you need full bodies or property writes. Mutations are whole-Item last-write-wins and appear in the app's undo history.",
			Logger:       logger,
		},
	)

	for _, spec := range buildCatalog(domainFileDescriptor()) {
		if !includeTier(spec.Tier, includeSync, includeDestructive, includeSecret) {
			continue
		}
		s.addTool(spec)
	}
	s.addConvenienceTools()
	s.addStatusTool()

	sort.Strings(s.names)
	logger.Info("MCP tool catalog ready", "tools", len(s.names), "toolset", s.toolset)
	return s
}

func includeTier(tier ToolTier, includeSync, includeDestructive, includeSecret bool) bool {
	switch tier {
	case TierSafe:
		return true
	case TierSync:
		return includeSync
	case TierDestructive:
		// Destructive tools need an explicit second opt-in, so --allow-sync
		// alone cannot import segments, rekey or GC the vault.
		return includeSync && includeDestructive
	case TierSecret:
		return includeSecret
	default:
		return false
	}
}

// beginTool applies the optional per-tool rate limit and starts the audit
// timer. A rejected call is audited immediately; accepted calls are recorded
// by the caller with finishTool.
func (s *Server) beginTool(tool string, args json.RawMessage) (time.Time, error) {
	start := time.Now()
	if err := s.limiter.allow(tool, start); err != nil {
		s.audit.Record(tool, args, 0, err)
		return start, err
	}
	return start, nil
}

// finishTool records the result of a call started with beginTool.
func (s *Server) finishTool(tool string, args json.RawMessage, start time.Time, err error) {
	s.audit.Record(tool, args, time.Since(start), err)
}

func (s *Server) addTool(spec methodSpec) {
	inputSchema := schemaForMessage(spec.Method.Input())
	if len(spec.Required) > 0 {
		inputSchema["required"] = spec.Required
	}
	outputSchema := schemaForMessage(spec.Method.Output())

	no := false
	annotations := &sdk.ToolAnnotations{
		Title:          spec.Name,
		ReadOnlyHint:   spec.ReadOnly,
		IdempotentHint: spec.Idempotent,
		OpenWorldHint:  &no,
	}
	if !spec.ReadOnly {
		destructive := spec.Destructive
		annotations.DestructiveHint = &destructive
	}

	s.sdk.AddTool(
		&sdk.Tool{
			Name:         spec.Name,
			Description:  spec.Description,
			InputSchema:  inputSchema,
			OutputSchema: outputSchema,
			Annotations:  annotations,
		},
		s.toolHandler(spec),
	)
	s.names = append(s.names, spec.Name)
}

func (s *Server) toolHandler(spec methodSpec) sdk.ToolHandler {
	return func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		var args json.RawMessage
		if req != nil && req.Params != nil {
			args = req.Params.Arguments
		}
		start, err := s.beginTool(spec.Name, args)
		if err != nil {
			return toolError(fmt.Errorf("%s: %w", spec.Name, err)), nil
		}
		out, err := s.call(ctx, spec, args)
		s.finishTool(spec.Name, args, start, err)
		if err != nil {
			return toolError(fmt.Errorf("%s: %w", spec.Name, err)), nil
		}
		return &sdk.CallToolResult{
			Content:           []sdk.Content{&sdk.TextContent{Text: string(out)}},
			StructuredContent: json.RawMessage(out),
		}, nil
	}
}

// call converts MCP JSON arguments to the protobuf request, forwards it to the
// app, and converts the protobuf response back to JSON.
func (s *Server) call(ctx context.Context, spec methodSpec, args json.RawMessage) ([]byte, error) {
	in := dynamicpb.NewMessage(spec.Method.Input())
	if len(args) > 0 && string(args) != "null" {
		// The SDK's low-level handler does not validate, and protojson accepts
		// missing fields. Check the catalog's required names so an agent gets a
		// clear error instead of an engine-level surprise (for example an
		// update that silently clears assignments).
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(args, &fields); err != nil {
			return nil, fmt.Errorf("invalid arguments: %w", err)
		}
		for _, name := range spec.Required {
			raw, ok := fields[name]
			if !ok || string(raw) == "null" {
				return nil, fmt.Errorf("missing required argument %q", name)
			}
		}
		if err := protojson.Unmarshal(args, in); err != nil {
			return nil, fmt.Errorf("invalid arguments: %w", err)
		}
	} else if len(spec.Required) > 0 {
		return nil, fmt.Errorf("missing required argument %q", spec.Required[0])
	}
	req, err := proto.Marshal(in)
	if err != nil {
		return nil, fmt.Errorf("encode request: %w", err)
	}

	respBytes, err := s.backend.Call(ctx, spec.FullName, req)
	if err != nil {
		return nil, err
	}

	out := dynamicpb.NewMessage(spec.Method.Output())
	if err := proto.Unmarshal(respBytes, out); err != nil {
		return nil, fmt.Errorf("decode app response: %w", err)
	}
	// EmitUnpopulated keeps round-trips predictable for an agent: an empty
	// assignments list appears as [], so "read, modify, update" does not
	// silently drop a field because the response omitted it.
	return protojson.MarshalOptions{
		UseProtoNames:   true,
		EmitUnpopulated: true,
		Indent:          "  ",
	}.Marshal(out)
}

func toolError(err error) *sdk.CallToolResult {
	return &sdk.CallToolResult{
		IsError: true,
		Content: []sdk.Content{&sdk.TextContent{Text: err.Error()}},
	}
}

func (s *Server) addStatusTool() {
	s.sdk.AddTool(
		&sdk.Tool{
			Name:        "err_status",
			Description: "Report whether the err app is connected, the bridge port, the advertised toolset and the vault-independent server version.",
			InputSchema: map[string]any{
				"type":                 "object",
				"properties":           map[string]any{},
				"additionalProperties": false,
			},
			Annotations: &sdk.ToolAnnotations{
				ReadOnlyHint:   true,
				IdempotentHint: true,
				OpenWorldHint:  boolPtr(false),
			},
		},
		func(_ context.Context, _ *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			start, err := s.beginTool("err_status", nil)
			if err != nil {
				return toolError(fmt.Errorf("err_status: %w", err)), nil
			}
			status := map[string]any{
				"app_connected":       false,
				"bridge_port":         s.port,
				"toolset":             s.toolset,
				"tools":               len(s.names),
				"version":             s.version,
				"server_version":      s.version,
				"bridge_protocol":     SupportedBridgeProtocol,
				"catalog_fingerprint": CatalogFingerprint(),
				"rate_limit":          s.limiter.limit,
			}
			if path := s.audit.Path(); path != "" {
				status["audit_log"] = path
			}
			if s.bridge != nil {
				connected, since, pending := s.bridge.Status()
				status["app_connected"] = connected
				status["pending_calls"] = pending
				if connected {
					status["connected_since"] = since.UTC().Format(time.RFC3339)
				}
				if app, version, protocol, fingerprint := s.bridge.AppInfo(); app != "" {
					status["app"] = app
					status["app_version"] = version
					status["app_protocol"] = protocol
					status["app_schema"] = fingerprint
				}
			}
			body, _ := json.MarshalIndent(status, "", "  ")
			s.finishTool("err_status", nil, start, nil)
			return &sdk.CallToolResult{
				Content:           []sdk.Content{&sdk.TextContent{Text: string(body)}},
				StructuredContent: json.RawMessage(body),
			}, nil
		},
	)
	s.names = append(s.names, "err_status")
}

func boolPtr(v bool) *bool { return &v }

// MCP returns the underlying SDK server (used by tests).
func (s *Server) MCP() *sdk.Server { return s.sdk }

// ToolNames returns the advertised tool names in sorted order.
func (s *Server) ToolNames() []string { return append([]string(nil), s.names...) }

// Toolset returns the enabled tier label ("safe", "safe+sync", ...).
func (s *Server) Toolset() string { return s.toolset }

// RunStdio serves MCP over stdin/stdout for clients that launch a local
// process. It blocks until the client closes the stream or ctx is cancelled.
// The HTTP app bridge is independent: the browser still connects to
// /app/ws on the configured port.
func (s *Server) RunStdio(ctx context.Context) error {
	return s.sdk.Run(ctx, &sdk.StdioTransport{})
}

// ServeIO serves MCP over arbitrary streams. Production uses RunStdio; tests
// use in-memory pipes to exercise the real initialize/tool handshake.
func (s *Server) ServeIO(ctx context.Context, r io.ReadCloser, w io.WriteCloser) error {
	return s.sdk.Run(ctx, &sdk.IOTransport{Reader: r, Writer: w})
}
