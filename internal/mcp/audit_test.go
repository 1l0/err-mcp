//go:build !js

package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestAuditTargets(t *testing.T) {
	cases := []struct {
		name string
		args string
		want []string
	}{
		{name: "empty", args: "", want: nil},
		{name: "null", args: "null", want: nil},
		{name: "single id", args: `{"id":"it_1"}`, want: []string{"id=it_1"}},
		{
			name: "edge endpoints",
			args: `{"parent_id":"p","child_id":"c","before_id":"b"}`,
			want: []string{"parent_id=p", "child_id=c", "before_id=b"},
		},
		{
			name: "content is ignored",
			args: `{"id":"it_1","text":"secret note","query":"secret","key":"vault-key","segment":"ciphertext"}`,
			want: []string{"id=it_1"},
		},
		{name: "non-string id", args: `{"id":42}`, want: nil},
		{name: "empty id", args: `{"id":""}`, want: nil},
		{name: "invalid json", args: `{`, want: nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := auditTargets(json.RawMessage(tc.args))
			if len(got) != len(tc.want) {
				t.Fatalf("auditTargets(%s) = %v, want %v", tc.args, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("auditTargets(%s) = %v, want %v", tc.args, got, tc.want)
				}
			}
		})
	}
}

func TestTruncateRunes(t *testing.T) {
	if got := truncate("short", 10); got != "short" {
		t.Fatalf("truncate kept %q", got)
	}
	if got := truncate(strings.Repeat("日", 10), 3); got != "日日日…" {
		t.Fatalf("truncate = %q", got)
	}
	if got := truncate("anything", 0); got != "" {
		t.Fatalf("truncate with zero = %q", got)
	}
}

func TestAuditLogWritesJSONLWithoutContent(t *testing.T) {
	var stderr bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	audit, err := NewAuditLog(AuditConfig{Path: path, Logger: logger})
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}

	args := json.RawMessage(`{"id":"it_1","text":"secret note","key":"vault-key","segment":"ciphertext"}`)
	audit.Record("update_item", args, 12*time.Millisecond, nil)
	audit.Record("delete_item", json.RawMessage(`{"id":"it_2"}`), time.Millisecond, errors.New("item not found"))
	if err := audit.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read audit log: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("audit log has %d lines, want 2: %s", len(lines), data)
	}

	var first AuditEntry
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatalf("decode first line: %v", err)
	}
	if first.Tool != "update_item" || !first.OK {
		t.Fatalf("first entry = %+v", first)
	}
	if len(first.Targets) != 1 || first.Targets[0] != "id=it_1" {
		t.Fatalf("first targets = %v", first.Targets)
	}

	var second AuditEntry
	if err := json.Unmarshal([]byte(lines[1]), &second); err != nil {
		t.Fatalf("decode second line: %v", err)
	}
	if second.Tool != "delete_item" || second.OK || !strings.Contains(second.Error, "item not found") {
		t.Fatalf("second entry = %+v", second)
	}

	for _, leaked := range []string{"secret note", "vault-key", "ciphertext"} {
		if strings.Contains(string(data), leaked) || strings.Contains(stderr.String(), leaked) {
			t.Errorf("audit output leaked %q:\nfile: %s\nstderr: %s", leaked, data, stderr.String())
		}
	}
	if !strings.Contains(stderr.String(), "tool=update_item") {
		t.Errorf("stderr audit line missing tool name: %s", stderr.String())
	}
}

func TestRateLimiter(t *testing.T) {
	limiter := newRateLimiter(2)
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)

	if err := limiter.allow("get_item", now); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if err := limiter.allow("get_item", now.Add(time.Second)); err != nil {
		t.Fatalf("second call: %v", err)
	}
	if err := limiter.allow("get_item", now.Add(2*time.Second)); err == nil {
		t.Fatal("third call was not rate limited")
	}
	// The window is per tool: another tool is unaffected.
	if err := limiter.allow("list_titles", now.Add(2*time.Second)); err != nil {
		t.Fatalf("other tool was limited: %v", err)
	}
	// A new minute resets the window.
	if err := limiter.allow("get_item", now.Add(time.Minute)); err != nil {
		t.Fatalf("call after window reset: %v", err)
	}

	disabled := newRateLimiter(0)
	for i := 0; i < 100; i++ {
		if err := disabled.allow("get_item", now); err != nil {
			t.Fatalf("disabled limiter rejected call %d: %v", i, err)
		}
	}
}

func TestServerAuditsEveryCall(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	audit, err := NewAuditLog(AuditConfig{Path: path, Logger: logger})
	if err != nil {
		t.Fatalf("NewAuditLog: %v", err)
	}
	server := NewServer(ServerConfig{Backend: fakeItemBackend(t, "Hello"), Audit: audit, Logger: logger})
	session := connectTestClient(t, server)

	if _, err := session.CallTool(context.Background(), &sdk.CallToolParams{
		Name:      "get_item",
		Arguments: map[string]any{"id": "it_1"},
	}); err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if _, err := session.CallTool(context.Background(), &sdk.CallToolParams{
		Name: "err_status",
	}); err != nil {
		t.Fatalf("err_status: %v", err)
	}
	if err := audit.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read audit log: %v", err)
	}
	text := string(data)
	for _, want := range []string{`"tool":"get_item"`, `"tool":"err_status"`, `"targets":["id=it_1"]`} {
		if !strings.Contains(text, want) {
			t.Errorf("audit log lacks %s: %s", want, text)
		}
	}
	// Responses are never part of the audit trail.
	if strings.Contains(text, "Hello") || strings.Contains(text, `"text"`) {
		t.Errorf("audit log contains response content: %s", text)
	}
}
