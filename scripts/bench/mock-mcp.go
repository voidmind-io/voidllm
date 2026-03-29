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

		body, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		defer r.Body.Close()

		var req jsonRPCRequest
		if err := json.Unmarshal(body, &req); err != nil {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"jsonrpc":"2.0","id":null,"error":{"code":-32700,"message":"parse error"}}`)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Mcp-Session-Id", sessionID)

		switch req.Method {
		case "initialize":
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":"2025-03-26","capabilities":{"tools":{}},"serverInfo":{"name":"mock-mcp","version":"1.0"}}}`, req.ID)

		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)

		case "tools/list":
			// Replace the hardcoded ID with the actual request ID
			var resp map[string]any
			json.Unmarshal([]byte(toolsListResp), &resp)
			resp["id"] = json.RawMessage(req.ID)
			out, _ := json.Marshal(resp)
			w.Write(out)

		case "tools/call":
			if *latency > 0 {
				time.Sleep(*latency)
			}
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"content":[{"type":"text","text":"{\"status\":\"ok\",\"source\":\"mock\"}"}]}}`, req.ID)

		default:
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"error":{"code":-32601,"message":"method not found: %s"}}`, req.ID, req.Method)
		}
	})

	addr := fmt.Sprintf(":%d", *port)
	fmt.Printf("Mock MCP listening on %s (latency: %v)\n", addr, *latency)
	if err := http.ListenAndServe(addr, nil); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}
