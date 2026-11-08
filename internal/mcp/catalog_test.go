//go:build !js

package mcp

import (
	"context"
	"encoding/json"
	"testing"
)

func TestCatalogMatchesServices(t *testing.T) {
	specs := buildCatalog(domainFileDescriptor())
	if len(specs) != 35 {
		t.Fatalf("catalog has %d methods, want 35", len(specs))
	}
	seen := make(map[string]bool, len(specs))
	for _, spec := range specs {
		if seen[spec.Name] {
			t.Errorf("duplicate tool %q", spec.Name)
		}
		seen[spec.Name] = true
		if _, ok := methodMeta[spec.Name]; !ok {
			t.Errorf("new RPC method %q has no methodMeta entry; add a description and a tier", spec.Name)
		}
		if spec.Description == "" {
			t.Errorf("%s has no description", spec.Name)
		}
		if len(spec.FullName) < 2 || spec.FullName[0] != '/' {
			t.Errorf("%s has bad RPC path %q", spec.Name, spec.FullName)
		}
	}
	for _, want := range []string{
		"get_item", "create_item", "create_tree", "update_item", "children",
		"world_at", "undo", "import_segment", "get_vault_key", "run_gc",
		"preview_delete", "delete_item_cascade",
	} {
		if !seen[want] {
			t.Errorf("missing tool %q", want)
		}
	}
}

// TestCatalogFingerprint pins the committed descriptor. The Dart app computes
// the same fingerprint from its generated domain.pbjson.dart, so this test
// fails when the embedded domain.fds goes stale after a proto change.
func TestCatalogFingerprint(t *testing.T) {
	const want = "cf24ebe3e22e8876"
	if got := CatalogFingerprint(); got != want {
		t.Fatalf("catalog fingerprint = %q, want %q (regenerate domain.fds with scripts/gen-descriptor.sh)", got, want)
	}
}

func TestSnakeCase(t *testing.T) {
	cases := map[string]string{
		"GetItem":      "get_item",
		"WorldAt":      "world_at",
		"RunGC":        "run_gc",
		"GetSyncState": "get_sync_state",
		"ListItems":    "list_items",
		"URL":          "url",
	}
	for in, want := range cases {
		if got := snakeCase(in); got != want {
			t.Errorf("snakeCase(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSchemas(t *testing.T) {
	specs := buildCatalog(domainFileDescriptor())
	byName := make(map[string]methodSpec, len(specs))
	for _, spec := range specs {
		byName[spec.Name] = spec
	}

	get, ok := byName["get_item"]
	if !ok {
		t.Fatal("get_item missing")
	}
	schema := schemaForMessage(get.Method.Input())
	props, _ := schema["properties"].(map[string]any)
	if _, ok := props["id"]; !ok {
		t.Errorf("get_item input schema lacks id: %#v", props)
	}

	update, ok := byName["update_item"]
	if !ok {
		t.Fatal("update_item missing")
	}
	updateSchema := schemaForMessage(update.Method.Input())
	updateProps, _ := updateSchema["properties"].(map[string]any)
	for _, field := range []string{"id", "title", "text", "assignments"} {
		if _, ok := updateProps[field]; !ok {
			t.Errorf("update_item input schema lacks %s: %#v", field, updateProps)
		}
	}
	assignments, _ := updateProps["assignments"].(map[string]any)
	if assignments["type"] != "array" {
		t.Errorf("assignments schema = %#v, want an array", assignments)
	}

	// Every schema must be JSON-marshalable; this catches accidental cycles.
	for _, spec := range specs {
		if _, err := json.Marshal(schemaForMessage(spec.Method.Input())); err != nil {
			t.Errorf("%s input schema: %v", spec.Name, err)
		}
		if _, err := json.Marshal(schemaForMessage(spec.Method.Output())); err != nil {
			t.Errorf("%s output schema: %v", spec.Name, err)
		}
	}
}

func TestTierFiltering(t *testing.T) {
	backend := BackendFunc(func(_ context.Context, _ string, _ []byte) ([]byte, error) {
		return nil, nil
	})

	safe := NewServer(ServerConfig{Backend: backend, Toolset: "safe"})
	safeNames := toSet(safe.ToolNames())
	for _, name := range []string{"get_item", "err_status", "count_for_peer"} {
		if !safeNames[name] {
			t.Errorf("safe toolset lacks %q", name)
		}
	}
	for _, name := range []string{"import_segment", "get_vault_key", "run_gc"} {
		if safeNames[name] {
			t.Errorf("safe toolset exposes %q", name)
		}
	}

	syncOnly := NewServer(ServerConfig{Backend: backend, AllowSync: true})
	syncNames := toSet(syncOnly.ToolNames())
	if !syncNames["export_segment"] || !syncNames["create_snapshot"] {
		t.Errorf("sync tools missing with --allow-sync")
	}
	// Destructive sync tools need their own opt-in, independent of --allow-sync.
	for _, name := range []string{"import_segment", "adopt_vault_key", "run_gc"} {
		if syncNames[name] {
			t.Errorf("--allow-sync must not expose destructive tool %q", name)
		}
	}
	if syncNames["get_vault_key"] {
		t.Errorf("--allow-sync must not expose the vault key")
	}

	destructive := NewServer(ServerConfig{Backend: backend, AllowSync: true, AllowDestructive: true})
	destructiveNames := toSet(destructive.ToolNames())
	for _, name := range []string{"import_segment", "adopt_vault_key", "run_gc"} {
		if !destructiveNames[name] {
			t.Errorf("--allow-sync --allow-destructive lacks %q", name)
		}
	}
	if destructiveNames["get_vault_key"] {
		t.Errorf("--allow-destructive must not expose the vault key")
	}

	all := NewServer(ServerConfig{Backend: backend, Toolset: "all"})
	allNames := toSet(all.ToolNames())
	if !allNames["get_vault_key"] {
		t.Errorf("--toolset=all lacks get_vault_key")
	}
	if !allNames["import_segment"] {
		t.Errorf("--toolset=all lacks import_segment")
	}
}

func toSet(values []string) map[string]bool {
	out := make(map[string]bool, len(values))
	for _, value := range values {
		out[value] = true
	}
	return out
}
