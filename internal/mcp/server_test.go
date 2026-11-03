//go:build !js

package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func fakeItemBackend(t *testing.T, title string) Backend {
	t.Helper()
	return BackendFunc(func(_ context.Context, method string, req []byte) ([]byte, error) {
		if method != "/pb.ItemService/GetItem" {
			return nil, &RPCError{Code: 404, Message: "unexpected method " + method}
		}
		var in struct {
			ID string `json:"id"`
		}
		decodeProto(t, "pb.GetItemRequest", req, &in)
		body := fmt.Sprintf(`{"item":{"id":%q,"title":%q,"text":"body","created_at":"now","updated_at":"now"}}`, in.ID, title)
		return encodeProto(t, "pb.GetItemResponse", body), nil
	})
}

func connectTestClient(t *testing.T, server *Server) *sdk.ClientSession {
	t.Helper()
	ctx := context.Background()
	client := sdk.NewClient(&sdk.Implementation{Name: "test-client", Version: "1"}, nil)
	serverTransport, clientTransport := sdk.NewInMemoryTransports()
	if _, err := server.MCP().Connect(ctx, serverTransport, nil); err != nil {
		t.Fatalf("connect server: %v", err)
	}
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("connect client: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

func textContent(t *testing.T, result *sdk.CallToolResult) string {
	t.Helper()
	for _, content := range result.Content {
		if text, ok := content.(*sdk.TextContent); ok {
			return text.Text
		}
	}
	t.Fatalf("result has no text content: %#v", result.Content)
	return ""
}

func TestServerToolDispatch(t *testing.T) {
	server := NewServer(ServerConfig{Backend: fakeItemBackend(t, "Hello")})
	session := connectTestClient(t, server)

	result, err := session.CallTool(context.Background(), &sdk.CallToolParams{
		Name:      "get_item",
		Arguments: map[string]any{"id": "it_1"},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if result.IsError {
		t.Fatalf("tool returned an error: %s", textContent(t, result))
	}
	var body struct {
		Item struct {
			ID    string `json:"id"`
			Title string `json:"title"`
			Text  string `json:"text"`
		} `json:"item"`
	}
	if err := json.Unmarshal([]byte(textContent(t, result)), &body); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if body.Item.ID != "it_1" || body.Item.Title != "Hello" || body.Item.Text != "body" {
		t.Fatalf("item = %+v", body.Item)
	}

	// StructuredContent must mirror the text payload.
	structured, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatalf("marshal structured content: %v", err)
	}
	if !strings.Contains(string(structured), `"title":"Hello"`) {
		t.Fatalf("structured content = %s", structured)
	}
}

func TestServerRequiresFields(t *testing.T) {
	server := NewServer(ServerConfig{Backend: fakeItemBackend(t, "x")})
	session := connectTestClient(t, server)

	result, err := session.CallTool(context.Background(), &sdk.CallToolParams{Name: "get_item"})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if !result.IsError {
		t.Fatal("missing id was accepted")
	}
	if text := textContent(t, result); !strings.Contains(text, `missing required argument "id"`) {
		t.Fatalf("error text = %q", text)
	}

	// update_item requires assignments too, so a read-modify-write cannot
	// silently clear the property writes.
	result, err = session.CallTool(context.Background(), &sdk.CallToolParams{
		Name:      "update_item",
		Arguments: map[string]any{"id": "x", "title": "t", "text": "b"},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if !result.IsError || !strings.Contains(textContent(t, result), `"assignments"`) {
		t.Fatalf("update without assignments was accepted: %s", textContent(t, result))
	}
}

func TestServerToolError(t *testing.T) {
	server := NewServer(ServerConfig{Backend: BackendFunc(
		func(context.Context, string, []byte) ([]byte, error) {
			return nil, &RPCError{Code: 500, Message: "item not found"}
		},
	)})
	session := connectTestClient(t, server)

	result, err := session.CallTool(context.Background(), &sdk.CallToolParams{
		Name:      "get_item",
		Arguments: map[string]any{"id": "missing"},
	})
	if err != nil {
		t.Fatalf("CallTool returned a protocol error: %v", err)
	}
	if !result.IsError {
		t.Fatal("result is not marked as an error")
	}
	text := textContent(t, result)
	if !strings.Contains(text, "item not found") {
		t.Fatalf("error text = %q", text)
	}
}

func TestServerListsTools(t *testing.T) {
	server := NewServer(ServerConfig{Backend: fakeItemBackend(t, "x")})
	session := connectTestClient(t, server)

	names := make(map[string]bool)
	for tool, err := range session.Tools(context.Background(), nil) {
		if err != nil {
			t.Fatalf("Tools: %v", err)
		}
		names[tool.Name] = true
		if tool.InputSchema == nil {
			t.Errorf("%s has no input schema", tool.Name)
		}
	}
	for _, want := range []string{"get_item", "create_item", "children", "world_at", "undo", "err_status"} {
		if !names[want] {
			t.Errorf("missing tool %q", want)
		}
	}
	for _, hidden := range []string{"import_segment", "get_vault_key", "run_gc"} {
		if names[hidden] {
			t.Errorf("safe toolset exposes %q", hidden)
		}
	}
}

func TestServerCreateTreeTool(t *testing.T) {
	var captured testTreeRequest
	server := NewServer(ServerConfig{Backend: BackendFunc(
		func(_ context.Context, method string, req []byte) ([]byte, error) {
			if method != "/pb.ItemService/CreateTree" {
				return nil, &RPCError{Code: 404, Message: "unexpected method " + method}
			}
			decodeProto(t, "pb.CreateTreeRequest", req, &captured)
			return encodeProto(t, "pb.CreateTreeResponse",
				`{"count":2,"items":[{"id":"it_a","title":"A"},{"id":"it_b","title":"B"}]}`), nil
		},
	)})
	session := connectTestClient(t, server)

	result, err := session.CallTool(context.Background(), &sdk.CallToolParams{
		Name: "create_tree",
		Arguments: map[string]any{
			"items": []any{
				map[string]any{
					"title": "A",
					"text":  "body",
					"children": []any{
						map[string]any{"title": "B"},
					},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if result.IsError {
		t.Fatalf("create_tree error: %s", textContent(t, result))
	}
	if len(captured.Items) != 1 || captured.Items[0].Title != "A" ||
		captured.Items[0].Text != "body" ||
		len(captured.Items[0].Children) != 1 ||
		captured.Items[0].Children[0].Title != "B" {
		t.Fatalf("captured request = %+v", captured)
	}
	var body struct {
		Count int `json:"count"`
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(textContent(t, result)), &body); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if body.Count != 2 || len(body.Items) != 2 || body.Items[0].ID != "it_a" {
		t.Fatalf("result = %+v", body)
	}
}

func TestServerCreateTreeRequiresItems(t *testing.T) {
	server := NewServer(ServerConfig{Backend: fakeItemBackend(t, "x")})
	session := connectTestClient(t, server)

	result, err := session.CallTool(context.Background(), &sdk.CallToolParams{Name: "create_tree"})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if !result.IsError || !strings.Contains(textContent(t, result), `"items"`) {
		t.Fatalf("missing items was accepted: %s", textContent(t, result))
	}
}

// TestServeIOMCPHandshake drives the real MCP initialize and tool-call
// handshake over in-memory pipes, which is the transport RunStdio uses.
func TestServeIOMCPHandshake(t *testing.T) {
	server := NewServer(ServerConfig{
		Backend: fakeItemBackend(t, "Hello"),
		Version: "9.9.9",
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	clientToServerR, clientToServerW := io.Pipe()
	serverToClientR, serverToClientW := io.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = server.ServeIO(ctx, clientToServerR, serverToClientW)
	}()

	client := sdk.NewClient(&sdk.Implementation{Name: "test-client", Version: "1"}, nil)
	session, err := client.Connect(ctx, &sdk.IOTransport{
		Reader: serverToClientR,
		Writer: clientToServerW,
	}, nil)
	if err != nil {
		cancel()
		t.Fatalf("connect: %v", err)
	}
	defer func() {
		_ = session.Close()
		cancel()
	}()

	init := session.InitializeResult()
	if init == nil || init.ServerInfo == nil {
		t.Fatalf("initialize result = %+v", init)
	}
	if init.ServerInfo.Name != "err" || init.ServerInfo.Version != "9.9.9" {
		t.Fatalf("server info = %+v", init.ServerInfo)
	}

	result, err := session.CallTool(ctx, &sdk.CallToolParams{
		Name:      "get_item",
		Arguments: map[string]any{"id": "it_1"},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if result.IsError {
		t.Fatalf("tool error: %s", textContent(t, result))
	}
	var body struct {
		Item struct {
			Title string `json:"title"`
		} `json:"item"`
	}
	if err := json.Unmarshal([]byte(textContent(t, result)), &body); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if body.Item.Title != "Hello" {
		t.Fatalf("item title = %q", body.Item.Title)
	}

	_ = session.Close()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ServeIO did not stop after the client closed")
	}
}

func TestServerStatusTool(t *testing.T) {
	server := NewServer(ServerConfig{Backend: fakeItemBackend(t, "x"), Port: 8787})
	session := connectTestClient(t, server)

	result, err := session.CallTool(context.Background(), &sdk.CallToolParams{Name: "err_status"})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if result.IsError {
		t.Fatalf("status error: %s", textContent(t, result))
	}
	var status map[string]any
	if err := json.Unmarshal([]byte(textContent(t, result)), &status); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if status["app_connected"] != false {
		t.Fatalf("app_connected = %v, want false", status["app_connected"])
	}
	if status["bridge_port"] != float64(8787) {
		t.Fatalf("bridge_port = %v", status["bridge_port"])
	}
	if status["tools"] == float64(0) {
		t.Fatalf("tools = %v", status["tools"])
	}
	if status["bridge_protocol"] != float64(SupportedBridgeProtocol) {
		t.Fatalf("bridge_protocol = %v", status["bridge_protocol"])
	}
	if status["server_version"] != status["version"] {
		t.Fatalf("server_version = %v, version = %v", status["server_version"], status["version"])
	}
	if status["catalog_fingerprint"] != CatalogFingerprint() {
		t.Fatalf("catalog_fingerprint = %v", status["catalog_fingerprint"])
	}
}
