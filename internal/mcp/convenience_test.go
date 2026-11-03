//go:build !js

package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// treeBackend serves a small vault for the convenience-tool tests.
//
//	root1 Alpha
//	  kid1 Apple child
//	  kid2 Kiwi
//	root2 Bravo
func treeBackend(t *testing.T) Backend {
	t.Helper()
	longText := "first root with apples " + strings.Repeat("filler ", 60) + "TAIL-MARKER"
	items := []testItem{
		{ID: "root1", Title: "Alpha", Text: longText, UpdatedAt: "1"},
		{ID: "root2", Title: "Bravo", Text: "second root", UpdatedAt: "2"},
		{ID: "kid1", Title: "Apple child", Text: "about 東京 and apples", UpdatedAt: "3"},
		{ID: "kid2", Title: "Kiwi", Text: "kiwi body", UpdatedAt: "4"},
	}
	byID := make(map[string]testItem, len(items))
	for _, item := range items {
		byID[item.ID] = item
	}
	children := map[string][]string{
		"root1": {"kid1", "kid2"},
	}
	roots := []string{"root1", "root2"}

	return BackendFunc(func(_ context.Context, method string, req []byte) ([]byte, error) {
		switch method {
		case "/pb.ItemService/ListItems":
			return encodeProto(t, "pb.ListItemsResponse", mustJSON(t, map[string]any{"items": items})), nil
		case "/pb.ItemService/GetItem":
			var in struct {
				ID string `json:"id"`
			}
			decodeProto(t, "pb.GetItemRequest", req, &in)
			item, ok := byID[in.ID]
			if !ok {
				return nil, &RPCError{Code: 404, Message: "item not found"}
			}
			return encodeProto(t, "pb.GetItemResponse", mustJSON(t, map[string]any{"item": item})), nil
		case "/pb.ItemService/Roots":
			out := make([]testItem, 0, len(roots))
			for _, id := range roots {
				out = append(out, byID[id])
			}
			return encodeProto(t, "pb.RootsResponse", mustJSON(t, map[string]any{"roots": out})), nil
		case "/pb.ItemService/Children":
			var in struct {
				ParentID string `json:"parent_id"`
			}
			decodeProto(t, "pb.ChildrenRequest", req, &in)
			out := make([]map[string]any, 0)
			for _, id := range children[in.ParentID] {
				out = append(out, map[string]any{"item": byID[id]})
			}
			return encodeProto(t, "pb.ChildrenResponse", mustJSON(t, map[string]any{"children": out})), nil
		default:
			return nil, &RPCError{Code: 404, Message: "unexpected method " + method}
		}
	})
}

func callTool(t *testing.T, session *sdk.ClientSession, name string, args map[string]any) (string, bool) {
	t.Helper()
	result, err := session.CallTool(context.Background(), &sdk.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("CallTool %s: %v", name, err)
	}
	return textContent(t, result), result.IsError
}

func TestListTitlesProjectsAndPaginates(t *testing.T) {
	server := NewServer(ServerConfig{Backend: treeBackend(t)})
	session := connectTestClient(t, server)

	text, isErr := callTool(t, session, "list_titles", map[string]any{"limit": 1})
	if isErr {
		t.Fatalf("list_titles: %s", text)
	}
	var body struct {
		Total  int `json:"total"`
		Offset int `json:"offset"`
		Items  []struct {
			ID        string `json:"id"`
			Title     string `json:"title"`
			UpdatedAt string `json:"updated_at"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(text), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Total != 4 || len(body.Items) != 1 {
		t.Fatalf("total=%d items=%d, want 4 and 1", body.Total, len(body.Items))
	}
	if body.Items[0].Title != "Alpha" || body.Items[0].ID != "root1" {
		t.Fatalf("first item = %+v", body.Items[0])
	}
	if strings.Contains(text, "first root with apples") {
		t.Fatalf("list_titles leaked body text: %s", text)
	}

	text, isErr = callTool(t, session, "list_titles", map[string]any{"offset": 1, "limit": 1})
	if isErr {
		t.Fatalf("list_titles offset: %s", text)
	}
	if err := json.Unmarshal([]byte(text), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Offset != 1 || len(body.Items) != 1 || body.Items[0].Title != "Apple child" {
		t.Fatalf("offset page = %+v", body)
	}
}

func TestOutlineDepthAndTruncation(t *testing.T) {
	server := NewServer(ServerConfig{Backend: treeBackend(t)})
	session := connectTestClient(t, server)

	text, isErr := callTool(t, session, "outline", map[string]any{"root_id": "root1", "depth": 0})
	if isErr {
		t.Fatalf("outline depth 0: %s", text)
	}
	var body struct {
		Roots []struct {
			ID        string `json:"id"`
			Title     string `json:"title"`
			Truncated bool   `json:"truncated"`
			Children  []struct {
				ID    string `json:"id"`
				Title string `json:"title"`
			} `json:"children"`
		} `json:"roots"`
	}
	if err := json.Unmarshal([]byte(text), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Roots) != 1 || len(body.Roots[0].Children) != 0 || !body.Roots[0].Truncated {
		t.Fatalf("depth 0 outline = %+v", body)
	}

	text, isErr = callTool(t, session, "outline", map[string]any{"root_id": "root1", "depth": 1})
	if isErr {
		t.Fatalf("outline depth 1: %s", text)
	}
	if err := json.Unmarshal([]byte(text), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Roots) != 1 || len(body.Roots[0].Children) != 2 {
		t.Fatalf("depth 1 outline = %+v", body)
	}
	if body.Roots[0].Children[0].ID != "kid1" || body.Roots[0].Children[1].ID != "kid2" {
		t.Fatalf("children = %+v", body.Roots[0].Children)
	}
	if strings.Contains(text, "about 東京") {
		t.Fatalf("outline leaked body text: %s", text)
	}

	text, isErr = callTool(t, session, "outline", map[string]any{"depth": 0})
	if isErr {
		t.Fatalf("outline forest: %s", text)
	}
	if err := json.Unmarshal([]byte(text), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Roots) != 2 {
		t.Fatalf("forest roots = %d, want 2", len(body.Roots))
	}
}

func TestSearchItemsFindsEnglishAndJapanese(t *testing.T) {
	server := NewServer(ServerConfig{Backend: treeBackend(t)})
	session := connectTestClient(t, server)

	text, isErr := callTool(t, session, "search_items", map[string]any{"query": "APPLE"})
	if isErr {
		t.Fatalf("search: %s", text)
	}
	var body struct {
		Total    int `json:"total"`
		Returned int `json:"returned"`
		Items    []struct {
			ID      string `json:"id"`
			Title   string `json:"title"`
			Snippet string `json:"snippet"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(text), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Total != 2 || body.Returned != 2 {
		t.Fatalf("apple matches = %+v", body)
	}
	if body.Items[0].Title != "Alpha" || body.Items[1].Title != "Apple child" {
		t.Fatalf("match order = %+v", body.Items)
	}
	if strings.Contains(text, "TAIL-MARKER") {
		t.Fatalf("search leaked the full body: %s", text)
	}
	if !strings.Contains(body.Items[1].Snippet, "東京") {
		t.Fatalf("snippet = %q, want the 東京 excerpt", body.Items[1].Snippet)
	}

	text, isErr = callTool(t, session, "search_items", map[string]any{"query": "東京"})
	if isErr {
		t.Fatalf("japanese search: %s", text)
	}
	if err := json.Unmarshal([]byte(text), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Total != 1 || body.Items[0].ID != "kid1" {
		t.Fatalf("japanese matches = %+v", body)
	}
}

func TestConvenienceToolsRejectBadArguments(t *testing.T) {
	server := NewServer(ServerConfig{Backend: treeBackend(t)})
	session := connectTestClient(t, server)

	if text, isErr := callTool(t, session, "list_titles", map[string]any{"limit": 1, "bogus": true}); !isErr {
		t.Fatalf("unknown field was accepted: %s", text)
	}
	if text, isErr := callTool(t, session, "search_items", map[string]any{"query": "  "}); !isErr {
		t.Fatalf("empty query was accepted: %s", text)
	}
	if text, isErr := callTool(t, session, "outline", map[string]any{"depth": -1}); !isErr {
		t.Fatalf("negative depth was accepted: %s", text)
	}
}
