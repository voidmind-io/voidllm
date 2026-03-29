package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

// mockMCPServer is a minimal MCP server (JSON-RPC 2.0) for benchmarking.
// It supports initialize, tools/list, and tools/call with configurable delay.
type mockMCPServer struct {
	srv     *http.Server
	addr    string
	latency time.Duration
}

type jsonRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
}

// startMockMCP starts an MCP-compatible mock on a random port.
func startMockMCP(latency time.Duration) (*mockMCPServer, error) {
	sessionID := "bench-session-001"

	toolsListResp, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      0,
		"result": map[string]any{
			"tools": []map[string]any{
				{"name": "mock_tool", "description": "A mock tool", "inputSchema": map[string]any{"type": "object"}},
				{"name": "mock_search", "description": "Search mock data", "inputSchema": map[string]any{"type": "object"}},
				{"name": "mock_get", "description": "Get a resource", "inputSchema": map[string]any{"type": "object"}},
				{"name": "mock_list", "description": "List resources", "inputSchema": map[string]any{"type": "object"}},
				{"name": "mock_create", "description": "Create a resource", "inputSchema": map[string]any{"type": "object"}},
			},
		},
	})

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		defer r.Body.Close()
		body, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		var req jsonRPCRequest
		if err := json.Unmarshal(body, &req); err != nil {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"jsonrpc":"2.0","id":null,"error":{"code":-32700,"message":"parse error"}}`)
			return
		}

		id := req.ID
		if len(id) == 0 {
			id = json.RawMessage("null")
		}

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Mcp-Session-Id", sessionID)

		switch req.Method {
		case "initialize":
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":"2025-03-26","capabilities":{"tools":{}},"serverInfo":{"name":"mock-mcp","version":"1.0"}}}`, id)

		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)

		case "tools/list":
			var resp map[string]any
			if err := json.Unmarshal(toolsListResp, &resp); err != nil {
				fmt.Fprintf(os.Stderr, "tools/list: unmarshal: %v\n", err)
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			resp["id"] = id
			out, err := json.Marshal(resp)
			if err != nil {
				fmt.Fprintf(os.Stderr, "tools/list: marshal: %v\n", err)
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			_, _ = w.Write(out)

		case "tools/call":
			if latency > 0 {
				time.Sleep(latency)
			}
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"content":[{"type":"text","text":"{\"status\":\"ok\",\"source\":\"mock\"}"}]}}`, id)

		default:
			errResp, err := json.Marshal(map[string]any{
				"jsonrpc": "2.0",
				"id":      id,
				"error":   map[string]any{"code": -32601, "message": "method not found: " + req.Method},
			})
			if err != nil {
				fmt.Fprintf(os.Stderr, "default: marshal: %v\n", err)
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			_, _ = w.Write(errResp)
		}
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("mock mcp: listen: %w", err)
	}

	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)

	addr := ln.Addr().String()
	if !strings.HasPrefix(addr, "http") {
		addr = "http://" + addr
	}

	return &mockMCPServer{srv: srv, addr: addr, latency: latency}, nil
}

func (m *mockMCPServer) Close() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	m.srv.Shutdown(ctx)
}

func (m *mockMCPServer) URL() string {
	return m.addr
}
