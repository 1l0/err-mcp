//go:build !js

package mcp

import (
	"encoding/json"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

// The tests speak the app's wire format through the embedded descriptor set
// instead of the app's generated Go types, so they exercise exactly the
// decoding path the production code uses.

func testMessage(t *testing.T, fullName protoreflect.FullName) *dynamicpb.Message {
	t.Helper()
	descriptor, ok := lookupMessage(fullName)
	if !ok {
		t.Fatalf("unknown domain message %q", fullName)
	}
	return dynamicpb.NewMessage(descriptor)
}

// encodeProto turns a protojson body into protobuf wire bytes.
func encodeProto(t *testing.T, fullName protoreflect.FullName, body string) []byte {
	t.Helper()
	message := testMessage(t, fullName)
	if body != "" {
		if err := protojson.Unmarshal([]byte(body), message); err != nil {
			t.Fatalf("encode %s: %v", fullName, err)
		}
	}
	raw, err := proto.Marshal(message)
	if err != nil {
		t.Fatalf("marshal %s: %v", fullName, err)
	}
	return raw
}

// decodeProto decodes protobuf wire bytes into plain Go structs or maps.
func decodeProto(t *testing.T, fullName protoreflect.FullName, raw []byte, out any) {
	t.Helper()
	message := testMessage(t, fullName)
	if err := proto.Unmarshal(raw, message); err != nil {
		t.Fatalf("unmarshal %s: %v", fullName, err)
	}
	body, err := protojson.MarshalOptions{UseProtoNames: true}.Marshal(message)
	if err != nil {
		t.Fatalf("decode %s: %v", fullName, err)
	}
	if err := json.Unmarshal(body, out); err != nil {
		t.Fatalf("decode %s into %T: %v", fullName, out, err)
	}
}

// mustJSON marshals a fixture into protojson input.
func mustJSON(t *testing.T, value any) string {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	return string(body)
}

// testItem is the Item fixture shape used by the fake backends.
type testItem struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Text      string `json:"text"`
	UpdatedAt string `json:"updated_at"`
}

// testTreeItem and testTreeRequest mirror CreateTree so the relay tests can
// assert on the nested request without generated types.
type testTreeItem struct {
	Title    string         `json:"title"`
	Text     string         `json:"text"`
	Children []testTreeItem `json:"children"`
}

type testTreeRequest struct {
	Items    []testTreeItem `json:"items"`
	ParentID string         `json:"parent_id"`
	BeforeID string         `json:"before_id"`
}
