//go:build !js

package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestEndToEndRelay connects a fake app to the bridge and an in-memory MCP
// client to the server, then drives an item call through the whole path.
func TestEndToEndRelay(t *testing.T) {
	const token = "test-token"
	bridge := NewBridge(BridgeConfig{
		Token:               token,
		EnforceLoopbackHost: true,
		Logger:              testLogger(),
	})
	defer bridge.Close()
	server := NewServer(ServerConfig{
		Backend: bridge,
		Bridge:  bridge,
		Port:    8787,
	})

	mux := http.NewServeMux()
	mux.Handle("/app/ws", bridge)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	// Fake app: answer every request with a fixed GetItemResponse.
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/app/ws?token=" + token
	app, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial app: %v (resp %v)", err, resp)
	}
	defer app.Close()
	stopApp := make(chan struct{})
	defer close(stopApp)
	go func() {
		for {
			_, data, err := app.ReadMessage()
			if err != nil {
				return
			}
			var frame bridgeFrame
			if err := json.Unmarshal(data, &frame); err != nil {
				return
			}
			if frame.Type != "request" {
				continue
			}
			var in struct {
				ID string `json:"id"`
			}
			decodeProto(t, "pb.GetItemRequest", frame.Payload, &in)
			out := encodeProto(t, "pb.GetItemResponse",
				fmt.Sprintf(`{"item":{"id":%q,"title":"Relayed"}}`, in.ID))
			select {
			case <-stopApp:
				return
			default:
			}
			_ = app.WriteJSON(bridgeFrame{Type: "response", ID: frame.ID, Payload: out})
		}
	}()
	waitConnected(t, bridge)

	session := connectTestClient(t, server)
	ctx := context.Background()

	result, err := session.CallTool(ctx, &sdk.CallToolParams{
		Name:      "get_item",
		Arguments: map[string]any{"id": "it_relay"},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if result.IsError {
		t.Fatalf("tool error: %s", textContent(t, result))
	}
	text := textContent(t, result)
	if !strings.Contains(text, `"Relayed"`) || !strings.Contains(text, `"it_relay"`) {
		t.Fatalf("result = %s", text)
	}

	statusResult, err := session.CallTool(ctx, &sdk.CallToolParams{Name: "err_status"})
	if err != nil {
		t.Fatalf("err_status: %v", err)
	}
	var status map[string]any
	if err := json.Unmarshal([]byte(textContent(t, statusResult)), &status); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if status["app_connected"] != true {
		t.Fatalf("app_connected = %v, want true", status["app_connected"])
	}
}

// TestEndToEndCreateTree relays the bulk-import tool through the real bridge
// frame protocol and checks the nested request survives the trip.
func TestEndToEndCreateTree(t *testing.T) {
	const token = "test-token"
	bridge := NewBridge(BridgeConfig{
		Token:               token,
		EnforceLoopbackHost: true,
		Logger:              testLogger(),
	})
	defer bridge.Close()
	server := NewServer(ServerConfig{
		Backend: bridge,
		Bridge:  bridge,
		Port:    8789,
	})

	mux := http.NewServeMux()
	mux.Handle("/app/ws", bridge)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/app/ws?token=" + token
	app, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial app: %v (resp %v)", err, resp)
	}
	defer app.Close()

	requests := make(chan testTreeRequest, 1)
	go func() {
		for {
			_, data, err := app.ReadMessage()
			if err != nil {
				return
			}
			var frame bridgeFrame
			if err := json.Unmarshal(data, &frame); err != nil {
				return
			}
			if frame.Type != "request" {
				continue
			}
			var in testTreeRequest
			decodeProto(t, "pb.CreateTreeRequest", frame.Payload, &in)
			requests <- in
			out := encodeProto(t, "pb.CreateTreeResponse",
				`{"count":2,"items":[{"id":"it_a","title":"A"},{"id":"it_b","title":"B"}]}`)
			_ = app.WriteJSON(bridgeFrame{Type: "response", ID: frame.ID, Payload: out})
		}
	}()
	waitConnected(t, bridge)

	session := connectTestClient(t, server)
	result, err := session.CallTool(context.Background(), &sdk.CallToolParams{
		Name: "create_tree",
		Arguments: map[string]any{
			"items": []any{
				map[string]any{
					"title": "A",
					"children": []any{
						map[string]any{"title": "B"},
					},
				},
			},
			"parent_id": "it_parent",
		},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if result.IsError {
		t.Fatalf("tool error: %s", textContent(t, result))
	}

	got := <-requests
	if len(got.Items) != 1 || got.Items[0].Title != "A" ||
		len(got.Items[0].Children) != 1 ||
		got.Items[0].Children[0].Title != "B" ||
		got.ParentID != "it_parent" {
		t.Fatalf("relayed request = %+v", got)
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
