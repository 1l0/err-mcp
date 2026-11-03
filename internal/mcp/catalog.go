//go:build !js

package mcp

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// CatalogFingerprint is a stable fingerprint of the RPC service descriptors.
// The Dart app computes the same value from its generated domain.pbjson.dart,
// so a mismatch means the two sides were generated from different
// domain.proto files. The first 16 hex characters are plenty for diagnostics.
func CatalogFingerprint() string {
	file, err := domainDescriptor()
	if err != nil {
		return ""
	}
	h := sha256.New()
	for _, name := range []string{"SyncService", "ItemService"} {
		service := file.Services().ByName(protoreflect.Name(name))
		if service == nil {
			continue
		}
		raw, err := proto.Marshal(protodesc.ToServiceDescriptorProto(service))
		if err != nil {
			return ""
		}
		h.Write(raw)
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// ToolTier controls which tools are advertised by default.
type ToolTier int

const (
	// TierSafe is the default item/graph/view surface. It cannot rekey,
	// import segments, or disclose the vault key.
	TierSafe ToolTier = iota
	// TierSync contains segment import/export, snapshot, key adoption and GC.
	// These can lose or rekey a vault.
	TierSync
	// TierDestructive is the subset of sync tools that can lose or rekey the
	// vault (import_segment, adopt_vault_key, run_gc). It requires both
	// --allow-sync and --allow-destructive, so it can be disabled independently
	// of the rest of the sync tier.
	TierDestructive
	// TierSecret is the vault key itself. It is never enabled by default.
	TierSecret
)

func (t ToolTier) String() string {
	switch t {
	case TierSafe:
		return "safe"
	case TierSync:
		return "sync"
	case TierDestructive:
		return "destructive"
	case TierSecret:
		return "secret"
	default:
		return "unknown"
	}
}

// methodSpec is one generated MCP tool.
type methodSpec struct {
	Name        string
	FullName    string // godash RPC path, e.g. /pb.ItemService/GetItem
	Method      protoreflect.MethodDescriptor
	Tier        ToolTier
	Description string
	Required    []string
	ReadOnly    bool
	Destructive bool
	Idempotent  bool
}

type toolMeta struct {
	Description string
	Required    []string
	Tier        ToolTier
	ReadOnly    bool
	Destructive bool
	Idempotent  bool
}

// methodMeta is the hand-written part of the catalog: descriptions, required
// fields and safety annotations. Schemas themselves come from the protobuf
// descriptors so a new RPC method is never silently untyped.
//
// New methods not listed here are advertised only with --allow-sync, because
// an unknown method might mutate or expose data.
var methodMeta = map[string]toolMeta{
	"get_item": {
		Description: "Get one Item by id, including the full body text and property writes. Use list_titles, outline or search_items to discover ids first.",
		Required:    []string{"id"},
		Tier:        TierSafe,
		ReadOnly:    true,
		Idempotent:  true,
	},
	"list_items": {
		Description: "List every live Item in the vault with full body text and property writes. Expensive on large vaults; prefer list_titles, outline or search_items for discovery, and get_item for a single body.",
		Tier:        TierSafe,
		ReadOnly:    true,
		Idempotent:  true,
	},
	"create_item": {
		Description: "Create a new Item with a title. It starts at the top level of the Forest; use add_parent or move_item to place it under another Item.",
		Required:    []string{"title"},
		Tier:        TierSafe,
	},
	"create_tree": {
		Description: "Create a nested tree of Items in one call and one undo step. items is a depth-first array of {title, text, children}; optional parent_id places the top-level nodes under an Item (default Forest) and before_id inserts them before a sibling. Returns the created Items in creation order.",
		Required:    []string{"items"},
		Tier:        TierSafe,
	},
	"update_item": {
		Description: "Replace an Item's title, body text and property writes. This is a whole-Item last-write-wins update: include every field you want to keep, not only the changed ones. Send assignments as [] to clear them.",
		Required:    []string{"id", "title", "text", "assignments"},
		Tier:        TierSafe,
	},
	"delete_item": {
		Description: "Soft-delete an Item. It can be restored with restore_item.",
		Required:    []string{"id"},
		Tier:        TierSafe,
		Destructive: true,
	},
	"restore_item": {
		Description: "Restore a soft-deleted Item. Its id must still be known.",
		Required:    []string{"id"},
		Tier:        TierSafe,
	},
	"children": {
		Description: "List the ordered children of an Item, with each PARENT_OF edge's order_key and edge_id.",
		Required:    []string{"parent_id"},
		Tier:        TierSafe,
		ReadOnly:    true,
		Idempotent:  true,
	},
	"parents": {
		Description: "List an Item's parent Items. Multiple parents mean the Item is an alias under several places.",
		Required:    []string{"child_id"},
		Tier:        TierSafe,
		ReadOnly:    true,
		Idempotent:  true,
	},
	"roots": {
		Description: "List the user-visible top-level Items (Forest children with no other live parent), in order.",
		Tier:        TierSafe,
		ReadOnly:    true,
		Idempotent:  true,
	},
	"add_parent": {
		Description: "Add an alias: make parent_id a parent of child_id. Optional before_id places the alias before that sibling.",
		Required:    []string{"parent_id", "child_id"},
		Tier:        TierSafe,
	},
	"remove_parent": {
		Description: "Remove one PARENT_OF edge between parent_id and child_id.",
		Required:    []string{"parent_id", "child_id"},
		Tier:        TierSafe,
		Destructive: true,
	},
	"move_item": {
		Description: "Move child_id from from_parent_id to to_parent_id. Optional before_id places it before that sibling.",
		Required:    []string{"child_id", "from_parent_id", "to_parent_id"},
		Tier:        TierSafe,
	},
	"reorder_child": {
		Description: "Move child_id within parent_id's children, optionally before before_id.",
		Required:    []string{"parent_id", "child_id"},
		Tier:        TierSafe,
	},
	"reorder_root": {
		Description: "Move a top-level Item within the Forest order, optionally before before_id.",
		Required:    []string{"id"},
		Tier:        TierSafe,
	},
	"move_to_root": {
		Description: "Make an Item a top-level Forest child, optionally before before_id.",
		Required:    []string{"id"},
		Tier:        TierSafe,
	},
	"list_views": {
		Description: "List saved Tree Views (bookmarks), each with its root Item id.",
		Tier:        TierSafe,
		ReadOnly:    true,
		Idempotent:  true,
	},
	"create_view": {
		Description: "Create a Tree View named name rooted at root_id.",
		Required:    []string{"name", "root_id"},
		Tier:        TierSafe,
	},
	"rename_view": {
		Description: "Rename a Tree View.",
		Required:    []string{"id", "name"},
		Tier:        TierSafe,
		Idempotent:  true,
	},
	"delete_view": {
		Description: "Delete a Tree View. The root Item itself is not deleted.",
		Required:    []string{"id"},
		Tier:        TierSafe,
		Destructive: true,
	},
	"world_at": {
		Description: "Compute every property visible from an Item by walking its ancestors, plus diagnostics. Each value includes the source Item id.",
		Required:    []string{"context_id"},
		Tier:        TierSafe,
		ReadOnly:    true,
		Idempotent:  true,
	},
	"history": {
		Description: "Return the undo and redo stack labels for the current app session.",
		Tier:        TierSafe,
		ReadOnly:    true,
		Idempotent:  true,
	},
	"undo": {
		Description: "Undo the most recent app-session change. force bypasses the remote-change guard.",
		Tier:        TierSafe,
	},
	"redo": {
		Description: "Redo the most recently undone app-session change. force bypasses the remote-change guard.",
		Tier:        TierSafe,
	},
	"get_sync_state": {
		Description: "Return this device's id, OpLog version vector and vault fingerprint. It does not return the key.",
		Tier:        TierSafe,
		ReadOnly:    true,
		Idempotent:  true,
	},
	"count_for_peer": {
		Description: "Count the operations and bytes a peer version vector is missing; used for sync progress.",
		Tier:        TierSafe,
		ReadOnly:    true,
		Idempotent:  true,
	},

	// Sync tier: opt in with --allow-sync.
	"export_segment": {
		Description: "Export a sealed, encrypted OpLog segment. Advances the folder export watermark; prefer export_for_peer for peer sync.",
		Tier:        TierSync,
	},
	"import_segment": {
		Description: "Import a sealed segment or snapshot produced by export_segment, export_for_peer or create_snapshot. Requires --allow-sync and --allow-destructive.",
		Required:    []string{"segment"},
		Tier:        TierDestructive,
		Destructive: true,
	},
	"export_for_peer": {
		Description: "Export the delta a peer's version vector is missing. Does not advance the folder export watermark.",
		Tier:        TierSync,
		ReadOnly:    true,
		Idempotent:  true,
	},
	"create_snapshot": {
		Description: "Compact and export a sealed bootstrap snapshot of the whole vault.",
		Tier:        TierSync,
	},
	"adopt_vault_key": {
		Description: "Replace this vault's key with a base64 pairing code. Dangerous: it changes which devices can decrypt the vault. Requires --allow-sync and --allow-destructive.",
		Required:    []string{"key"},
		Tier:        TierDestructive,
		Destructive: true,
	},
	"run_gc": {
		Description: "Purge tombstones up to grace_hlc. With an empty grace_hlc the last snapshot is used. Requires --allow-sync and --allow-destructive.",
		Tier:        TierDestructive,
		Destructive: true,
	},

	// Secret tier: never enabled by default.
	"get_vault_key": {
		Description: "Return this vault's base64 encryption key. Sensitive: anyone with this key can decrypt the vault.",
		Tier:        TierSecret,
		ReadOnly:    true,
		Idempotent:  true,
	},
}

// buildCatalog walks the generated service descriptors and returns one spec
// per RPC method.
func buildCatalog(file protoreflect.FileDescriptor) []methodSpec {
	var specs []methodSpec
	for _, serviceName := range []string{"ItemService", "SyncService"} {
		service := file.Services().ByName(protoreflect.Name(serviceName))
		if service == nil {
			continue
		}
		for i := 0; i < service.Methods().Len(); i++ {
			method := service.Methods().Get(i)
			name := snakeCase(string(method.Name()))
			meta, known := methodMeta[name]
			if !known {
				meta = toolMeta{
					Description: fmt.Sprintf("%s.%s", serviceName, method.Name()),
					// Unknown methods are opt-in: they may mutate or expose data.
					Tier: TierSync,
				}
			}
			specs = append(specs, methodSpec{
				Name:        name,
				FullName:    fmt.Sprintf("/%s/%s", service.FullName(), method.Name()),
				Method:      method,
				Tier:        meta.Tier,
				Description: meta.Description,
				Required:    meta.Required,
				ReadOnly:    meta.ReadOnly,
				Destructive: meta.Destructive,
				Idempotent:  meta.Idempotent,
			})
		}
	}
	return specs
}

// schemaForMessage builds an inline JSON Schema object for a protobuf message.
// The domain messages are small and non-recursive; the depth cap keeps a future
// recursive message from producing an unbounded document.
func schemaForMessage(md protoreflect.MessageDescriptor) map[string]any {
	return schemaForMessageDepth(md, 0)
}

func schemaForMessageDepth(md protoreflect.MessageDescriptor, depth int) map[string]any {
	if depth > 8 {
		return map[string]any{"type": "object"}
	}
	properties := make(map[string]any, md.Fields().Len())
	for i := 0; i < md.Fields().Len(); i++ {
		fd := md.Fields().Get(i)
		// The protobuf field name (snake_case) is accepted by protojson on
		// input and used on output (UseProtoNames).
		properties[string(fd.Name())] = schemaForField(fd, depth)
	}
	return map[string]any{
		"type":                 "object",
		"properties":           properties,
		"additionalProperties": false,
	}
}

func schemaForField(fd protoreflect.FieldDescriptor, depth int) map[string]any {
	switch {
	case fd.IsMap():
		return map[string]any{
			"type":                 "object",
			"additionalProperties": schemaForKind(fd.MapValue(), depth),
		}
	case fd.IsList():
		return map[string]any{
			"type":  "array",
			"items": schemaForKind(fd, depth),
		}
	default:
		return schemaForKind(fd, depth)
	}
}

func schemaForKind(fd protoreflect.FieldDescriptor, depth int) map[string]any {
	var out map[string]any
	switch fd.Kind() {
	case protoreflect.BoolKind:
		out = map[string]any{"type": "boolean"}
	case protoreflect.EnumKind:
		out = map[string]any{"type": "string", "enum": enumNames(fd.Enum())}
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind:
		out = map[string]any{"type": "integer", "format": "int32"}
	case protoreflect.Uint32Kind, protoreflect.Fixed32Kind:
		out = map[string]any{"type": "integer", "format": "uint32"}
	case protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
		// protojson encodes 64-bit integers as strings.
		out = map[string]any{"type": "string", "format": "int64"}
	case protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		out = map[string]any{"type": "string", "format": "uint64"}
	case protoreflect.FloatKind:
		out = map[string]any{"type": "number", "format": "float"}
	case protoreflect.DoubleKind:
		out = map[string]any{"type": "number", "format": "double"}
	case protoreflect.StringKind:
		out = map[string]any{"type": "string"}
	case protoreflect.BytesKind:
		out = map[string]any{"type": "string", "contentEncoding": "base64"}
	case protoreflect.MessageKind, protoreflect.GroupKind:
		out = schemaForMessageDepth(fd.Message(), depth+1)
	default:
		out = map[string]any{}
	}
	return out
}

func enumNames(ed protoreflect.EnumDescriptor) []string {
	names := make([]string, 0, ed.Values().Len())
	for i := 0; i < ed.Values().Len(); i++ {
		names = append(names, string(ed.Values().Get(i).Name()))
	}
	return names
}

// snakeCase converts an RPC method name to a tool name: RunGC -> run_gc,
// WorldAt -> world_at, GetItem -> get_item.
func snakeCase(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			if i > 0 {
				prev := s[i-1]
				nextLower := i+1 < len(s) && s[i+1] >= 'a' && s[i+1] <= 'z'
				if (prev >= 'a' && prev <= 'z') || (prev >= '0' && prev <= '9') ||
					(prev >= 'A' && prev <= 'Z' && nextLower) {
					b.WriteByte('_')
				}
			}
			b.WriteByte(c + ('a' - 'A'))
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}
