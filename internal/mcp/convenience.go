//go:build !js

package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/dynamicpb"
)

// Convenience tools project the existing RPC surface into context-sized
// results. They never replace get_item/list_items: the full read tools stay
// available for export and round-trips.
const (
	defaultListTitlesLimit = 100
	maxListTitlesLimit     = 500

	defaultOutlineDepth = 3
	maxOutlineDepth     = 10
	outlineNodeBudget   = 500

	defaultSearchLimit = 20
	maxSearchLimit     = 200
	searchSnippetRune  = 60
)

// addConvenienceTools registers the discovery helpers next to the generated
// catalog. They are part of the safe tier.
func (s *Server) addConvenienceTools() {
	s.addProjectedTool(
		"list_titles",
		"List live Items as id, title and updated_at only. Cheap discovery for large vaults; use get_item for a body. Sorted by title.",
		map[string]any{
			"type": "object",
			"properties": map[string]any{
				"limit":  map[string]any{"type": "integer", "minimum": 0, "maximum": maxListTitlesLimit, "description": fmt.Sprintf("Maximum Items to return (default %d).", defaultListTitlesLimit)},
				"offset": map[string]any{"type": "integer", "minimum": 0, "description": "Skip this many Items after sorting by title."},
			},
			"additionalProperties": false,
		},
		func(ctx context.Context, args json.RawMessage) (any, error) {
			var in listTitlesArgs
			if err := decodeArgs(args, &in); err != nil {
				return nil, err
			}
			return s.listTitles(ctx, in)
		},
	)

	s.addProjectedTool(
		"outline",
		fmt.Sprintf("Return the tree as nested {id, title, children} without note bodies. Starts at root_id, or at every top-level root when omitted. depth is the number of child levels to expand (default %d, max %d); a truncated node means raise depth or call outline on it. At most %d nodes are returned per call.", defaultOutlineDepth, maxOutlineDepth, outlineNodeBudget),
		map[string]any{
			"type": "object",
			"properties": map[string]any{
				"root_id": map[string]any{"type": "string", "description": "Start Item id. Omit to outline the whole Forest."},
				"depth":   map[string]any{"type": "integer", "minimum": 0, "maximum": maxOutlineDepth, "description": fmt.Sprintf("Child levels to expand (default %d).", defaultOutlineDepth)},
			},
			"additionalProperties": false,
		},
		func(ctx context.Context, args json.RawMessage) (any, error) {
			var in outlineArgs
			if err := decodeArgs(args, &in); err != nil {
				return nil, err
			}
			return s.outline(ctx, in)
		},
	)

	s.addProjectedTool(
		"search_items",
		"Case-insensitive substring search over Item titles and bodies. Returns id, title and a short snippet, never the full body. Use get_item to read a match.",
		map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{"type": "string", "description": "Text to find in a title or body."},
				"limit": map[string]any{"type": "integer", "minimum": 0, "maximum": maxSearchLimit, "description": fmt.Sprintf("Maximum matches to return (default %d).", defaultSearchLimit)},
			},
			"required":             []string{"query"},
			"additionalProperties": false,
		},
		func(ctx context.Context, args json.RawMessage) (any, error) {
			var in searchArgs
			if err := decodeArgs(args, &in); err != nil {
				return nil, err
			}
			return s.searchItems(ctx, in)
		},
	)
}

// addProjectedTool registers a hand-written tool whose result is JSON. The
// generated catalog uses protobuf schemas; these tools project those results.
func (s *Server) addProjectedTool(name, description string, inputSchema map[string]any, fn func(context.Context, json.RawMessage) (any, error)) {
	s.sdk.AddTool(
		&sdk.Tool{
			Name:        name,
			Description: description,
			InputSchema: inputSchema,
			Annotations: &sdk.ToolAnnotations{
				Title:          name,
				ReadOnlyHint:   true,
				IdempotentHint: true,
				OpenWorldHint:  boolPtr(false),
			},
		},
		func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			var args json.RawMessage
			if req != nil && req.Params != nil {
				args = req.Params.Arguments
			}
			start, err := s.beginTool(name, args)
			if err != nil {
				return toolError(fmt.Errorf("%s: %w", name, err)), nil
			}
			out, err := fn(ctx, args)
			if err != nil {
				s.finishTool(name, args, start, err)
				return toolError(fmt.Errorf("%s: %w", name, err)), nil
			}
			body, err := json.MarshalIndent(out, "", "  ")
			if err != nil {
				s.finishTool(name, args, start, err)
				return toolError(fmt.Errorf("%s: encode result: %w", name, err)), nil
			}
			s.finishTool(name, args, start, nil)
			return &sdk.CallToolResult{
				Content:           []sdk.Content{&sdk.TextContent{Text: string(body)}},
				StructuredContent: json.RawMessage(body),
			}, nil
		},
	)
	s.names = append(s.names, name)
}

// domainMessages mirrors the subset of the domain.proto messages the
// convenience tools read and write. They are plain Go structs decoded through
// the embedded descriptor, so this package does not import the app's generated
// protobuf code.
type itemView struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Text      string `json:"text"`
	UpdatedAt string `json:"updated_at"`
}

type listItemsResponse struct {
	Items []*itemView `json:"items"`
}

type getItemRequest struct {
	ID string `json:"id"`
}

type getItemResponse struct {
	Item *itemView `json:"item"`
}

type rootsResponse struct {
	Roots []*itemView `json:"roots"`
}

type childrenRequest struct {
	ParentID string `json:"parent_id"`
}

type childRef struct {
	Item *itemView `json:"item"`
}

type childrenResponse struct {
	Children []*childRef `json:"children"`
}

// callOp executes one app RPC without generated message types: the request is
// encoded from in, the response is decoded into out, both through the dynamic
// messages described by the embedded descriptor set. A nil in or out is
// allowed for empty request or response messages.
func (s *Server) callOp(ctx context.Context, fullName string, in, out any) error {
	method, ok := lookupMethod(fullName)
	if !ok {
		return fmt.Errorf("unknown RPC method %q", fullName)
	}
	req := dynamicpb.NewMessage(method.Input())
	if in != nil {
		body, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
		if err := protojson.Unmarshal(body, req); err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
	}
	raw, err := proto.Marshal(req)
	if err != nil {
		return fmt.Errorf("encode request: %w", err)
	}

	respBytes, err := s.backend.Call(ctx, fullName, raw)
	if err != nil {
		return err
	}

	// Decode through the descriptor so a newer app field the tool ignores does
	// not break the projection.
	resp := dynamicpb.NewMessage(method.Output())
	if err := proto.Unmarshal(respBytes, resp); err != nil {
		return fmt.Errorf("decode app response: %w", err)
	}
	if out == nil {
		return nil
	}
	body, err := protojson.MarshalOptions{UseProtoNames: true}.Marshal(resp)
	if err != nil {
		return fmt.Errorf("decode app response: %w", err)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("decode app response: %w", err)
	}
	return nil
}

type listTitlesArgs struct {
	Limit  int `json:"limit"`
	Offset int `json:"offset"`
}

type titleEntry struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	UpdatedAt string `json:"updated_at"`
}

type listTitlesResult struct {
	Total  int          `json:"total"`
	Offset int          `json:"offset"`
	Items  []titleEntry `json:"items"`
}

func (s *Server) listTitles(ctx context.Context, in listTitlesArgs) (any, error) {
	if in.Offset < 0 {
		return nil, fmt.Errorf("offset must not be negative")
	}
	limit := in.Limit
	if limit < 0 {
		return nil, fmt.Errorf("limit must not be negative")
	}
	if limit == 0 {
		limit = defaultListTitlesLimit
	}
	if limit > maxListTitlesLimit {
		limit = maxListTitlesLimit
	}

	var resp listItemsResponse
	if err := s.callOp(ctx, "/pb.ItemService/ListItems", nil, &resp); err != nil {
		return nil, err
	}
	items := append([]*itemView(nil), resp.Items...)
	sort.SliceStable(items, func(i, j int) bool {
		a, b := strings.ToLower(items[i].Title), strings.ToLower(items[j].Title)
		if a != b {
			return a < b
		}
		return items[i].ID < items[j].ID
	})

	start := min(in.Offset, len(items))
	end := min(start+limit, len(items))
	out := make([]titleEntry, 0, end-start)
	for _, item := range items[start:end] {
		out = append(out, titleEntry{ID: item.ID, Title: item.Title, UpdatedAt: item.UpdatedAt})
	}
	return listTitlesResult{Total: len(items), Offset: start, Items: out}, nil
}

type outlineArgs struct {
	RootID string `json:"root_id"`
	Depth  int    `json:"depth"`
}

type outlineNode struct {
	ID        string         `json:"id"`
	Title     string         `json:"title"`
	Children  []*outlineNode `json:"children,omitempty"`
	Truncated bool           `json:"truncated,omitempty"`
}

type outlineResult struct {
	Roots     []*outlineNode `json:"roots"`
	Truncated bool           `json:"truncated,omitempty"`
}

func (s *Server) outline(ctx context.Context, in outlineArgs) (any, error) {
	depth := in.Depth
	if depth < 0 {
		return nil, fmt.Errorf("depth must not be negative")
	}
	if depth > maxOutlineDepth {
		depth = maxOutlineDepth
	}
	budget := outlineNodeBudget

	if strings.TrimSpace(in.RootID) != "" {
		var resp getItemResponse
		if err := s.callOp(ctx, "/pb.ItemService/GetItem", getItemRequest{ID: in.RootID}, &resp); err != nil {
			return nil, err
		}
		if resp.Item == nil {
			return nil, fmt.Errorf("item %q not found", in.RootID)
		}
		node, err := s.outlineNode(ctx, resp.Item, depth, &budget)
		if err != nil {
			return nil, err
		}
		return outlineResult{Roots: []*outlineNode{node}}, nil
	}

	var roots rootsResponse
	if err := s.callOp(ctx, "/pb.ItemService/Roots", nil, &roots); err != nil {
		return nil, err
	}
	result := outlineResult{Roots: make([]*outlineNode, 0, len(roots.Roots))}
	for _, root := range roots.Roots {
		node, err := s.outlineNode(ctx, root, depth, &budget)
		if err != nil {
			return nil, err
		}
		result.Roots = append(result.Roots, node)
		if budget <= 0 {
			result.Truncated = true
			break
		}
	}
	return result, nil
}

func (s *Server) outlineNode(ctx context.Context, item *itemView, depth int, budget *int) (*outlineNode, error) {
	node := &outlineNode{ID: item.ID, Title: item.Title}
	if *budget <= 0 {
		node.Truncated = true
		return node, nil
	}
	*budget--

	children, err := s.childItems(ctx, item.ID)
	if err != nil {
		return nil, err
	}
	if len(children) == 0 {
		return node, nil
	}
	if depth <= 0 {
		node.Truncated = true
		return node, nil
	}
	for _, child := range children {
		if *budget <= 0 {
			node.Truncated = true
			break
		}
		built, err := s.outlineNode(ctx, child, depth-1, budget)
		if err != nil {
			return nil, err
		}
		node.Children = append(node.Children, built)
	}
	return node, nil
}

func (s *Server) childItems(ctx context.Context, parentID string) ([]*itemView, error) {
	var resp childrenResponse
	if err := s.callOp(ctx, "/pb.ItemService/Children", childrenRequest{ParentID: parentID}, &resp); err != nil {
		return nil, err
	}
	out := make([]*itemView, 0, len(resp.Children))
	for _, child := range resp.Children {
		if child.Item != nil {
			out = append(out, child.Item)
		}
	}
	return out, nil
}

type searchArgs struct {
	Query string `json:"query"`
	Limit int    `json:"limit"`
}

type searchEntry struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Snippet string `json:"snippet"`
}

type searchResult struct {
	Query    string        `json:"query"`
	Total    int           `json:"total"`
	Returned int           `json:"returned"`
	Items    []searchEntry `json:"items"`
}

func (s *Server) searchItems(ctx context.Context, in searchArgs) (any, error) {
	query := strings.TrimSpace(in.Query)
	if query == "" {
		return nil, fmt.Errorf(`missing required argument "query"`)
	}
	limit := in.Limit
	if limit < 0 {
		return nil, fmt.Errorf("limit must not be negative")
	}
	if limit == 0 {
		limit = defaultSearchLimit
	}
	if limit > maxSearchLimit {
		limit = maxSearchLimit
	}

	var resp listItemsResponse
	if err := s.callOp(ctx, "/pb.ItemService/ListItems", nil, &resp); err != nil {
		return nil, err
	}
	needle := strings.ToLower(query)
	matches := make([]searchEntry, 0)
	for _, item := range resp.Items {
		if !containsFold(item.Title, needle) && !containsFold(item.Text, needle) {
			continue
		}
		matches = append(matches, searchEntry{
			ID:      item.ID,
			Title:   item.Title,
			Snippet: snippet(item.Text, query, searchSnippetRune),
		})
	}
	sort.SliceStable(matches, func(i, j int) bool {
		a, b := strings.ToLower(matches[i].Title), strings.ToLower(matches[j].Title)
		if a != b {
			return a < b
		}
		return matches[i].ID < matches[j].ID
	})
	total := len(matches)
	if len(matches) > limit {
		matches = matches[:limit]
	}
	return searchResult{Query: query, Total: total, Returned: len(matches), Items: matches}, nil
}

func containsFold(s, lowerNeedle string) bool {
	return strings.Contains(strings.ToLower(s), lowerNeedle)
}

// snippet returns a short excerpt around the first query match in text. It
// collapses whitespace and uses rune offsets so Japanese is never cut mid-rune.
func snippet(text, query string, radius int) string {
	text = strings.Join(strings.Fields(text), " ")
	if text == "" {
		return ""
	}
	runes := []rune(text)
	start := 0
	queryRunes := []rune(strings.ToLower(query))
	if index := indexRunes([]rune(strings.ToLower(text)), queryRunes); index >= 0 {
		start = max(0, index-radius)
	}
	end := min(len(runes), start+len(queryRunes)+radius*2)
	out := string(runes[start:end])
	if start > 0 {
		out = "…" + out
	}
	if end < len(runes) {
		out += "…"
	}
	return out
}

func indexRunes(haystack, needle []rune) int {
	if len(needle) == 0 {
		return 0
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		match := true
		for j := range needle {
			if haystack[i+j] != needle[j] {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}

func decodeArgs(raw json.RawMessage, v any) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("invalid arguments: %w", err)
	}
	return nil
}
