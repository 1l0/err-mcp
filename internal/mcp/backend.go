// Package mcp exposes the err app's RPC surface to MCP clients through a local
// server.
//
// It is deliberately a proxy: the web app owns the vault (SQLite in OPFS) and
// executes every operation in its Go/WASM engine. The server validates and
// describes the calls, and forwards them over a loopback WebSocket. Nothing in
// this package stores vault data or reimplements the item model.
package mcp

import (
	"context"
	"errors"
	"fmt"
)

// ErrAppNotConnected is returned when no err app is attached to the bridge.
// It is a normal tool error: the model can tell the user to open the app.
var ErrAppNotConnected = errors.New("the err app is not connected; open the web app and enable the MCP bridge")

// RPCError is an error returned by the app's RPC dispatcher. Code is the
// godash/gRPC-style status code (400 for a bad request, 500 for a failure).
type RPCError struct {
	Code    int
	Message string
}

func (e *RPCError) Error() string {
	return fmt.Sprintf("app rpc error %d: %s", e.Code, e.Message)
}

// Backend executes one app RPC. method is the godash path, for example
// "/pb.ItemService/GetItem"; req and the returned bytes are protobuf-encoded
// request and response messages.
//
// Bridge is the production implementation. Tests use a function adapter.
type Backend interface {
	Call(ctx context.Context, method string, req []byte) ([]byte, error)
}

// BackendFunc adapts a function to Backend.
type BackendFunc func(ctx context.Context, method string, req []byte) ([]byte, error)

// Call implements Backend.
func (f BackendFunc) Call(ctx context.Context, method string, req []byte) ([]byte, error) {
	return f(ctx, method, req)
}
