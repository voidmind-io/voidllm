// Mock MCP server for benchmarking MCP proxy overhead.
// Speaks JSON-RPC 2.0 over HTTP with configurable latency.
// Usage: go run scripts/bench/mock-mcp.go [-latency 10ms] [-port 9998]
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

type jsonRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
}

func main() {
	latency := flag.Duration("latency", 10*time.Millisecond, "simulated response latency for tools/call")
	port := flag.Int("port", 9998, "listen port")
	flag.Parse()

	sessionID := "bench-session-001"

	toolsListResp := `{"jsonrpc":"2.0","id":1,"result":{"tools":[
		{"name":"mock_tool","description":"A mock tool for benchmarking","inputSchema":{"type":"object","properties":{"input":{"type":"string"}}}},
		{"name":"mock_search","description":"Search mock data","inputSchema":{"type":"object","properties":{"query":{"type":"string"}},"required":["query"]}},
		{"name":"mock_get","description":"Get a mock resource","inputSchema":{"type":"object","properties":{"id":{"type":"string"}},"required":["id"]}},
		{"name":"mock_list","description":"List mock resources","inputSchema":{"type":"object"}},
		{"name":"mock_create","description":"Create a mock resource","inputSchema":{"type":"object","properties":{"name":{"type":"string"}},"required":["name"]}}
	]}}`

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
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

		// Use JSON null when no ID was provided in the request.
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
			if err := json.Unmarshal([]byte(toolsListResp), &resp); err != nil {
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
			if *latency > 0 {
				time.Sleep(*latency)
			}
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"content":[{"type":"text","text":"{\"status\":\"ok\",\"source\":\"mock\"}"}]}}`, id)

		default:
			errResp, err := json.Marshal(map[string]any{
				"jsonrpc": "2.0",
				"id":      id,
				"error": map[string]any{
					"code":    -32601,
					"message": "method not found: " + req.Method,
				},
			})
			if err != nil {
				fmt.Fprintf(os.Stderr, "default: marshal error response: %v\n", err)
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			_, _ = w.Write(errResp)
		}
	})

	addr := fmt.Sprintf(":%d", *port)
	fmt.Printf("Mock MCP listening on %s (latency: %v)\n", addr, *latency)
	if err := http.ListenAndServe(addr, nil); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}
