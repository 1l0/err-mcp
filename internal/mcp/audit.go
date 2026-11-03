//go:build !js

package mcp

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"
)

// AuditConfig configures the per-call audit log.
//
// The audit log records one entry per MCP tool call. It never receives the
// call arguments wholesale: only target ids are extracted with an allowlist,
// so note text, segments and vault keys cannot leak into the log.
type AuditConfig struct {
	// Path, when set, is appended as newline-delimited JSON. The file is
	// created with mode 0600.
	Path string
	// Logger receives the human-readable line. It defaults to slog.Default().
	Logger *slog.Logger
}

// AuditEntry is one audited tool call.
type AuditEntry struct {
	Time       time.Time `json:"time"`
	Tool       string    `json:"tool"`
	DurationMS int64     `json:"duration_ms"`
	OK         bool      `json:"ok"`
	Error      string    `json:"error,omitempty"`
	Targets    []string  `json:"targets,omitempty"`
}

// AuditLog writes the per-call audit trail. A nil *AuditLog is safe to use and
// drops entries, which keeps optional wiring simple in tests.
type AuditLog struct {
	logger *slog.Logger
	path   string
	file   *os.File
	mu     sync.Mutex
}

// NewAuditLog opens the optional JSONL file and prepares the stderr logger.
func NewAuditLog(cfg AuditConfig) (*AuditLog, error) {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	a := &AuditLog{logger: logger, path: cfg.Path}
	if cfg.Path != "" {
		file, err := os.OpenFile(cfg.Path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return nil, fmt.Errorf("open audit log %s: %w", cfg.Path, err)
		}
		a.file = file
	}
	return a, nil
}

// Path returns the configured JSONL path, or "" when logging to stderr only.
func (a *AuditLog) Path() string {
	if a == nil {
		return ""
	}
	return a.path
}

// Close releases the optional file.
func (a *AuditLog) Close() error {
	if a == nil || a.file == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	err := a.file.Close()
	a.file = nil
	return err
}

// Record appends one entry. err is the tool-call failure, if any. args is the
// raw MCP argument object; only target ids are extracted from it.
func (a *AuditLog) Record(tool string, args json.RawMessage, duration time.Duration, callErr error) {
	if a == nil {
		return
	}
	entry := AuditEntry{
		Time:       time.Now().UTC(),
		Tool:       tool,
		DurationMS: duration.Milliseconds(),
		OK:         callErr == nil,
		Targets:    auditTargets(args),
	}
	if callErr != nil {
		entry.Error = truncate(callErr.Error(), 300)
	}

	attrs := []any{
		"tool", entry.Tool,
		"duration_ms", entry.DurationMS,
		"ok", entry.OK,
	}
	if len(entry.Targets) > 0 {
		attrs = append(attrs, "targets", strings.Join(entry.Targets, ","))
	}
	if entry.Error != "" {
		attrs = append(attrs, "error", entry.Error)
	}
	a.logger.Info("mcp tool call", attrs...)

	if a.file == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.file == nil {
		return
	}
	enc := json.NewEncoder(a.file)
	if err := enc.Encode(entry); err != nil {
		a.logger.Warn("write audit log", "error", err)
	}
}

// auditIDFields is the allowlist of argument fields that hold item or view
// ids. Content-bearing fields (text, query, key, segment, ...) are never
// logged.
var auditIDFields = []string{
	"id",
	"parent_id",
	"child_id",
	"from_parent_id",
	"to_parent_id",
	"root_id",
	"context_id",
	"before_id",
}

// auditTargets extracts "field=id" pairs from the argument object. Order is
// deterministic so log lines are stable and easy to grep.
func auditTargets(args json.RawMessage) []string {
	if len(args) == 0 || string(args) == "null" {
		return nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(args, &fields); err != nil {
		return nil
	}
	var out []string
	for _, key := range auditIDFields {
		raw, ok := fields[key]
		if !ok {
			continue
		}
		var value string
		if err := json.Unmarshal(raw, &value); err != nil || value == "" {
			continue
		}
		out = append(out, key+"="+value)
	}
	return out
}

// rateLimiter is a fixed-window per-tool limiter. A nil or zero-limit limiter
// allows every call.
type rateLimiter struct {
	limit   int
	mu      sync.Mutex
	windows map[string]rateWindow
}

type rateWindow struct {
	start time.Time
	count int
}

func newRateLimiter(limit int) *rateLimiter {
	return &rateLimiter{limit: limit, windows: make(map[string]rateWindow)}
}

func (r *rateLimiter) allow(tool string, now time.Time) error {
	if r == nil || r.limit <= 0 {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	window := r.windows[tool]
	if window.start.IsZero() || now.Sub(window.start) >= time.Minute {
		window = rateWindow{start: now}
	}
	if window.count >= r.limit {
		r.windows[tool] = window
		return fmt.Errorf("rate limit exceeded: %s allows %d calls per minute", tool, r.limit)
	}
	window.count++
	r.windows[tool] = window
	return nil
}

// truncate limits an error message by rune count so a Japanese diagnostic is
// never cut mid-character.
func truncate(s string, n int) string {
	if n <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "…"
}
