package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ---- TransformRequest -------------------------------------------------------

func TestGeminiTransformRequest(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   string
		checkFn func(t *testing.T, req geminiRequest, adapter *GeminiAdapter)
		wantErr bool
	}{
		{
			name:  "basic user message converted to contents",
			input: `{"model":"gemini-1.5-pro","messages":[{"role":"user","content":"Hello"}]}`,
			checkFn: func(t *testing.T, req geminiRequest, _ *GeminiAdapter) {
				t.Helper()
				if len(req.Contents) != 1 {
					t.Fatalf("len(contents) = %d, want 1", len(req.Contents))
				}
				if req.Contents[0].Role != "user" {
					t.Errorf("contents[0].role = %q, want %q", req.Contents[0].Role, "user")
				}
				if len(req.Contents[0].Parts) != 1 {
					t.Fatalf("len(contents[0].parts) = %d, want 1", len(req.Contents[0].Parts))
				}
				if req.Contents[0].Parts[0].Text != "Hello" {
					t.Errorf("contents[0].parts[0].text = %q, want %q", req.Contents[0].Parts[0].Text, "Hello")
				}
			},
		},
		{
			name:  "assistant role mapped to model role",
			input: `{"model":"gemini-1.5-pro","messages":[{"role":"user","content":"Hi"},{"role":"assistant","content":"Hey there"}]}`,
			checkFn: func(t *testing.T, req geminiRequest, _ *GeminiAdapter) {
				t.Helper()
				if len(req.Contents) != 2 {
					t.Fatalf("len(contents) = %d, want 2", len(req.Contents))
				}
				if req.Contents[1].Role != "model" {
					t.Errorf("contents[1].role = %q, want %q (assistant should map to model)", req.Contents[1].Role, "model")
				}
				if req.Contents[1].Parts[0].Text != "Hey there" {
					t.Errorf("contents[1].parts[0].text = %q, want %q", req.Contents[1].Parts[0].Text, "Hey there")
				}
			},
		},
		{
			name:  "system message extracted to systemInstruction and removed from contents",
			input: `{"model":"gemini-1.5-pro","messages":[{"role":"system","content":"You are helpful."},{"role":"user","content":"Hi"}]}`,
			checkFn: func(t *testing.T, req geminiRequest, _ *GeminiAdapter) {
				t.Helper()
				if req.SystemInstruction == nil {
					t.Fatal("systemInstruction is nil, want non-nil")
				}
				if len(req.SystemInstruction.Parts) != 1 {
					t.Fatalf("len(systemInstruction.parts) = %d, want 1", len(req.SystemInstruction.Parts))
				}
				if req.SystemInstruction.Parts[0].Text != "You are helpful." {
					t.Errorf("systemInstruction.parts[0].text = %q, want %q", req.SystemInstruction.Parts[0].Text, "You are helpful.")
				}
				// System message must not appear in contents.
				for _, c := range req.Contents {
					if c.Role == "system" {
						t.Error("contents still contains a system-role entry")
					}
				}
				if len(req.Contents) != 1 {
					t.Errorf("len(contents) = %d, want 1", len(req.Contents))
				}
			},
		},
		{
			name:  "multiple system messages merged into systemInstruction parts",
			input: `{"model":"gemini-1.5-pro","messages":[{"role":"system","content":"Part one."},{"role":"system","content":"Part two."},{"role":"user","content":"Hello"}]}`,
			checkFn: func(t *testing.T, req geminiRequest, _ *GeminiAdapter) {
				t.Helper()
				if req.SystemInstruction == nil {
					t.Fatal("systemInstruction is nil, want non-nil")
				}
				if len(req.SystemInstruction.Parts) != 2 {
					t.Fatalf("len(systemInstruction.parts) = %d, want 2", len(req.SystemInstruction.Parts))
				}
				if req.SystemInstruction.Parts[0].Text != "Part one." {
					t.Errorf("systemInstruction.parts[0].text = %q, want %q", req.SystemInstruction.Parts[0].Text, "Part one.")
				}
				if req.SystemInstruction.Parts[1].Text != "Part two." {
					t.Errorf("systemInstruction.parts[1].text = %q, want %q", req.SystemInstruction.Parts[1].Text, "Part two.")
				}
			},
		},
		{
			name:  "no system message produces nil systemInstruction",
			input: `{"model":"gemini-1.5-pro","messages":[{"role":"user","content":"Hi"}]}`,
			checkFn: func(t *testing.T, req geminiRequest, _ *GeminiAdapter) {
				t.Helper()
				if req.SystemInstruction != nil {
					t.Error("systemInstruction is non-nil, want nil when no system message present")
				}
			},
		},
		{
			name:  "mixed roles user assistant user preserved in order",
			input: `{"model":"gemini-1.5-pro","messages":[{"role":"user","content":"Q1"},{"role":"assistant","content":"A1"},{"role":"user","content":"Q2"}]}`,
			checkFn: func(t *testing.T, req geminiRequest, _ *GeminiAdapter) {
				t.Helper()
				if len(req.Contents) != 3 {
					t.Fatalf("len(contents) = %d, want 3", len(req.Contents))
				}
				wantRoles := []string{"user", "model", "user"}
				wantTexts := []string{"Q1", "A1", "Q2"}
				for i, c := range req.Contents {
					if c.Role != wantRoles[i] {
						t.Errorf("contents[%d].role = %q, want %q", i, c.Role, wantRoles[i])
					}
					if c.Parts[0].Text != wantTexts[i] {
						t.Errorf("contents[%d].parts[0].text = %q, want %q", i, c.Parts[0].Text, wantTexts[i])
					}
				}
			},
		},
		{
			name:  "temperature mapped to generationConfig.temperature",
			input: `{"model":"gemini-1.5-pro","messages":[{"role":"user","content":"Hi"}],"temperature":0.7}`,
			checkFn: func(t *testing.T, req geminiRequest, _ *GeminiAdapter) {
				t.Helper()
				if req.GenerationConfig.Temperature == nil {
					t.Fatal("generationConfig.temperature is nil, want 0.7")
				}
				if *req.GenerationConfig.Temperature != 0.7 {
					t.Errorf("generationConfig.temperature = %v, want 0.7", *req.GenerationConfig.Temperature)
				}
			},
		},
		{
			name:  "top_p mapped to generationConfig.topP",
			input: `{"model":"gemini-1.5-pro","messages":[{"role":"user","content":"Hi"}],"top_p":0.9}`,
			checkFn: func(t *testing.T, req geminiRequest, _ *GeminiAdapter) {
				t.Helper()
				if req.GenerationConfig.TopP == nil {
					t.Fatal("generationConfig.topP is nil, want 0.9")
				}
				if *req.GenerationConfig.TopP != 0.9 {
					t.Errorf("generationConfig.topP = %v, want 0.9", *req.GenerationConfig.TopP)
				}
			},
		},
		{
			name:  "max_tokens mapped to generationConfig.maxOutputTokens",
			input: `{"model":"gemini-1.5-pro","messages":[{"role":"user","content":"Hi"}],"max_tokens":512}`,
			checkFn: func(t *testing.T, req geminiRequest, _ *GeminiAdapter) {
				t.Helper()
				if req.GenerationConfig.MaxOutputTokens == nil {
					t.Fatal("generationConfig.maxOutputTokens is nil, want 512")
				}
				if *req.GenerationConfig.MaxOutputTokens != 512 {
					t.Errorf("generationConfig.maxOutputTokens = %d, want 512", *req.GenerationConfig.MaxOutputTokens)
				}
			},
		},
		{
			name:  "max_completion_tokens takes precedence over max_tokens",
			input: `{"model":"gemini-1.5-pro","messages":[{"role":"user","content":"Hi"}],"max_tokens":512,"max_completion_tokens":1024}`,
			checkFn: func(t *testing.T, req geminiRequest, _ *GeminiAdapter) {
				t.Helper()
				if req.GenerationConfig.MaxOutputTokens == nil {
					t.Fatal("generationConfig.maxOutputTokens is nil, want 1024")
				}
				if *req.GenerationConfig.MaxOutputTokens != 1024 {
					t.Errorf("generationConfig.maxOutputTokens = %d, want 1024 (max_completion_tokens should win)", *req.GenerationConfig.MaxOutputTokens)
				}
			},
		},
		{
			name:  "stop string mapped to stopSequences array",
			input: `{"model":"gemini-1.5-pro","messages":[{"role":"user","content":"Hi"}],"stop":"END"}`,
			checkFn: func(t *testing.T, req geminiRequest, _ *GeminiAdapter) {
				t.Helper()
				if len(req.GenerationConfig.StopSequences) != 1 {
					t.Fatalf("len(stopSequences) = %d, want 1", len(req.GenerationConfig.StopSequences))
				}
				if req.GenerationConfig.StopSequences[0] != "END" {
					t.Errorf("stopSequences[0] = %q, want %q", req.GenerationConfig.StopSequences[0], "END")
				}
			},
		},
		{
			name:  "stop array mapped to stopSequences",
			input: `{"model":"gemini-1.5-pro","messages":[{"role":"user","content":"Hi"}],"stop":["STOP","END"]}`,
			checkFn: func(t *testing.T, req geminiRequest, _ *GeminiAdapter) {
				t.Helper()
				if len(req.GenerationConfig.StopSequences) != 2 {
					t.Fatalf("len(stopSequences) = %d, want 2", len(req.GenerationConfig.StopSequences))
				}
				if req.GenerationConfig.StopSequences[0] != "STOP" {
					t.Errorf("stopSequences[0] = %q, want %q", req.GenerationConfig.StopSequences[0], "STOP")
				}
				if req.GenerationConfig.StopSequences[1] != "END" {
					t.Errorf("stopSequences[1] = %q, want %q", req.GenerationConfig.StopSequences[1], "END")
				}
			},
		},
		{
			name:  "response_format json_object sets responseMimeType",
			input: `{"model":"gemini-1.5-pro","messages":[{"role":"user","content":"Hi"}],"response_format":{"type":"json_object"}}`,
			checkFn: func(t *testing.T, req geminiRequest, _ *GeminiAdapter) {
				t.Helper()
				if req.GenerationConfig.ResponseMIMEType != "application/json" {
					t.Errorf("responseMimeType = %q, want %q", req.GenerationConfig.ResponseMIMEType, "application/json")
				}
			},
		},
		{
			name:  "response_format text does not set responseMimeType",
			input: `{"model":"gemini-1.5-pro","messages":[{"role":"user","content":"Hi"}],"response_format":{"type":"text"}}`,
			checkFn: func(t *testing.T, req geminiRequest, _ *GeminiAdapter) {
				t.Helper()
				if req.GenerationConfig.ResponseMIMEType != "" {
					t.Errorf("responseMimeType = %q, want empty for non-json_object type", req.GenerationConfig.ResponseMIMEType)
				}
			},
		},
		{
			name:  "stream true stored in adapter state",
			input: `{"model":"gemini-1.5-pro","messages":[{"role":"user","content":"Hi"}],"stream":true}`,
			checkFn: func(t *testing.T, _ geminiRequest, a *GeminiAdapter) {
				t.Helper()
				if !a.streaming {
					t.Error("adapter.streaming = false, want true after stream:true request")
				}
			},
		},
		{
			name:  "stream false leaves adapter non-streaming",
			input: `{"model":"gemini-1.5-pro","messages":[{"role":"user","content":"Hi"}],"stream":false}`,
			checkFn: func(t *testing.T, _ geminiRequest, a *GeminiAdapter) {
				t.Helper()
				if a.streaming {
					t.Error("adapter.streaming = true, want false after stream:false request")
				}
			},
		},
		{
			name:  "no stream field leaves adapter non-streaming",
			input: `{"model":"gemini-1.5-pro","messages":[{"role":"user","content":"Hi"}]}`,
			checkFn: func(t *testing.T, _ geminiRequest, a *GeminiAdapter) {
				t.Helper()
				if a.streaming {
					t.Error("adapter.streaming = true, want false when stream field absent")
				}
			},
		},
		{
			name:  "empty messages array produces empty contents",
			input: `{"model":"gemini-1.5-pro","messages":[]}`,
			checkFn: func(t *testing.T, req geminiRequest, _ *GeminiAdapter) {
				t.Helper()
				if len(req.Contents) != 0 {
					t.Errorf("len(contents) = %d, want 0 for empty messages", len(req.Contents))
				}
			},
		},
		{
			name:  "content array blocks with type text converted to parts",
			input: `{"model":"gemini-1.5-pro","messages":[{"role":"user","content":[{"type":"text","text":"Block one"},{"type":"text","text":"Block two"}]}]}`,
			checkFn: func(t *testing.T, req geminiRequest, _ *GeminiAdapter) {
				t.Helper()
				if len(req.Contents) != 1 {
					t.Fatalf("len(contents) = %d, want 1", len(req.Contents))
				}
				if len(req.Contents[0].Parts) != 2 {
					t.Fatalf("len(contents[0].parts) = %d, want 2", len(req.Contents[0].Parts))
				}
				if req.Contents[0].Parts[0].Text != "Block one" {
					t.Errorf("parts[0].text = %q, want %q", req.Contents[0].Parts[0].Text, "Block one")
				}
				if req.Contents[0].Parts[1].Text != "Block two" {
					t.Errorf("parts[1].text = %q, want %q", req.Contents[0].Parts[1].Text, "Block two")
				}
			},
		},
		{
			name:  "content array with non-text blocks skipped",
			input: `{"model":"gemini-1.5-pro","messages":[{"role":"user","content":[{"type":"image_url","url":"http://example.com/img.png"},{"type":"text","text":"describe it"}]}]}`,
			checkFn: func(t *testing.T, req geminiRequest, _ *GeminiAdapter) {
				t.Helper()
				if len(req.Contents) != 1 {
					t.Fatalf("len(contents) = %d, want 1", len(req.Contents))
				}
				// Only the text block should appear.
				if len(req.Contents[0].Parts) != 1 {
					t.Fatalf("len(parts) = %d, want 1 (non-text block should be skipped)", len(req.Contents[0].Parts))
				}
				if req.Contents[0].Parts[0].Text != "describe it" {
					t.Errorf("parts[0].text = %q, want %q", req.Contents[0].Parts[0].Text, "describe it")
				}
			},
		},
		{
			name:  "n maps to candidateCount in generationConfig",
			input: `{"model":"gemini-1.5-pro","messages":[{"role":"user","content":"Hi"}],"n":3}`,
			checkFn: func(t *testing.T, req geminiRequest, _ *GeminiAdapter) {
				t.Helper()
				if req.GenerationConfig.CandidateCount == nil {
					t.Fatal("generationConfig.candidateCount is nil, want 3")
				}
				if *req.GenerationConfig.CandidateCount != 3 {
					t.Errorf("generationConfig.candidateCount = %d, want 3", *req.GenerationConfig.CandidateCount)
				}
			},
		},
		{
			name:    "invalid JSON returns error",
			input:   `not-json`,
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			a := &GeminiAdapter{}
			out, err := a.TransformRequest([]byte(tc.input), Model{})

			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("TransformRequest() error = %v", err)
			}

			var req geminiRequest
			if err := json.Unmarshal(out, &req); err != nil {
				t.Fatalf("output is not valid geminiRequest JSON: %v", err)
			}

			tc.checkFn(t, req, a)
		})
	}
}

// ---- TransformURL -----------------------------------------------------------

func TestGeminiTransformURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		baseURL   string
		model     Model
		streaming bool
		wantURL   string
	}{
		{
			name:    "Gemini API non-streaming URL",
			baseURL: "https://generativelanguage.googleapis.com",
			model:   Model{Name: "gemini-1.5-pro", Provider: "gemini"},
			wantURL: "https://generativelanguage.googleapis.com/v1beta/models/gemini-1.5-pro:generateContent",
		},
		{
			name:      "Gemini API streaming URL uses streamGenerateContent with alt=sse",
			baseURL:   "https://generativelanguage.googleapis.com",
			model:     Model{Name: "gemini-1.5-pro", Provider: "gemini"},
			streaming: true,
			wantURL:   "https://generativelanguage.googleapis.com/v1beta/models/gemini-1.5-pro:streamGenerateContent?alt=sse",
		},
		{
			name:    "Gemini API trailing slash on base URL does not produce double slash",
			baseURL: "https://generativelanguage.googleapis.com/",
			model:   Model{Name: "gemini-1.5-flash", Provider: "gemini"},
			wantURL: "https://generativelanguage.googleapis.com/v1beta/models/gemini-1.5-flash:generateContent",
		},
		{
			name:    "Vertex AI URL via provider field",
			baseURL: "https://us-central1-aiplatform.googleapis.com",
			model: Model{
				Name:        "gemini-1.5-pro",
				Provider:    "vertex",
				GCPProject:  "my-project",
				GCPLocation: "us-central1",
			},
			wantURL: "https://us-central1-aiplatform.googleapis.com/v1/projects/my-project/locations/us-central1/publishers/google/models/gemini-1.5-pro:generateContent",
		},
		{
			name:    "Vertex AI streaming URL",
			baseURL: "https://us-central1-aiplatform.googleapis.com",
			model: Model{
				Name:        "gemini-1.5-pro",
				Provider:    "vertex",
				GCPProject:  "my-project",
				GCPLocation: "us-central1",
			},
			streaming: true,
			wantURL:   "https://us-central1-aiplatform.googleapis.com/v1/projects/my-project/locations/us-central1/publishers/google/models/gemini-1.5-pro:streamGenerateContent?alt=sse",
		},
		{
			name:    "provider field is sole authority: gemini provider with aiplatform base URL uses Gemini API path",
			baseURL: "https://aiplatform.googleapis.com",
			model: Model{
				Name:        "gemini-1.5-flash",
				Provider:    "gemini",
				GCPProject:  "proj-123",
				GCPLocation: "europe-west4",
			},
			wantURL: "https://aiplatform.googleapis.com/v1beta/models/gemini-1.5-flash:generateContent",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			a := &GeminiAdapter{streaming: tc.streaming}
			got := a.TransformURL(tc.baseURL, "chat/completions", tc.model)

			if got != tc.wantURL {
				t.Errorf("TransformURL() = %q, want %q", got, tc.wantURL)
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

func TestGeminiSetHeaders(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		requestURL      string
		model           Model
		initialAuth     string
		wantGoogAPIKey  string // expected x-goog-api-key value ("" = absent)
		wantAuthPresent bool   // Authorization header should still be present
	}{
		{
			name:           "Gemini API: Authorization removed and x-goog-api-key set",
			requestURL:     "https://generativelanguage.googleapis.com/v1beta/models/gemini-1.5-pro:generateContent",
			model:          Model{APIKey: "gemini-api-key-123", Provider: "gemini"},
			initialAuth:    "Bearer vl_uk_somekey",
			wantGoogAPIKey: "gemini-api-key-123",
		},
		{
			name:        "Gemini API: empty APIKey produces no x-goog-api-key header",
			requestURL:  "https://generativelanguage.googleapis.com/v1beta/models/gemini-1.5-pro:generateContent",
			model:       Model{APIKey: "", Provider: "gemini"},
			initialAuth: "Bearer vl_uk_somekey",
		},
		{
			name:            "Vertex AI via provider field: Authorization kept unchanged",
			requestURL:      "https://us-central1-aiplatform.googleapis.com/v1/projects/proj/locations/us-central1/publishers/google/models/gemini-1.5-pro:generateContent",
			model:           Model{APIKey: "should-be-ignored", Provider: "vertex"},
			initialAuth:     "Bearer gcloud-access-token",
			wantAuthPresent: true,
		},
		{
			name:           "provider field is sole authority: gemini provider with aiplatform host uses x-goog-api-key",
			requestURL:     "https://aiplatform.googleapis.com/v1/projects/proj/locations/us-central1/publishers/google/models/gemini:generateContent",
			model:          Model{APIKey: "key", Provider: "gemini"},
			initialAuth:    "Bearer gcloud-token",
			wantGoogAPIKey: "key",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			req := httptest.NewRequest(http.MethodPost, tc.requestURL, nil)
			if tc.initialAuth != "" {
				req.Header.Set("Authorization", tc.initialAuth)
			}

			a := &GeminiAdapter{}
			a.SetHeaders(req, tc.model)

			if tc.wantAuthPresent {
				if got := req.Header.Get("Authorization"); got != tc.initialAuth {
					t.Errorf("Authorization = %q, want %q (should be preserved for Vertex)", got, tc.initialAuth)
				}
			} else {
				if got := req.Header.Get("Authorization"); got != "" {
					t.Errorf("Authorization = %q, want absent (should be removed for Gemini API)", got)
				}
			}

			if tc.wantGoogAPIKey != "" {
				if got := req.Header.Get("x-goog-api-key"); got != tc.wantGoogAPIKey {
					t.Errorf("x-goog-api-key = %q, want %q", got, tc.wantGoogAPIKey)
				}
			} else if !tc.wantAuthPresent {
				// Gemini API path with empty key — header must be absent.
				if got := req.Header.Get("x-goog-api-key"); got != "" {
					t.Errorf("x-goog-api-key = %q, want absent when APIKey is empty", got)
				}
			}
		})
	}
}

// ---- TransformResponse ------------------------------------------------------

func TestGeminiTransformResponse(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		inputJSON      string
		wantContent    string
		wantFinish     string
		wantPrompt     int
		wantCompletion int
		wantTotal      int
		wantErr        bool
	}{
		{
			name: "basic response converted to OpenAI format",
			inputJSON: `{
				"candidates": [{"content":{"role":"model","parts":[{"text":"Hello there"}]},"finishReason":"STOP"}],
				"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":5,"totalTokenCount":15}
			}`,
			wantContent:    "Hello there",
			wantFinish:     "stop",
			wantPrompt:     10,
			wantCompletion: 5,
			wantTotal:      15,
		},
		{
			name:       "finishReason STOP maps to stop",
			inputJSON:  `{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}],"usageMetadata":{}}`,
			wantFinish: "stop",
		},
		{
			name:       "finishReason MAX_TOKENS maps to length",
			inputJSON:  `{"candidates":[{"content":{"role":"model","parts":[{"text":"truncated"}]},"finishReason":"MAX_TOKENS"}],"usageMetadata":{}}`,
			wantFinish: "length",
		},
		{
			name:       "finishReason SAFETY maps to content_filter",
			inputJSON:  `{"candidates":[{"content":{"role":"model","parts":[{"text":""}]},"finishReason":"SAFETY"}],"usageMetadata":{}}`,
			wantFinish: "content_filter",
		},
		{
			name:       "finishReason RECITATION maps to content_filter",
			inputJSON:  `{"candidates":[{"content":{"role":"model","parts":[{"text":""}]},"finishReason":"RECITATION"}],"usageMetadata":{}}`,
			wantFinish: "content_filter",
		},
		{
			name:       "unknown finishReason defaults to stop",
			inputJSON:  `{"candidates":[{"content":{"role":"model","parts":[{"text":"hi"}]},"finishReason":"OTHER"}],"usageMetadata":{}}`,
			wantFinish: "stop",
		},
		{
			name:        "multiple parts joined into single content string",
			inputJSON:   `{"candidates":[{"content":{"role":"model","parts":[{"text":"Hello"},{"text":" world"}]},"finishReason":"STOP"}],"usageMetadata":{}}`,
			wantContent: "Hello world",
			wantFinish:  "stop",
		},
		{
			name:       "empty candidates produces stop finish reason",
			inputJSON:  `{"candidates":[],"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":0,"totalTokenCount":5}}`,
			wantFinish: "stop",
			wantPrompt: 5,
			wantTotal:  5,
		},
		{
			name: "usage metadata mapped to OpenAI usage fields",
			inputJSON: `{
				"candidates":[{"content":{"role":"model","parts":[{"text":"x"}]},"finishReason":"STOP"}],
				"usageMetadata":{"promptTokenCount":42,"candidatesTokenCount":13,"totalTokenCount":55}
			}`,
			wantPrompt:     42,
			wantCompletion: 13,
			wantTotal:      55,
		},
		{
			name:      "invalid JSON returns error",
			inputJSON: "not-json",
			wantErr:   true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			a := &GeminiAdapter{}
			out, err := a.TransformResponse([]byte(tc.inputJSON))

			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("TransformResponse() error = %v", err)
			}

			var resp openAIResponse
			if err := json.Unmarshal(out, &resp); err != nil {
				t.Fatalf("output is not valid openAIResponse JSON: %v", err)
			}

			if resp.Object != "chat.completion" {
				t.Errorf("object = %q, want %q", resp.Object, "chat.completion")
			}
			if len(resp.Choices) != 1 {
				t.Fatalf("len(choices) = %d, want 1", len(resp.Choices))
			}

			ch := resp.Choices[0]
			if tc.wantContent != "" && ch.Message.Content != tc.wantContent {
				t.Errorf("choices[0].message.content = %q, want %q", ch.Message.Content, tc.wantContent)
			}
			if tc.wantFinish != "" && ch.FinishReason != tc.wantFinish {
				t.Errorf("choices[0].finish_reason = %q, want %q", ch.FinishReason, tc.wantFinish)
			}
			if tc.wantPrompt != 0 && resp.Usage.PromptTokens != tc.wantPrompt {
				t.Errorf("usage.prompt_tokens = %d, want %d", resp.Usage.PromptTokens, tc.wantPrompt)
			}
			if tc.wantCompletion != 0 && resp.Usage.CompletionTokens != tc.wantCompletion {
				t.Errorf("usage.completion_tokens = %d, want %d", resp.Usage.CompletionTokens, tc.wantCompletion)
			}
			if tc.wantTotal != 0 && resp.Usage.TotalTokens != tc.wantTotal {
				t.Errorf("usage.total_tokens = %d, want %d", resp.Usage.TotalTokens, tc.wantTotal)
			}
		})
	}
}

// ---- TransformStreamLine ----------------------------------------------------

func TestGeminiTransformStreamLine(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		line      string
		wantNil   bool
		wantExact string
		checkFn   func(t *testing.T, out []byte)
	}{
		{
			name:    "non-data line is dropped",
			line:    "event: ping",
			wantNil: true,
		},
		{
			name:    "comment line is dropped",
			line:    ": keep-alive",
			wantNil: true,
		},
		{
			name:    "invalid JSON payload is dropped",
			line:    "data: not-json",
			wantNil: true,
		},
		{
			name:    "empty content and no finishReason chunk is dropped",
			line:    `data: {"candidates":[{"content":{"role":"model","parts":[{"text":""}]},"finishReason":""}],"usageMetadata":{}}`,
			wantNil: true,
		},
		{
			name: "data line with text delta produces OpenAI chunk",
			line: `data: {"candidates":[{"content":{"role":"model","parts":[{"text":"hello"}]},"finishReason":""}],"usageMetadata":{}}`,
			checkFn: func(t *testing.T, out []byte) {
				t.Helper()
				chunk := parseChunk(t, out)
				if chunk.Object != "chat.completion.chunk" {
					t.Errorf("object = %q, want %q", chunk.Object, "chat.completion.chunk")
				}
				if len(chunk.Choices) != 1 {
					t.Fatalf("len(choices) = %d, want 1", len(chunk.Choices))
				}
				if chunk.Choices[0].Delta.Content != "hello" {
					t.Errorf("delta.content = %q, want %q", chunk.Choices[0].Delta.Content, "hello")
				}
				if chunk.Choices[0].FinishReason != nil {
					t.Errorf("finish_reason = %v, want nil for mid-stream chunk", *chunk.Choices[0].FinishReason)
				}
			},
		},
		{
			name: "finishReason STOP in chunk sets finish_reason stop",
			line: `data: {"candidates":[{"content":{"role":"model","parts":[{"text":"final"}]},"finishReason":"STOP"}],"usageMetadata":{}}`,
			checkFn: func(t *testing.T, out []byte) {
				t.Helper()
				chunk := parseChunk(t, out)
				if len(chunk.Choices) != 1 {
					t.Fatalf("len(choices) = %d, want 1", len(chunk.Choices))
				}
				if chunk.Choices[0].FinishReason == nil {
					t.Fatal("finish_reason is nil, want non-nil")
				}
				if *chunk.Choices[0].FinishReason != "stop" {
					t.Errorf("finish_reason = %q, want %q", *chunk.Choices[0].FinishReason, "stop")
				}
			},
		},
		{
			name: "finishReason MAX_TOKENS in chunk maps to length",
			line: `data: {"candidates":[{"content":{"role":"model","parts":[{"text":"cut"}]},"finishReason":"MAX_TOKENS"}],"usageMetadata":{}}`,
			checkFn: func(t *testing.T, out []byte) {
				t.Helper()
				chunk := parseChunk(t, out)
				if len(chunk.Choices) != 1 {
					t.Fatalf("len(choices) = %d, want 1", len(chunk.Choices))
				}
				if chunk.Choices[0].FinishReason == nil {
					t.Fatal("finish_reason is nil, want non-nil")
				}
				if *chunk.Choices[0].FinishReason != "length" {
					t.Errorf("finish_reason = %q, want %q", *chunk.Choices[0].FinishReason, "length")
				}
			},
		},
		{
			name: "finishReason SAFETY in chunk maps to content_filter",
			line: `data: {"candidates":[{"content":{"role":"model","parts":[{"text":""}]},"finishReason":"SAFETY"}],"usageMetadata":{}}`,
			checkFn: func(t *testing.T, out []byte) {
				t.Helper()
				chunk := parseChunk(t, out)
				if len(chunk.Choices) != 1 {
					t.Fatalf("len(choices) = %d, want 1", len(chunk.Choices))
				}
				if chunk.Choices[0].FinishReason == nil {
					t.Fatal("finish_reason is nil, want non-nil")
				}
				if *chunk.Choices[0].FinishReason != "content_filter" {
					t.Errorf("finish_reason = %q, want %q", *chunk.Choices[0].FinishReason, "content_filter")
				}
			},
		},
		{
			name: "blank line after terminal chunk becomes data: [DONE]",
			// This case is tested via the stateful sequence test below; here we
			// verify that a standalone blank line on a fresh adapter passes through.
			line:      "",
			wantExact: "",
		},
		{
			name:      "Gemini [DONE] sentinel passed through",
			line:      "data: [DONE]",
			wantExact: "data: [DONE]",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			a := &GeminiAdapter{}
			out := a.TransformStreamLine([]byte(tc.line))

			if tc.wantNil {
				if out != nil {
					t.Errorf("TransformStreamLine() = %q, want nil", out)
				}
				return
			}

			if tc.line == "" || tc.wantExact != "" {
				if string(out) != tc.wantExact {
					t.Errorf("TransformStreamLine() = %q, want %q", out, tc.wantExact)
				}
				return
			}

			if out == nil {
				t.Fatal("TransformStreamLine() = nil, want non-nil")
			}
			tc.checkFn(t, out)
		})
	}
}

// TestGeminiTransformStreamLine_DoneSequence verifies the full terminal chunk
// sequence: a finishReason chunk sets doneSent, and the blank SSE delimiter
// that follows is converted to data: [DONE].
func TestGeminiTransformStreamLine_DoneSequence(t *testing.T) {
	t.Parallel()

	a := &GeminiAdapter{}

	// 1. A normal mid-stream delta should produce a chunk without finish_reason.
	mid := a.TransformStreamLine([]byte(`data: {"candidates":[{"content":{"role":"model","parts":[{"text":"word "}]},"finishReason":""}],"usageMetadata":{}}`))
	if mid == nil {
		t.Fatal("mid-stream TransformStreamLine() = nil, want non-nil chunk")
	}
	midChunk := parseChunk(t, mid)
	if midChunk.Choices[0].FinishReason != nil {
		t.Errorf("mid-stream finish_reason = %v, want nil", *midChunk.Choices[0].FinishReason)
	}

	// 2. Terminal chunk with finishReason should emit a chunk with finish_reason.
	terminal := a.TransformStreamLine([]byte(`data: {"candidates":[{"content":{"role":"model","parts":[{"text":"done"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":10,"totalTokenCount":15}}`))
	if terminal == nil {
		t.Fatal("terminal TransformStreamLine() = nil, want non-nil chunk")
	}
	termChunk := parseChunk(t, terminal)
	if termChunk.Choices[0].FinishReason == nil {
		t.Fatal("terminal finish_reason is nil, want non-nil")
	}
	if *termChunk.Choices[0].FinishReason != "stop" {
		t.Errorf("terminal finish_reason = %q, want %q", *termChunk.Choices[0].FinishReason, "stop")
	}

	// 3. The blank SSE delimiter immediately after must become data: [DONE].
	done := a.TransformStreamLine([]byte(""))
	if string(done) != "data: [DONE]" {
		t.Errorf("blank-after-terminal TransformStreamLine() = %q, want %q", done, "data: [DONE]")
	}

	// 4. The doneSent flag must be cleared so subsequent blank lines pass through normally.
	afterDone := a.TransformStreamLine([]byte(""))
	if string(afterDone) != "" {
		t.Errorf("second-blank TransformStreamLine() = %q, want empty string", afterDone)
	}
}

// TestGeminiTransformStreamLine_UsageAccumulation verifies that usageMetadata
// carried in a stream chunk is accumulated and available via StreamUsage.
func TestGeminiTransformStreamLine_UsageAccumulation(t *testing.T) {
	t.Parallel()

	a := &GeminiAdapter{}

	// Feed a chunk carrying usage metadata.
	_ = a.TransformStreamLine([]byte(`data: {"candidates":[{"content":{"role":"model","parts":[{"text":"hi"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":20,"candidatesTokenCount":8,"totalTokenCount":28}}`))

	usage := a.StreamUsage()
	if usage.PromptTokens != 20 {
		t.Errorf("PromptTokens = %d, want 20", usage.PromptTokens)
	}
	if usage.CompletionTokens != 8 {
		t.Errorf("CompletionTokens = %d, want 8", usage.CompletionTokens)
	}
	if usage.TotalTokens != 28 {
		t.Errorf("TotalTokens = %d, want 28", usage.TotalTokens)
	}
}

// ---- StreamUsage ------------------------------------------------------------

func TestGeminiStreamUsage(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		lines          []string
		wantPrompt     int
		wantCompletion int
		wantTotal      int
	}{
		{
			name:           "zero usage before any stream lines",
			lines:          nil,
			wantPrompt:     0,
			wantCompletion: 0,
			wantTotal:      0,
		},
		{
			name: "usage accumulated from final chunk",
			lines: []string{
				`data: {"candidates":[{"content":{"role":"model","parts":[{"text":"a"}]},"finishReason":""}],"usageMetadata":{"promptTokenCount":0,"candidatesTokenCount":0,"totalTokenCount":0}}`,
				`data: {"candidates":[{"content":{"role":"model","parts":[{"text":"b"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":15,"candidatesTokenCount":7,"totalTokenCount":22}}`,
			},
			wantPrompt:     15,
			wantCompletion: 7,
			wantTotal:      22,
		},
		{
			name: "TotalTokens computed as sum of prompt and completion",
			lines: []string{
				`data: {"candidates":[{"content":{"role":"model","parts":[{"text":"x"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":4,"totalTokenCount":7}}`,
			},
			wantPrompt:     3,
			wantCompletion: 4,
			wantTotal:      7,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			a := &GeminiAdapter{}
			for _, line := range tc.lines {
				_ = a.TransformStreamLine([]byte(line))
			}

			got := a.StreamUsage()
			if got.PromptTokens != tc.wantPrompt {
				t.Errorf("PromptTokens = %d, want %d", got.PromptTokens, tc.wantPrompt)
			}
			if got.CompletionTokens != tc.wantCompletion {
				t.Errorf("CompletionTokens = %d, want %d", got.CompletionTokens, tc.wantCompletion)
			}
			if got.TotalTokens != tc.wantTotal {
				t.Errorf("TotalTokens = %d, want %d", got.TotalTokens, tc.wantTotal)
			}
		})
	}
}

// ---- geminiFinishReason (unit) ----------------------------------------------

func TestGeminiFinishReason(t *testing.T) {
	t.Parallel()

	tests := []struct {
		input string
		want  string
	}{
		{"STOP", "stop"},
		{"MAX_TOKENS", "length"},
		{"SAFETY", "content_filter"},
		{"RECITATION", "content_filter"},
		{"OTHER", "stop"},
		{"", "stop"},
		{"UNKNOWN_REASON", "stop"},
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			t.Parallel()
			got := geminiFinishReason(tc.input)
			if got != tc.want {
				t.Errorf("geminiFinishReason(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

// ---- GetAdapter integration -------------------------------------------------

func TestGetAdapter_GeminiVertex(t *testing.T) {
	t.Parallel()

	tests := []struct {
		provider string
		wantNil  bool
	}{
		{"gemini", false},
		{"vertex", false},
	}

	for _, tc := range tests {
		t.Run(tc.provider, func(t *testing.T) {
			t.Parallel()
			got := GetAdapter(tc.provider)
			if tc.wantNil && got != nil {
				t.Errorf("GetAdapter(%q) = non-nil, want nil", tc.provider)
			}
			if !tc.wantNil && got == nil {
				t.Errorf("GetAdapter(%q) = nil, want *GeminiAdapter", tc.provider)
			}
			if !tc.wantNil {
				if _, ok := got.(*GeminiAdapter); !ok {
					t.Errorf("GetAdapter(%q) = %T, want *GeminiAdapter", tc.provider, got)
				}
			}
		})
	}
}
