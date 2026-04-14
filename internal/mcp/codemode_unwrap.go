package mcp

import "github.com/voidmind-io/voidllm/internal/jsonx"

// unwrapToolResult strips the MCP ToolResult wrapper when the response matches
// the expected shape. Returns raw unchanged when the response does not match
// the ToolResult wrapper shape, providing a defensive fallback for arbitrary payloads.
func unwrapToolResult(raw jsonx.RawMessage) jsonx.RawMessage {
	// Phase 1: inspect top-level keys to confirm the MCP ToolResult shape.
	var topLevel map[string]jsonx.RawMessage
	if err := jsonx.Unmarshal(raw, &topLevel); err != nil {
		return raw
	}

	if _, hasContent := topLevel["content"]; !hasContent {
		return raw
	}

	// Only unwrap when every top-level key is one of the known ToolResult fields.
	// Any extra key means this is payload data, not a wrapper.
	knownKeys := 0
	for k := range topLevel {
		switch k {
		case "content", "isError", "structuredContent", "_meta":
			knownKeys++
		}
	}
	if knownKeys != len(topLevel) {
		return raw
	}

	// Phase 2: parse the content array.
	var tr struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := jsonx.Unmarshal(raw, &tr); err != nil || len(tr.Content) == 0 {
		return raw
	}

	// Phase 3: extract the payload.
	if len(tr.Content) == 1 && tr.Content[0].Type == "text" {
		text := tr.Content[0].Text
		if jsonx.Valid([]byte(text)) {
			return jsonx.RawMessage(text)
		}
		quoted, err := jsonx.Marshal(text)
		if err != nil {
			return raw
		}
		return jsonx.RawMessage(quoted)
	}

	texts := make([]string, 0, len(tr.Content))
	for _, c := range tr.Content {
		if c.Type == "text" {
			texts = append(texts, c.Text)
		}
	}
	if len(texts) == 0 {
		return raw
	}
	out, err := jsonx.Marshal(texts)
	if err != nil {
		return raw
	}
	return jsonx.RawMessage(out)
}
