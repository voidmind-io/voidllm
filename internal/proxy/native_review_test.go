package proxy_test

// native_review_test.go covers the review-round behaviors of the native
// endpoints: stateless Responses defaults, stateful parameter rejection,
// usage capture after a client disconnect, fallback access checks, count_tokens
// breaker neutrality, stream_options on passthrough streams and the missing
// usage warning.

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/voidmind-io/voidllm/internal/circuitbreaker"
	"github.com/voidmind-io/voidllm/internal/config"
	"github.com/voidmind-io/voidllm/internal/proxy"
)

func TestNativeReview_ResponsesStoreDefault(t *testing.T) {
	t.Parallel()
	up := jsonUpstream(t, http.StatusOK, responsesJSON(1, 1))
	env := newNHEnv(t, nhOpts{models: []config.ModelConfig{oaiModel("gpt", "openai", up.srv.URL)}})

	tests := []struct {
		name, body, want string
	}{
		{"absent becomes false", `{"model":"gpt","input":"x"}`, `"store":false`},
		{"client true is kept", `{"model":"gpt","input":"x","store":true}`, `"store":true`},
		{"client false is kept", `{"model":"gpt","input":"x","store":false}`, `"store":false`},
	}
	for _, tc := range tests {
		if status, out := env.post(t, "/v1/responses", tc.body); status != http.StatusOK {
			t.Fatalf("%s: status %d body %s", tc.name, status, out)
		}
		if got := up.lastReq().Body; !strings.Contains(got, tc.want) {
			t.Errorf("%s: upstream body %s, want %s", tc.name, got, tc.want)
		}
	}
	if got := up.lastReq().Body; strings.Count(got, `"store"`) != 1 {
		t.Errorf("store duplicated: %s", got)
	}
}

func TestNativeReview_ResponsesRejectsStatefulParams(t *testing.T) {
	t.Parallel()
	up := jsonUpstream(t, http.StatusOK, responsesJSON(1, 1))
	env := newNHEnv(t, nhOpts{models: []config.ModelConfig{oaiModel("gpt", "openai", up.srv.URL)}})

	rejected := []string{
		`{"model":"gpt","input":"x","previous_response_id":"resp_1"}`,
		`{"model":"gpt","input":"x","conversation":"conv_1"}`,
		`{"model":"gpt","input":"x","conversation":{"id":"conv_1"}}`,
		`{"model":"gpt","input":"x","background":true}`,
	}
	for _, b := range rejected {
		status, out := env.post(t, "/v1/responses", b)
		if status != http.StatusBadRequest || !strings.Contains(out, `"code":"unsupported_parameter"`) {
			t.Errorf("%s: status %d body %s, want 400 unsupported_parameter", b, status, out)
		}
	}
	if up.hits.Load() != 0 {
		t.Errorf("rejected requests reached the upstream %d times", up.hits.Load())
	}

	accepted := []string{
		`{"model":"gpt","input":"x","previous_response_id":null}`,
		`{"model":"gpt","input":"x","conversation":null}`,
		`{"model":"gpt","input":"x","background":false}`,
	}
	for _, b := range accepted {
		if status, out := env.post(t, "/v1/responses", b); status != http.StatusOK {
			t.Errorf("%s: status %d body %s, want 200", b, status, out)
		}
	}
}

// TestNativeReview_ClientDisconnectStillCapturesUsage hangs up after the first
// bytes of a stream and expects the terminal usage to be charged anyway.
func TestNativeReview_ClientDisconnectStillCapturesUsage(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name, path, req string
		start           string
		delta           string
		end             string
		wantPrompt      int
		wantCompletion  int
	}{
		{
			name: "messages", path: "/v1/messages",
			req:   `{"model":"m","stream":true,"max_tokens":8,"messages":[]}`,
			start: msgStart,
			delta: "event: content_block_delta\n" + `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"` + strings.Repeat("x", 30000) + `"}}` + "\n\n",
			end:   msgStop, wantPrompt: 7, wantCompletion: 9,
		},
		{
			name: "responses", path: "/v1/responses",
			req:   `{"model":"m","stream":true,"input":"x"}`,
			start: "event: response.created\n" + `data: {"type":"response.created","response":{}}` + "\n\n",
			delta: "event: response.output_text.delta\n" + `data: {"type":"response.output_text.delta","delta":"` + strings.Repeat("x", 30000) + `"}` + "\n\n",
			end:   respCompleted, wantPrompt: 7, wantCompletion: 9,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			up := newNHUpstream(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				fl := w.(http.Flusher)
				fmt.Fprint(w, tc.start)
				fl.Flush()
				time.Sleep(300 * time.Millisecond) // client hangs up meanwhile
				for i := 0; i < 200; i++ {
					fmt.Fprint(w, tc.delta)
					fl.Flush()
				}
				fmt.Fprint(w, tc.end)
				fl.Flush()
			})
			provider := "anthropic"
			if tc.name == "responses" {
				provider = "vllm"
			}
			env := newNHEnv(t, nhOpts{models: []config.ModelConfig{oaiModel("m", provider, up.srv.URL)}})

			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatalf("listen: %v", err)
			}
			go func() { _ = env.app.Listener(ln, fiber.ListenConfig{DisableStartupMessage: true}) }()
			t.Cleanup(func() { _ = env.app.Shutdown() })

			conn, err := net.Dial("tcp", ln.Addr().String())
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			fmt.Fprintf(conn, "POST %s HTTP/1.1\r\nHost: x\r\nContent-Type: application/json\r\nAuthorization: Bearer %s\r\nContent-Length: %d\r\n\r\n%s",
				tc.path, env.key, len(tc.req), tc.req)
			br := bufio.NewReader(conn)
			if _, err := br.ReadString('\n'); err != nil { // status line: stream started
				t.Fatalf("read status line: %v", err)
			}
			_ = conn.Close()

			rows := env.waitRows(t, 1)
			if rows[0].prompt != tc.wantPrompt || rows[0].completion != tc.wantCompletion {
				t.Errorf("usage row = %+v, want prompt %d completion %d", rows[0], tc.wantPrompt, tc.wantCompletion)
			}
		})
	}
}

func TestNativeReview_FallbackStopsAtAccessDeniedTarget(t *testing.T) {
	t.Parallel()

	primary := jsonUpstream(t, http.StatusInternalServerError, `{"type":"error","error":{"type":"api_error","message":"down"}}`)
	denied := jsonUpstream(t, http.StatusOK, `{"never":"called"}`)
	good := jsonUpstream(t, http.StatusOK, messagesJSON(3, 4))

	pm := anthModel("primary", primary.srv.URL)
	pm.Fallback = "fb1"
	dm := oaiModel("fb1", "vllm", denied.srv.URL) // incompatible AND denied
	dm.Fallback = "fb2"
	gm := anthModel("fb2", good.srv.URL)

	env := newNHEnv(t, nhOpts{
		models:        []config.ModelConfig{pm, dm, gm},
		fallbackDepth: 3,
		access: func(keyID string) *proxy.ModelAccessCache {
			c := proxy.NewModelAccessCache()
			c.Load(nil, nil, map[string][]string{keyID: {"primary", "fb2"}})
			return c
		},
	})
	status, body := env.post(t, "/v1/messages", messagesBody("primary", false))
	if status != http.StatusInternalServerError {
		t.Errorf("status %d body %s, want the primary's 500 (chain stops at the denied target)", status, body)
	}
	if good.hits.Load() != 0 || denied.hits.Load() != 0 {
		t.Errorf("fallback targets were called: denied=%d good=%d, want 0", denied.hits.Load(), good.hits.Load())
	}
}

func TestNativeReview_CountTokensDoesNotResetBreaker(t *testing.T) {
	t.Parallel()

	up := newNHUpstream(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/count_tokens") {
			fmt.Fprint(w, `{"input_tokens":1}`)
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"type":"error","error":{"type":"api_error","message":"x"}}`)
	})
	env := newNHEnv(t, nhOpts{
		models:  []config.ModelConfig{anthModel("claude", up.srv.URL)},
		breaker: &circuitbreaker.Config{Enabled: true, Threshold: 2, Timeout: time.Minute, HalfOpenMax: 1},
	})

	if s, _ := env.post(t, "/v1/messages", messagesBody("claude", false)); s != http.StatusInternalServerError {
		t.Fatalf("first generation: status %d", s)
	}
	if s, b := env.post(t, "/v1/messages/count_tokens", `{"model":"claude","messages":[]}`); s != http.StatusOK {
		t.Fatalf("count_tokens: status %d body %s", s, b)
	}
	if s, _ := env.post(t, "/v1/messages", messagesBody("claude", false)); s != http.StatusInternalServerError {
		t.Fatalf("second generation: status %d", s)
	}
	// Two generation failures in a row must have opened the circuit; a success
	// recorded for count_tokens in between would have reset the counter.
	if s, b := env.post(t, "/v1/messages", messagesBody("claude", false)); s != http.StatusServiceUnavailable {
		t.Errorf("third generation: status %d body %s, want 503 circuit open", s, b)
	}
}

func TestNativeReview_StreamOptionsForPassthroughStreams(t *testing.T) {
	t.Parallel()
	up := jsonUpstream(t, http.StatusOK, `{"data":[]}`)
	env := newNHEnv(t, nhOpts{models: []config.ModelConfig{oaiModel("emb", "vllm", up.srv.URL)}})

	if s, b := env.post(t, "/v1/embeddings", `{"model":"emb","input":"x","stream":true}`); s != http.StatusOK {
		t.Fatalf("status %d body %s", s, b)
	}
	if got := up.lastReq().Body; !strings.Contains(got, `"stream_options":{"include_usage":true}`) {
		t.Errorf("stream_options not injected on a passthrough stream: %s", got)
	}
}

func TestNativeReview_WarnsWhenSuccessfulResponseHasNoUsage(t *testing.T) {
	t.Parallel()
	up := jsonUpstream(t, http.StatusOK, `{"id":"msg_1","type":"message","content":[]}`)
	env := newNHEnv(t, nhOpts{models: []config.ModelConfig{anthModel("claude", up.srv.URL)}})

	if s, b := env.post(t, "/v1/messages", messagesBody("claude", false)); s != http.StatusOK {
		t.Fatalf("status %d body %s", s, b)
	}
	out := env.logs.String()
	if strings.Count(out, "native response completed without usage") != 1 ||
		!strings.Contains(out, "model=claude") || !strings.Contains(out, "format=messages") {
		t.Errorf("expected exactly one metadata-only warning, logs:\n%s", out)
	}
}
