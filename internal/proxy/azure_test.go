package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ---- TransformURL -----------------------------------------------------------

func TestAzureTransformURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		baseURL      string
		upstreamPath string
		model        Model
		wantURL      string
	}{
		{
			name:         "chat/completions produces correct deployment URL with api-version",
			baseURL:      "https://myco.openai.azure.com",
			upstreamPath: "chat/completions",
			model: Model{
				AzureDeployment: "gpt4",
				AzureAPIVersion: "2024-10-21",
			},
			wantURL: "https://myco.openai.azure.com/openai/deployments/gpt4/chat/completions?api-version=2024-10-21",
		},
		{
			name:         "embeddings path uses deployment URL",
			baseURL:      "https://myco.openai.azure.com",
			upstreamPath: "embeddings",
			model: Model{
				AzureDeployment: "text-embedding-ada-002",
				AzureAPIVersion: "2024-02-01",
			},
			wantURL: "https://myco.openai.azure.com/openai/deployments/text-embedding-ada-002/embeddings?api-version=2024-02-01",
		},
		{
			name:         "completions path uses deployment URL",
			baseURL:      "https://myco.openai.azure.com",
			upstreamPath: "completions",
			model: Model{
				AzureDeployment: "gpt-35-turbo",
				AzureAPIVersion: "2024-02-01",
			},
			wantURL: "https://myco.openai.azure.com/openai/deployments/gpt-35-turbo/completions?api-version=2024-02-01",
		},
		{
			name:         "empty AzureAPIVersion uses default GA version",
			baseURL:      "https://myco.openai.azure.com",
			upstreamPath: "chat/completions",
			model: Model{
				AzureDeployment: "gpt4",
				AzureAPIVersion: "",
			},
			wantURL: "https://myco.openai.azure.com/openai/deployments/gpt4/chat/completions?api-version=2024-10-21",
		},
		{
			name:         "trailing slash on base URL does not produce double slash",
			baseURL:      "https://myco.openai.azure.com/",
			upstreamPath: "chat/completions",
			model: Model{
				AzureDeployment: "gpt4",
				AzureAPIVersion: "2024-10-21",
			},
			wantURL: "https://myco.openai.azure.com/openai/deployments/gpt4/chat/completions?api-version=2024-10-21",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			a := &AzureAdapter{}
			got := a.TransformURL(tc.baseURL, tc.upstreamPath, tc.model)

			if got != tc.wantURL {
				t.Errorf("TransformURL(%q, %q) = %q, want %q", tc.baseURL, tc.upstreamPath, got, tc.wantURL)
			}

			// Guard against double slashes after the scheme.
			noScheme := strings.SplitN(got, "://", 2)
			if len(noScheme) == 2 && strings.Contains(noScheme[1], "//") {
				t.Errorf("TransformURL result %q contains double slash in path", got)
			}
		})
	}
}

// ---- SetHeaders -------------------------------------------------------------

func TestAzureSetHeaders(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		model        Model
		initialAuth  string
		wantAPIKey   string // expected "api-key" value ("" means absent)
		wantAuthGone bool
	}{
		{
			name:         "Authorization removed and api-key set",
			model:        Model{APIKey: "azure-secret-key"},
			initialAuth:  "Bearer vl_uk_somekey",
			wantAPIKey:   "azure-secret-key",
			wantAuthGone: true,
		},
		{
			name:         "empty APIKey produces no api-key header",
			model:        Model{APIKey: ""},
			initialAuth:  "Bearer vl_uk_somekey",
			wantAPIKey:   "",
			wantAuthGone: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			req := httptest.NewRequest(http.MethodPost, "https://myco.openai.azure.com/openai/deployments/gpt4/chat/completions", nil)
			if tc.initialAuth != "" {
				req.Header.Set("Authorization", tc.initialAuth)
			}

			a := &AzureAdapter{}
			a.SetHeaders(req, tc.model)

			if tc.wantAuthGone {
				if got := req.Header.Get("Authorization"); got != "" {
					t.Errorf("Authorization header = %q, want absent (empty)", got)
				}
			}

			if tc.wantAPIKey != "" {
				if got := req.Header.Get("api-key"); got != tc.wantAPIKey {
					t.Errorf("api-key = %q, want %q", got, tc.wantAPIKey)
				}
			} else {
				if got := req.Header.Get("api-key"); got != "" {
					t.Errorf("api-key = %q, want absent (empty)", got)
				}
			}
		})
	}
}

// ---- TransformRequest (passthrough) -----------------------------------------

func TestAzureTransformRequest_Passthrough(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
	}{
		{
			name:  "JSON body returned unchanged",
			input: `{"model":"gpt-4o","messages":[{"role":"user","content":"Hello"}],"stream":false}`,
		},
		{
			name:  "body with extra fields returned unchanged",
			input: `{"model":"gpt-4o","messages":[{"role":"user","content":"Hi"}],"temperature":0.7,"max_tokens":512,"n":1}`,
		},
		{
			name:  "empty body returned unchanged",
			input: ``,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			a := &AzureAdapter{}
			got, err := a.TransformRequest([]byte(tc.input), Model{})

			if err != nil {
				t.Fatalf("TransformRequest() error = %v, want nil", err)
			}
			if string(got) != tc.input {
				t.Errorf("TransformRequest() = %q, want %q (passthrough unchanged)", got, tc.input)
			}
		})
	}
}

// ---- TransformResponse (passthrough) ----------------------------------------

func TestAzureTransformResponse_Passthrough(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
	}{
		{
			name:  "OpenAI response body returned unchanged",
			input: `{"id":"chatcmpl-abc","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"Hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}`,
		},
		{
			name:  "error response body returned unchanged",
			input: `{"error":{"code":"invalid_request","message":"bad request"}}`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			a := &AzureAdapter{}
			got, err := a.TransformResponse([]byte(tc.input))

			if err != nil {
				t.Fatalf("TransformResponse() error = %v, want nil", err)
			}
			if string(got) != tc.input {
				t.Errorf("TransformResponse() = %q, want %q (passthrough unchanged)", got, tc.input)
			}
		})
	}
}

// ---- TransformStreamLine (passthrough) --------------------------------------

func TestAzureTransformStreamLine_Passthrough(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
	}{
		{
			name:  "OpenAI SSE data line returned unchanged",
			input: `data: {"id":"chatcmpl-abc","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"Hello"},"finish_reason":null}]}`,
		},
		{
			name:  "DONE sentinel returned unchanged",
			input: "data: [DONE]",
		},
		{
			name:  "blank SSE delimiter returned unchanged",
			input: "",
		},
		{
			name:  "arbitrary line returned unchanged",
			input: "event: ping",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			a := &AzureAdapter{}
			got := a.TransformStreamLine([]byte(tc.input))

			if string(got) != tc.input {
				t.Errorf("TransformStreamLine() = %q, want %q (passthrough unchanged)", got, tc.input)
			}
		})
	}
}
