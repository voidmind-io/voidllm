package health_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/voidmind-io/voidllm/internal/health"
)

// readBody reads and JSON-decodes req.Body into a map. It fails the test if
// the request has no body or the body is not valid JSON.
func readBody(t *testing.T, req *http.Request) map[string]any {
	t.Helper()
	if req.Body == nil {
		t.Fatal("request has no body")
	}
	raw, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal body %q: %v", raw, err)
	}
	return doc
}

// TestBuildProbeRequest_ChatByProvider verifies that BuildProbeRequest builds
// a provider-correct URL, headers, and body for a chat probe against every
// supported provider. This is the core assertion the provider-aware probe
// work (issue #182) exists to guarantee: each provider is probed the same
// way the real proxy hot path would talk to it.
func TestBuildProbeRequest_ChatByProvider(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// target is the ProbeTarget under test.
		target health.ProbeTarget
		// wantURL is the exact expected request URL.
		wantURL string
		// wantMethod is the exact expected HTTP method.
		wantMethod string
		// wantHeaders lists header/value pairs that must be present exactly.
		wantHeaders map[string]string
		// wantHeadersAbsent lists headers that must NOT be set (empty value).
		wantHeadersAbsent []string
		// checkBody, when non-nil, receives the JSON-decoded request body.
		checkBody func(t *testing.T, body map[string]any)
	}{
		{
			name: "anthropic",
			target: health.ProbeTarget{
				ModelName: "claude-3-opus",
				Provider:  "anthropic",
				BaseURL:   "https://api.anthropic.com",
				APIKey:    "sk-ant-test-key",
			},
			wantURL:    "https://api.anthropic.com/v1/messages",
			wantMethod: http.MethodPost,
			wantHeaders: map[string]string{
				"x-api-key":         "sk-ant-test-key",
				"anthropic-version": "2023-06-01",
			},
			wantHeadersAbsent: []string{"Authorization"},
			checkBody: func(t *testing.T, body map[string]any) {
				t.Helper()
				if _, ok := body["messages"]; !ok {
					t.Error("body missing \"messages\"")
				}
				if _, ok := body["max_tokens"]; !ok {
					t.Error("body missing \"max_tokens\" (Anthropic requires it)")
				}
				msgs, ok := body["messages"].([]any)
				if !ok || len(msgs) != 1 {
					t.Fatalf("messages = %#v, want a single-element array", body["messages"])
				}
				msg, ok := msgs[0].(map[string]any)
				if !ok {
					t.Fatalf("messages[0] = %#v, want an object", msgs[0])
				}
				if msg["role"] != "user" {
					t.Errorf("messages[0].role = %v, want %q", msg["role"], "user")
				}
				// Anthropic's Messages API represents content as an array of
				// typed blocks, not a bare string — this is the concrete
				// structural difference from an OpenAI chat body.
				if _, ok := msg["content"].([]any); !ok {
					t.Errorf("messages[0].content = %#v (%T), want a content-block array (Messages-shaped, not OpenAI-shaped)", msg["content"], msg["content"])
				}
			},
		},
		{
			name: "gemini",
			target: health.ProbeTarget{
				ModelName: "gemini-1.5-pro",
				Provider:  "gemini",
				BaseURL:   "https://generativelanguage.googleapis.com",
				APIKey:    "goog-test-key",
			},
			wantURL:    "https://generativelanguage.googleapis.com/v1beta/models/gemini-1.5-pro:generateContent",
			wantMethod: http.MethodPost,
			wantHeaders: map[string]string{
				"x-goog-api-key": "goog-test-key",
			},
			wantHeadersAbsent: []string{"Authorization"},
			checkBody: func(t *testing.T, body map[string]any) {
				t.Helper()
				if _, ok := body["messages"]; ok {
					t.Error("body has OpenAI-shaped \"messages\" field; want Gemini-shaped \"contents\"")
				}
				contents, ok := body["contents"].([]any)
				if !ok || len(contents) == 0 {
					t.Fatalf("contents = %#v, want a non-empty array", body["contents"])
				}
			},
		},
		{
			name: "azure",
			target: health.ProbeTarget{
				ModelName:       "gpt-4o",
				Provider:        "azure",
				BaseURL:         "https://myres.openai.azure.com",
				APIKey:          "azure-test-key",
				AzureDeployment: "gpt4-deployment",
			},
			wantURL:    "https://myres.openai.azure.com/openai/deployments/gpt4-deployment/chat/completions?api-version=2024-10-21",
			wantMethod: http.MethodPost,
			wantHeaders: map[string]string{
				"api-key": "azure-test-key",
			},
			wantHeadersAbsent: []string{"Authorization"},
			checkBody: func(t *testing.T, body map[string]any) {
				t.Helper()
				// The probe body's model field carries the Azure deployment
				// name, mirroring the real proxy path's substitution.
				if body["model"] != "gpt4-deployment" {
					t.Errorf("body.model = %v, want %q", body["model"], "gpt4-deployment")
				}
			},
		},
		{
			name: "azure with explicit api version",
			target: health.ProbeTarget{
				ModelName:       "gpt-4o",
				Provider:        "azure",
				BaseURL:         "https://myres.openai.azure.com",
				APIKey:          "azure-test-key",
				AzureDeployment: "gpt4-deployment",
				AzureAPIVersion: "2023-05-15",
			},
			wantURL:    "https://myres.openai.azure.com/openai/deployments/gpt4-deployment/chat/completions?api-version=2023-05-15",
			wantMethod: http.MethodPost,
			wantHeaders: map[string]string{
				"api-key": "azure-test-key",
			},
			wantHeadersAbsent: []string{"Authorization"},
		},
		{
			name: "vertex",
			target: health.ProbeTarget{
				ModelName:   "gemini-1.5-pro",
				Provider:    "vertex",
				BaseURL:     "https://us-central1-aiplatform.googleapis.com",
				APIKey:      "vertex-bearer-token",
				GCPProject:  "proj-123",
				GCPLocation: "us-central1",
			},
			wantURL:    "https://us-central1-aiplatform.googleapis.com/v1/projects/proj-123/locations/us-central1/publishers/google/models/gemini-1.5-pro:generateContent",
			wantMethod: http.MethodPost,
			// The trap: unlike every other provider, Vertex authenticates
			// with the default Bearer Authorization header that is set
			// BEFORE the adapter runs — GeminiAdapter.SetHeaders
			// deliberately leaves it untouched for provider "vertex".
			wantHeaders: map[string]string{
				"Authorization": "Bearer vertex-bearer-token",
			},
			checkBody: func(t *testing.T, body map[string]any) {
				t.Helper()
				if _, ok := body["contents"].([]any); !ok {
					t.Errorf("contents = %#v, want a non-empty array", body["contents"])
				}
			},
		},
		{
			name: "openai",
			target: health.ProbeTarget{
				ModelName: "gpt-4",
				Provider:  "openai",
				BaseURL:   "https://api.openai.com/v1",
				APIKey:    "sk-openai-test-key",
			},
			wantURL:    "https://api.openai.com/v1/chat/completions",
			wantMethod: http.MethodPost,
			wantHeaders: map[string]string{
				"Authorization": "Bearer sk-openai-test-key",
			},
			checkBody: func(t *testing.T, body map[string]any) {
				t.Helper()
				if body["model"] != "gpt-4" {
					t.Errorf("body.model = %v, want %q", body["model"], "gpt-4")
				}
				if _, ok := body["messages"]; !ok {
					t.Error("body missing \"messages\"")
				}
			},
		},
		{
			name: "vllm",
			target: health.ProbeTarget{
				ModelName: "llama-3-70b",
				Provider:  "vllm",
				BaseURL:   "http://localhost:8000/v1",
			},
			wantURL:           "http://localhost:8000/v1/chat/completions",
			wantMethod:        http.MethodPost,
			wantHeadersAbsent: []string{"Authorization"},
		},
		{
			name: "custom (unknown provider treated as passthrough)",
			target: health.ProbeTarget{
				ModelName: "custom-model",
				Provider:  "custom",
				BaseURL:   "http://localhost:9000",
				APIKey:    "custom-key",
			},
			wantURL:    "http://localhost:9000/chat/completions",
			wantMethod: http.MethodPost,
			wantHeaders: map[string]string{
				"Authorization": "Bearer custom-key",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			req, err := health.BuildProbeRequest(context.Background(), health.IntentChat, tc.target)
			if err != nil {
				t.Fatalf("BuildProbeRequest: %v", err)
			}

			if req.URL.String() != tc.wantURL {
				t.Errorf("URL = %q, want %q", req.URL.String(), tc.wantURL)
			}
			if req.Method != tc.wantMethod {
				t.Errorf("Method = %q, want %q", req.Method, tc.wantMethod)
			}
			for header, want := range tc.wantHeaders {
				if got := req.Header.Get(header); got != want {
					t.Errorf("header %q = %q, want %q", header, got, want)
				}
			}
			for _, header := range tc.wantHeadersAbsent {
				if got := req.Header.Get(header); got != "" {
					t.Errorf("header %q = %q, want absent", header, got)
				}
			}
			if tc.checkBody != nil {
				tc.checkBody(t, readBody(t, req))
			}
		})
	}
}

// TestBuildProbeRequest_Vertex_UsesBearerAuthorization is a focused,
// standalone assertion of the Vertex authentication trap: SetHeaders for
// provider "vertex" is a deliberate no-op, so BuildProbeRequest's own
// default Bearer Authorization header (set before the adapter runs) is what
// actually authenticates the probe. This must never be assumed away by a
// future refactor.
func TestBuildProbeRequest_Vertex_UsesBearerAuthorization(t *testing.T) {
	t.Parallel()

	req, err := health.BuildProbeRequest(context.Background(), health.IntentChat, health.ProbeTarget{
		ModelName:   "gemini-1.5-flash",
		Provider:    "vertex",
		BaseURL:     "https://us-central1-aiplatform.googleapis.com",
		APIKey:      "vertex-token",
		GCPProject:  "proj-xyz",
		GCPLocation: "us-central1",
	})
	if err != nil {
		t.Fatalf("BuildProbeRequest: %v", err)
	}

	got := req.Header.Get("Authorization")
	want := "Bearer vertex-token"
	if got != want {
		t.Errorf("Authorization = %q, want %q", got, want)
	}
}

// TestBuildProbeRequest_ModelsList_Applicability verifies which providers
// have a meaningful models-list probe and, where applicable, that the built
// request targets the right endpoint.
func TestBuildProbeRequest_ModelsList_Applicability(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		provider    string
		wantErr     error
		wantURLHas  string
		wantURLTail string
	}{
		{name: "azure not applicable", provider: "azure", wantErr: health.ErrProbeNotApplicable},
		{name: "vertex not applicable", provider: "vertex", wantErr: health.ErrProbeNotApplicable},
		{name: "gemini not applicable", provider: "gemini", wantErr: health.ErrProbeNotApplicable},
		{name: "anthropic applicable", provider: "anthropic", wantURLTail: "/models"},
		{name: "openai applicable", provider: "openai", wantURLTail: "/models"},
		{name: "vllm applicable", provider: "vllm", wantURLTail: "/models"},
		{name: "custom applicable", provider: "custom", wantURLTail: "/models"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			target := health.ProbeTarget{
				ModelName:   "some-model",
				Provider:    tc.provider,
				BaseURL:     "https://upstream.example.com",
				APIKey:      "key",
				GCPProject:  "proj",
				GCPLocation: "us-central1",
			}
			req, err := health.BuildProbeRequest(context.Background(), health.IntentModelsList, target)

			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				if req != nil {
					t.Error("request must be nil when ErrProbeNotApplicable is returned")
				}
				return
			}

			if err != nil {
				t.Fatalf("BuildProbeRequest: %v", err)
			}
			if req.Method != http.MethodGet {
				t.Errorf("Method = %q, want GET", req.Method)
			}
			if !strings.HasSuffix(req.URL.String(), tc.wantURLTail) {
				t.Errorf("URL = %q, want suffix %q", req.URL.String(), tc.wantURLTail)
			}
			if req.Body != nil {
				t.Error("models-list probe must not have a request body")
			}
		})
	}
}

// TestBuildProbeRequest_ModelsList_AnthropicUsesV1Prefix verifies that a
// models-list probe against provider "anthropic" requests the documented
// Anthropic endpoint /v1/models — not the OpenAI-style /models path that a
// naive base+"/models" concatenation would produce. The real Anthropic base
// URL (https://api.anthropic.com) carries no /v1 segment itself, so this
// prefix only appears in the built request if AnthropicAdapter.TransformURL
// maps the "models" path explicitly (see internal/proxy/anthropic.go).
func TestBuildProbeRequest_ModelsList_AnthropicUsesV1Prefix(t *testing.T) {
	t.Parallel()

	target := health.ProbeTarget{
		Provider: "anthropic",
		BaseURL:  "https://api.anthropic.com",
		APIKey:   "key",
	}
	req, err := health.BuildProbeRequest(context.Background(), health.IntentModelsList, target)
	if err != nil {
		t.Fatalf("BuildProbeRequest: %v", err)
	}
	if !strings.HasSuffix(req.URL.String(), "/v1/models") {
		t.Errorf("URL = %q, want suffix %q", req.URL.String(), "/v1/models")
	}
}

// TestBuildProbeRequest_Embeddings_Applicability verifies which providers
// have a meaningful embeddings probe and, where applicable, that the built
// request carries an embeddings-shaped body.
func TestBuildProbeRequest_Embeddings_Applicability(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		provider string
		wantErr  error
	}{
		{name: "anthropic not applicable", provider: "anthropic", wantErr: health.ErrProbeNotApplicable},
		{name: "gemini not applicable", provider: "gemini", wantErr: health.ErrProbeNotApplicable},
		{name: "vertex not applicable", provider: "vertex", wantErr: health.ErrProbeNotApplicable},
		{name: "azure applicable", provider: "azure"},
		{name: "openai applicable", provider: "openai"},
		{name: "vllm applicable", provider: "vllm"},
		{name: "custom applicable", provider: "custom"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			target := health.ProbeTarget{
				ModelName:       "embed-model",
				Provider:        tc.provider,
				BaseURL:         "https://upstream.example.com",
				APIKey:          "key",
				AzureDeployment: "embed-deployment",
				GCPProject:      "proj",
				GCPLocation:     "us-central1",
			}
			req, err := health.BuildProbeRequest(context.Background(), health.IntentEmbeddings, target)

			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				if req != nil {
					t.Error("request must be nil when ErrProbeNotApplicable is returned")
				}
				return
			}

			if err != nil {
				t.Fatalf("BuildProbeRequest: %v", err)
			}
			if req.Method != http.MethodPost {
				t.Errorf("Method = %q, want POST", req.Method)
			}
			body := readBody(t, req)
			if _, ok := body["input"]; !ok {
				t.Error("embeddings body missing \"input\"")
			}
			// Unlike the chat probe, the embeddings probe does not
			// substitute the Azure deployment name for the model field.
			if body["model"] != "embed-model" {
				t.Errorf("body.model = %v, want %q", body["model"], "embed-model")
			}
		})
	}
}

// TestBuildProbeRequest_ChatAlwaysApplicable verifies that IntentChat never
// returns ErrProbeNotApplicable, for any provider.
func TestBuildProbeRequest_ChatAlwaysApplicable(t *testing.T) {
	t.Parallel()

	for _, provider := range []string{"", "openai", "vllm", "ollama", "custom", "anthropic", "azure", "gemini", "vertex"} {
		provider := provider
		t.Run(provider, func(t *testing.T) {
			t.Parallel()

			target := health.ProbeTarget{
				ModelName:       "some-model",
				Provider:        provider,
				BaseURL:         "https://upstream.example.com",
				APIKey:          "key",
				AzureDeployment: "dep",
				GCPProject:      "proj",
				GCPLocation:     "us-central1",
			}
			_, err := health.BuildProbeRequest(context.Background(), health.IntentChat, target)
			if err != nil {
				t.Errorf("BuildProbeRequest(IntentChat, provider=%q): %v", provider, err)
			}
		})
	}
}
