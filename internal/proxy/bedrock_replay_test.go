package proxy

import (
	"bufio"
	"bytes"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
)

func TestBedrockReplay_MaxTokensCapture(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile("testdata/bedrock_converse_stream_max_tokens.bin")
	if err != nil {
		t.Fatal(err)
	}
	r := newEventStreamReader(io.NopCloser(bytes.NewReader(raw)))
	a := &BedrockAdapter{includeUsage: true, responseID: "chatcmpl-1", modelName: "m"}
	sc := bufio.NewScanner(r)
	var emitted []string
	for sc.Scan() {
		out, err := a.TransformStreamLine(sc.Bytes())
		if err != nil {
			t.Fatalf("abort: %v", err)
		}
		for _, o := range out {
			emitted = append(emitted, string(o))
		}
	}
	if sc.Err() != nil {
		t.Fatalf("scanner: %v", sc.Err())
	}
	sawLength := false
	for _, o := range emitted {
		if strings.Contains(o, `"finish_reason":"length"`) {
			sawLength = true
		}
	}
	if !sawLength {
		t.Error("no chunk with finish_reason length emitted")
	}
	lastData := ""
	for i := len(emitted) - 1; i >= 0; i-- {
		if strings.TrimSpace(emitted[i]) != "" {
			lastData = strings.TrimSpace(emitted[i])
			break
		}
	}
	if lastData != "data: [DONE]" {
		t.Errorf("last emitted line = %q, want data: [DONE]", lastData)
	}
	ui := a.StreamUsage()
	if ui.PromptTokens != 7 || ui.CompletionTokens != 10 {
		t.Fatalf("usage = %+v", ui)
	}
}

func TestBedrockHandler_ReplayCaptureMaxTokens(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile("testdata/bedrock_converse_stream_max_tokens.bin")
	if err != nil {
		t.Fatal(err)
	}
	for name, chunk := range map[string]int{"whole": len(raw), "7-byte": 7} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			up := newBHUpstream(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
				w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
				w.WriteHeader(http.StatusOK)
				for i := 0; i < len(raw); i += chunk {
					end := min(i+chunk, len(raw))
					_, _ = w.Write(raw[i:end])
					w.(http.Flusher).Flush()
				}
			})
			env := newBHEnv(t, bhRegistry(t, up.URL))
			resp := env.post(t, `{"model":"claude","stream":true,"stream_options":{"include_usage":true},"max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`)
			if !strings.Contains(resp.Body, `"finish_reason":"length"`) {
				t.Errorf("client body lacks finish_reason length")
			}
			if !strings.HasSuffix(strings.TrimSpace(resp.Body), "data: [DONE]") {
				t.Errorf("client body does not end with [DONE]")
			}
			if !strings.Contains(resp.Body, `"prompt_tokens":7`) || !strings.Contains(resp.Body, `"completion_tokens":10`) {
				t.Errorf("usage chunk wrong")
			}
			if row := env.waitUsage(t, 1); row.Status != 200 || row.Prompt != 7 || row.Completion != 10 {
				t.Errorf("usage row = %+v", row)
			}
		})
	}
}
