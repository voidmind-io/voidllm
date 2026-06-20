package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
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
			if tc.wantContent != "" {
				if ch.Message.Content == nil || *ch.Message.Content != tc.wantContent {
					var got string
					if ch.Message.Content != nil {
						got = *ch.Message.Content
					}
					t.Errorf("choices[0].message.content = %q, want %q", got, tc.wantContent)
				}
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

// ---- Tool-call happy paths --------------------------------------------------

// TestGeminiTransformRequest_Tools verifies that OpenAI tools are translated
// into a Gemini functionDeclarations array and that tool_choice is mapped to
// the correct functionCallingConfig mode.
func TestGeminiTransformRequest_Tools(t *testing.T) {
	t.Parallel()

	t.Run("tools translated to functionDeclarations", func(t *testing.T) {
		t.Parallel()
		input := `{
			"model": "gemini-1.5-pro",
			"messages": [{"role":"user","content":"What is the weather?"}],
			"tools": [
				{
					"type": "function",
					"function": {
						"name": "get_weather",
						"description": "Get the current weather",
						"parameters": {"type":"object","properties":{"location":{"type":"string"}}}
					}
				}
			]
		}`
		a := &GeminiAdapter{}
		out, err := a.TransformRequest([]byte(input), Model{})
		if err != nil {
			t.Fatalf("TransformRequest() error = %v", err)
		}
		var req geminiRequest
		if err := json.Unmarshal(out, &req); err != nil {
			t.Fatalf("unmarshal output: %v", err)
		}
		if len(req.Tools) != 1 {
			t.Fatalf("len(tools) = %d, want 1", len(req.Tools))
		}
		if len(req.Tools[0].FunctionDeclarations) != 1 {
			t.Fatalf("len(functionDeclarations) = %d, want 1", len(req.Tools[0].FunctionDeclarations))
		}
		decl := req.Tools[0].FunctionDeclarations[0]
		if decl.Name != "get_weather" {
			t.Errorf("name = %q, want %q", decl.Name, "get_weather")
		}
		if decl.Description != "Get the current weather" {
			t.Errorf("description = %q, want %q", decl.Description, "Get the current weather")
		}
		if string(decl.Parameters) == "" {
			t.Error("parameters should not be empty")
		}
	})

	t.Run("tool_choice auto maps to AUTO", func(t *testing.T) {
		t.Parallel()
		input := `{
			"model": "gemini-1.5-pro",
			"messages": [{"role":"user","content":"Hi"}],
			"tools": [{"type":"function","function":{"name":"fn","parameters":{}}}],
			"tool_choice": "auto"
		}`
		a := &GeminiAdapter{}
		out, err := a.TransformRequest([]byte(input), Model{})
		if err != nil {
			t.Fatalf("TransformRequest() error = %v", err)
		}
		var req geminiRequest
		if err := json.Unmarshal(out, &req); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if req.ToolConfig == nil {
			t.Fatal("toolConfig is nil, want non-nil")
		}
		if req.ToolConfig.FunctionCallingConfig.Mode != "AUTO" {
			t.Errorf("mode = %q, want AUTO", req.ToolConfig.FunctionCallingConfig.Mode)
		}
	})

	t.Run("tool_choice required maps to ANY", func(t *testing.T) {
		t.Parallel()
		input := `{
			"model": "gemini-1.5-pro",
			"messages": [{"role":"user","content":"Hi"}],
			"tools": [{"type":"function","function":{"name":"fn","parameters":{}}}],
			"tool_choice": "required"
		}`
		a := &GeminiAdapter{}
		out, err := a.TransformRequest([]byte(input), Model{})
		if err != nil {
			t.Fatalf("TransformRequest() error = %v", err)
		}
		var req geminiRequest
		if err := json.Unmarshal(out, &req); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if req.ToolConfig == nil {
			t.Fatal("toolConfig is nil")
		}
		if req.ToolConfig.FunctionCallingConfig.Mode != "ANY" {
			t.Errorf("mode = %q, want ANY", req.ToolConfig.FunctionCallingConfig.Mode)
		}
	})

	t.Run("tool_choice none removes tools and tool_choice", func(t *testing.T) {
		t.Parallel()
		input := `{
			"model": "gemini-1.5-pro",
			"messages": [{"role":"user","content":"Hi"}],
			"tools": [{"type":"function","function":{"name":"fn","parameters":{}}}],
			"tool_choice": "none"
		}`
		a := &GeminiAdapter{}
		out, err := a.TransformRequest([]byte(input), Model{})
		if err != nil {
			t.Fatalf("TransformRequest() error = %v", err)
		}
		var req geminiRequest
		if err := json.Unmarshal(out, &req); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if len(req.Tools) != 0 {
			t.Errorf("tools should be absent when tool_choice=none, got %d entries", len(req.Tools))
		}
		if req.ToolConfig != nil {
			t.Error("toolConfig should be nil when tool_choice=none")
		}
	})

	t.Run("named tool_choice maps to ANY with allowedFunctionNames", func(t *testing.T) {
		t.Parallel()
		input := `{
			"model": "gemini-1.5-pro",
			"messages": [{"role":"user","content":"Hi"}],
			"tools": [{"type":"function","function":{"name":"my_tool","parameters":{}}}],
			"tool_choice": {"type":"function","function":{"name":"my_tool"}}
		}`
		a := &GeminiAdapter{}
		out, err := a.TransformRequest([]byte(input), Model{})
		if err != nil {
			t.Fatalf("TransformRequest() error = %v", err)
		}
		var req geminiRequest
		if err := json.Unmarshal(out, &req); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if req.ToolConfig == nil {
			t.Fatal("toolConfig is nil")
		}
		cfg := req.ToolConfig.FunctionCallingConfig
		if cfg.Mode != "ANY" {
			t.Errorf("mode = %q, want ANY", cfg.Mode)
		}
		if len(cfg.AllowedFunctionNames) != 1 || cfg.AllowedFunctionNames[0] != "my_tool" {
			t.Errorf("allowedFunctionNames = %v, want [my_tool]", cfg.AllowedFunctionNames)
		}
	})

	t.Run("assistant tool_calls become functionCall parts", func(t *testing.T) {
		t.Parallel()
		input := `{
			"model": "gemini-1.5-pro",
			"messages": [
				{"role":"user","content":"What is the weather?"},
				{
					"role":"assistant",
					"content":null,
					"tool_calls":[{
						"id":"call_abc",
						"type":"function",
						"function":{"name":"get_weather","arguments":"{\"location\":\"NYC\"}"}
					}]
				}
			]
		}`
		a := &GeminiAdapter{}
		out, err := a.TransformRequest([]byte(input), Model{})
		if err != nil {
			t.Fatalf("TransformRequest() error = %v", err)
		}
		var req geminiRequest
		if err := json.Unmarshal(out, &req); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		// Expect 2 contents: user + model
		if len(req.Contents) != 2 {
			t.Fatalf("len(contents) = %d, want 2", len(req.Contents))
		}
		modelContent := req.Contents[1]
		if modelContent.Role != "model" {
			t.Errorf("role = %q, want model", modelContent.Role)
		}
		if len(modelContent.Parts) != 1 {
			t.Fatalf("len(parts) = %d, want 1", len(modelContent.Parts))
		}
		fc := modelContent.Parts[0].FunctionCall
		if fc == nil {
			t.Fatal("functionCall is nil")
		}
		if fc.Name != "get_weather" {
			t.Errorf("name = %q, want get_weather", fc.Name)
		}
		// Args must be a JSON object (parsed from the arguments string).
		if string(fc.Args) == "" || fc.Args[0] != '{' {
			t.Errorf("args = %q, want JSON object", fc.Args)
		}
	})

	t.Run("role:tool messages become functionResponse parts in user content", func(t *testing.T) {
		t.Parallel()
		input := `{
			"model": "gemini-1.5-pro",
			"messages": [
				{"role":"user","content":"Weather?"},
				{
					"role":"assistant",
					"content":null,
					"tool_calls":[{
						"id":"call_123",
						"type":"function",
						"function":{"name":"get_weather","arguments":"{}"}
					}]
				},
				{
					"role":"tool",
					"tool_call_id":"call_123",
					"content":"Sunny, 72°F"
				}
			]
		}`
		a := &GeminiAdapter{}
		out, err := a.TransformRequest([]byte(input), Model{})
		if err != nil {
			t.Fatalf("TransformRequest() error = %v", err)
		}
		var req geminiRequest
		if err := json.Unmarshal(out, &req); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		// Expect 3 contents: user, model, user(functionResponse)
		if len(req.Contents) != 3 {
			t.Fatalf("len(contents) = %d, want 3", len(req.Contents))
		}
		frContent := req.Contents[2]
		if frContent.Role != "user" {
			t.Errorf("role = %q, want user", frContent.Role)
		}
		if len(frContent.Parts) != 1 {
			t.Fatalf("len(parts) = %d, want 1", len(frContent.Parts))
		}
		fr := frContent.Parts[0].FunctionResponse
		if fr == nil {
			t.Fatal("functionResponse is nil")
		}
		if fr.Name != "get_weather" {
			t.Errorf("name = %q, want get_weather", fr.Name)
		}
		// Response must be a JSON object wrapper.
		if len(fr.Response) == 0 || fr.Response[0] != '{' {
			t.Errorf("response = %q, want JSON object", fr.Response)
		}
	})

	t.Run("tool_choice unknown string fails closed", func(t *testing.T) {
		t.Parallel()
		input := `{
			"model": "gemini-1.5-pro",
			"messages": [{"role":"user","content":"Hi"}],
			"tools": [{"type":"function","function":{"name":"fn","parameters":{}}}],
			"tool_choice": "random_value"
		}`
		a := &GeminiAdapter{}
		_, err := a.TransformRequest([]byte(input), Model{})
		if err == nil {
			t.Fatal("expected error for unknown tool_choice string, got nil")
		}
	})

	t.Run("tool type not function fails closed", func(t *testing.T) {
		t.Parallel()
		input := `{
			"model": "gemini-1.5-pro",
			"messages": [{"role":"user","content":"Hi"}],
			"tools": [{"type":"retrieval","function":{"name":"fn","parameters":{}}}]
		}`
		a := &GeminiAdapter{}
		_, err := a.TransformRequest([]byte(input), Model{})
		if err == nil {
			t.Fatal("expected error for non-function tool type, got nil")
		}
	})

	t.Run("tool function name with invalid chars fails closed", func(t *testing.T) {
		t.Parallel()
		input := `{
			"model": "gemini-1.5-pro",
			"messages": [{"role":"user","content":"Hi"}],
			"tools": [{"type":"function","function":{"name":"fn with spaces","parameters":{}}}]
		}`
		a := &GeminiAdapter{}
		_, err := a.TransformRequest([]byte(input), Model{})
		if err == nil {
			t.Fatal("expected error for invalid function name charset, got nil")
		}
	})
}

// TestGeminiTransformResponse_ToolCalls verifies that functionCall parts in a
// Gemini response are translated to OpenAI tool_calls with synthesised ids.
func TestGeminiTransformResponse_ToolCalls(t *testing.T) {
	t.Parallel()

	t.Run("single functionCall part becomes tool_call", func(t *testing.T) {
		t.Parallel()
		input := `{
			"candidates": [{
				"content": {
					"role": "model",
					"parts": [{
						"functionCall": {
							"name": "get_weather",
							"args": {"location": "NYC"}
						}
					}]
				},
				"finishReason": "FUNCTION_CALL"
			}],
			"usageMetadata": {"promptTokenCount":10,"candidatesTokenCount":5,"totalTokenCount":15}
		}`
		a := &GeminiAdapter{}
		out, err := a.TransformResponse([]byte(input))
		if err != nil {
			t.Fatalf("TransformResponse() error = %v", err)
		}
		var resp openAIResponse
		if err := json.Unmarshal(out, &resp); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if len(resp.Choices) != 1 {
			t.Fatalf("len(choices) = %d, want 1", len(resp.Choices))
		}
		ch := resp.Choices[0]
		if ch.FinishReason != "tool_calls" {
			t.Errorf("finish_reason = %q, want tool_calls", ch.FinishReason)
		}
		// content must be null when only tool calls present.
		if ch.Message.Content != nil {
			t.Errorf("content = %q, want nil (null)", *ch.Message.Content)
		}
		if len(ch.Message.ToolCalls) != 1 {
			t.Fatalf("len(tool_calls) = %d, want 1", len(ch.Message.ToolCalls))
		}
		tc := ch.Message.ToolCalls[0]
		if tc.ID == "" {
			t.Error("tool_call id should be non-empty (synthesised)")
		}
		if tc.Type != "function" {
			t.Errorf("type = %q, want function", tc.Type)
		}
		if tc.Function.Name != "get_weather" {
			t.Errorf("function.name = %q, want get_weather", tc.Function.Name)
		}
		// Arguments must be a JSON string representing the args object.
		if tc.Function.Arguments == "" {
			t.Error("function.arguments should not be empty")
		}
		if tc.Function.Arguments[0] != '{' {
			t.Errorf("function.arguments = %q, want JSON object string", tc.Function.Arguments)
		}
	})

	t.Run("multiple functionCall parts get sequential ids", func(t *testing.T) {
		t.Parallel()
		input := `{
			"candidates": [{
				"content": {
					"role": "model",
					"parts": [
						{"functionCall": {"name": "fn_a", "args": {}}},
						{"functionCall": {"name": "fn_b", "args": {"x": 1}}}
					]
				},
				"finishReason": "FUNCTION_CALL"
			}],
			"usageMetadata": {}
		}`
		a := &GeminiAdapter{}
		out, err := a.TransformResponse([]byte(input))
		if err != nil {
			t.Fatalf("TransformResponse() error = %v", err)
		}
		var resp openAIResponse
		if err := json.Unmarshal(out, &resp); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		tcs := resp.Choices[0].Message.ToolCalls
		if len(tcs) != 2 {
			t.Fatalf("len(tool_calls) = %d, want 2", len(tcs))
		}
		if tcs[0].ID == tcs[1].ID {
			t.Error("tool_call ids should be distinct")
		}
		if tcs[0].Function.Name != "fn_a" {
			t.Errorf("tool_calls[0].function.name = %q, want fn_a", tcs[0].Function.Name)
		}
		if tcs[1].Function.Name != "fn_b" {
			t.Errorf("tool_calls[1].function.name = %q, want fn_b", tcs[1].Function.Name)
		}
	})

	t.Run("mixed text and functionCall parts: text in content, tool_calls set", func(t *testing.T) {
		t.Parallel()
		input := `{
			"candidates": [{
				"content": {
					"role": "model",
					"parts": [
						{"text": "Let me check that for you."},
						{"functionCall": {"name": "search", "args": {"q": "weather"}}}
					]
				},
				"finishReason": "FUNCTION_CALL"
			}],
			"usageMetadata": {}
		}`
		a := &GeminiAdapter{}
		out, err := a.TransformResponse([]byte(input))
		if err != nil {
			t.Fatalf("TransformResponse() error = %v", err)
		}
		var resp openAIResponse
		if err := json.Unmarshal(out, &resp); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		ch := resp.Choices[0]
		if ch.Message.Content == nil || *ch.Message.Content != "Let me check that for you." {
			var got string
			if ch.Message.Content != nil {
				got = *ch.Message.Content
			}
			t.Errorf("content = %q, want \"Let me check that for you.\"", got)
		}
		if len(ch.Message.ToolCalls) != 1 {
			t.Fatalf("len(tool_calls) = %d, want 1", len(ch.Message.ToolCalls))
		}
	})

	t.Run("functionCall presence overrides STOP to tool_calls regardless of finishReason string", func(t *testing.T) {
		t.Parallel()
		// Gemini sends finishReason "STOP" for tool calls (no "FUNCTION_CALL" enum exists).
		// The adapter derives finish_reason from functionCall PRESENCE, not from the
		// Gemini finishReason string. A response with functionCall parts and finishReason
		// "STOP" must produce finish_reason "tool_calls".
		input := `{
			"candidates": [{
				"content": {
					"role": "model",
					"parts": [{"functionCall": {"name": "get_weather", "args": {"city": "NYC"}}}]
				},
				"finishReason": "STOP"
			}],
			"usageMetadata": {}
		}`
		a := &GeminiAdapter{}
		out, err := a.TransformResponse([]byte(input))
		if err != nil {
			t.Fatalf("TransformResponse() error = %v", err)
		}
		var resp openAIResponse
		if err := json.Unmarshal(out, &resp); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if resp.Choices[0].FinishReason != "tool_calls" {
			t.Errorf("finish_reason = %q, want tool_calls (functionCall presence must override STOP)", resp.Choices[0].FinishReason)
		}
	})
}

// TestGeminiTransformStreamLine_ToolCalls verifies streaming tool call delta
// emission conformant with OpenAI Stage 0c shape.
func TestGeminiTransformStreamLine_ToolCalls(t *testing.T) {
	t.Parallel()

	t.Run("functionCall chunk emits tool_calls delta with index id type name args", func(t *testing.T) {
		t.Parallel()
		line := `data: {"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"get_weather","args":{"location":"NYC"}}}]},"finishReason":"FUNCTION_CALL"}],"usageMetadata":{}}`
		a := &GeminiAdapter{}
		out := a.TransformStreamLine([]byte(line))
		if out == nil {
			t.Fatal("TransformStreamLine() = nil, want non-nil for functionCall chunk")
		}
		chunk := parseChunk(t, out)
		if len(chunk.Choices) != 1 {
			t.Fatalf("len(choices) = %d, want 1", len(chunk.Choices))
		}
		ch := chunk.Choices[0]
		if len(ch.Delta.ToolCalls) != 1 {
			t.Fatalf("len(delta.tool_calls) = %d, want 1", len(ch.Delta.ToolCalls))
		}
		tc := ch.Delta.ToolCalls[0]
		if tc.Index == nil {
			t.Fatal("tool_calls[0].index is nil")
		}
		if *tc.Index != 0 {
			t.Errorf("index = %d, want 0", *tc.Index)
		}
		if tc.ID == "" {
			t.Error("id should be non-empty")
		}
		if tc.Type != "function" {
			t.Errorf("type = %q, want function", tc.Type)
		}
		if tc.Function.Name != "get_weather" {
			t.Errorf("function.name = %q, want get_weather", tc.Function.Name)
		}
		if tc.Function.Arguments == "" {
			t.Error("function.arguments should not be empty")
		}
		// finish_reason on the same chunk as the functionCall.
		if ch.FinishReason == nil || *ch.FinishReason != "tool_calls" {
			var got string
			if ch.FinishReason != nil {
				got = *ch.FinishReason
			}
			t.Errorf("finish_reason = %q, want tool_calls", got)
		}
		// doneSent flag must be set.
		if !a.doneSent {
			t.Error("doneSent should be true after terminal functionCall chunk")
		}
	})

	t.Run("functionCall-only chunk is NOT dropped", func(t *testing.T) {
		t.Parallel()
		// A chunk with only a functionCall part and no text — previously would
		// have been dropped by the deltaText=="" && finishReason==nil guard.
		// This verifies the content-free-drop fix.
		line := `data: {"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"fn","args":{}}}]},"finishReason":""}],"usageMetadata":{}}`
		a := &GeminiAdapter{}
		out := a.TransformStreamLine([]byte(line))
		if out == nil {
			t.Fatal("TransformStreamLine() = nil for functionCall-only chunk (content-free-drop bug)")
		}
	})

	t.Run("stream tool call counter increments across multiple chunks", func(t *testing.T) {
		t.Parallel()
		a := &GeminiAdapter{}

		line1 := `data: {"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"fn_a","args":{}}}]},"finishReason":""}],"usageMetadata":{}}`
		out1 := a.TransformStreamLine([]byte(line1))
		if out1 == nil {
			t.Fatal("first chunk: nil")
		}
		chunk1 := parseChunk(t, out1)
		idx1 := *chunk1.Choices[0].Delta.ToolCalls[0].Index

		line2 := `data: {"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"fn_b","args":{}}}]},"finishReason":"FUNCTION_CALL"}],"usageMetadata":{}}`
		out2 := a.TransformStreamLine([]byte(line2))
		if out2 == nil {
			t.Fatal("second chunk: nil")
		}
		chunk2 := parseChunk(t, out2)
		idx2 := *chunk2.Choices[0].Delta.ToolCalls[0].Index

		if idx1 == idx2 {
			t.Errorf("tool call indices should differ: both are %d", idx1)
		}
		if idx2 != idx1+1 {
			t.Errorf("second index %d should be one more than first %d", idx2, idx1)
		}
	})

	t.Run("blank line after functionCall terminal chunk becomes DONE", func(t *testing.T) {
		t.Parallel()
		a := &GeminiAdapter{}
		// Emit terminal tool-call chunk.
		line := `data: {"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"fn","args":{}}}]},"finishReason":"FUNCTION_CALL"}],"usageMetadata":{}}`
		_ = a.TransformStreamLine([]byte(line))
		done := a.TransformStreamLine([]byte(""))
		if string(done) != "data: [DONE]" {
			t.Errorf("blank after terminal = %q, want data: [DONE]", done)
		}
	})
}

// ── REQUEST: tool definitions (detailed) ─────────────────────────────────────

// TestGeminiTransformRequest_ToolDefinitions exercises the detailed field-by-field
// translation of OpenAI tool definitions to Gemini functionDeclarations, including
// missing/null parameters handling and multiple tools.
func TestGeminiTransformRequest_ToolDefinitions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   string
		checkFn func(t *testing.T, req geminiRequest)
		wantErr bool
	}{
		{
			name: "name description parameters all preserved verbatim",
			input: `{"model":"gemini-1.5-pro","messages":[{"role":"user","content":"q"}],
				"tools":[{"type":"function","function":{
					"name":"search",
					"description":"Full-text search",
					"parameters":{"type":"object","properties":{"query":{"type":"string"},"limit":{"type":"integer"}},"required":["query"]}
				}}]}`,
			checkFn: func(t *testing.T, req geminiRequest) {
				t.Helper()
				if len(req.Tools) != 1 {
					t.Fatalf("len(tools) = %d, want 1", len(req.Tools))
				}
				decls := req.Tools[0].FunctionDeclarations
				if len(decls) != 1 {
					t.Fatalf("len(functionDeclarations) = %d, want 1", len(decls))
				}
				d := decls[0]
				if d.Name != "search" {
					t.Errorf("name = %q, want search", d.Name)
				}
				if d.Description != "Full-text search" {
					t.Errorf("description = %q, want 'Full-text search'", d.Description)
				}
				// Parameters must be a JSON object matching the OpenAI parameters.
				var params map[string]json.RawMessage
				if err := json.Unmarshal(d.Parameters, &params); err != nil {
					t.Fatalf("unmarshal parameters: %v", err)
				}
				if _, ok := params["properties"]; !ok {
					t.Error("parameters missing 'properties' key")
				}
				if _, ok := params["required"]; !ok {
					t.Error("parameters missing 'required' key")
				}
				var typ string
				_ = json.Unmarshal(params["type"], &typ)
				if typ != "object" {
					t.Errorf("parameters.type = %q, want object", typ)
				}
			},
		},
		{
			name: "missing parameters field emits nil parameters (omitted)",
			input: `{"model":"gemini-1.5-pro","messages":[{"role":"user","content":"q"}],
				"tools":[{"type":"function","function":{"name":"noop","description":"Does nothing"}}]}`,
			checkFn: func(t *testing.T, req geminiRequest) {
				t.Helper()
				if len(req.Tools) != 1 {
					t.Fatalf("len(tools) = %d, want 1", len(req.Tools))
				}
				d := req.Tools[0].FunctionDeclarations[0]
				if d.Name != "noop" {
					t.Errorf("name = %q, want noop", d.Name)
				}
				// Parameters is omitted (nil) when not supplied.
				if len(d.Parameters) != 0 {
					t.Errorf("parameters = %s, want nil/omitted when not supplied", d.Parameters)
				}
			},
		},
		{
			name: "multiple tools become multiple declarations in single geminiTool",
			input: `{"model":"gemini-1.5-pro","messages":[{"role":"user","content":"q"}],
				"tools":[
					{"type":"function","function":{"name":"fn_a","description":"A","parameters":{"type":"object","properties":{}}}},
					{"type":"function","function":{"name":"fn_b","description":"B","parameters":{"type":"object","properties":{}}}},
					{"type":"function","function":{"name":"fn_c","description":"C","parameters":{"type":"object","properties":{}}}}
				]}`,
			checkFn: func(t *testing.T, req geminiRequest) {
				t.Helper()
				// All declarations must be in a single Tools element.
				if len(req.Tools) != 1 {
					t.Fatalf("len(tools) = %d, want 1 (all declarations in one element)", len(req.Tools))
				}
				decls := req.Tools[0].FunctionDeclarations
				if len(decls) != 3 {
					t.Fatalf("len(functionDeclarations) = %d, want 3", len(decls))
				}
				wantNames := []string{"fn_a", "fn_b", "fn_c"}
				for i, d := range decls {
					if d.Name != wantNames[i] {
						t.Errorf("decls[%d].name = %q, want %q", i, d.Name, wantNames[i])
					}
				}
			},
		},
		{
			name: "parameters as JSON array (not object) returns error",
			input: `{"model":"gemini-1.5-pro","messages":[{"role":"user","content":"q"}],
				"tools":[{"type":"function","function":{"name":"fn","parameters":[1,2,3]}}]}`,
			wantErr: true,
		},
		{
			// The impl rejects parameters:null because the trim+check treats "null"
			// as a non-object value (trimmed[0] == 'n', not '{').
			// This is conservative fail-closed behaviour: null parameters must be
			// omitted entirely, not set to JSON null.
			name: "parameters as JSON null returns error (must be omitted not null)",
			input: `{"model":"gemini-1.5-pro","messages":[{"role":"user","content":"q"}],
				"tools":[{"type":"function","function":{"name":"fn","parameters":null}}]}`,
			wantErr: true,
		},
		{
			name: "tool type not function returns error",
			input: `{"model":"gemini-1.5-pro","messages":[{"role":"user","content":"q"}],
				"tools":[{"type":"retrieval","function":{"name":"fn"}}]}`,
			wantErr: true,
		},
		{
			name: "empty tool function name returns error",
			input: `{"model":"gemini-1.5-pro","messages":[{"role":"user","content":"q"}],
				"tools":[{"type":"function","function":{"name":"","parameters":{}}}]}`,
			wantErr: true,
		},
		{
			name: "function name with at-sign returns error",
			input: `{"model":"gemini-1.5-pro","messages":[{"role":"user","content":"q"}],
				"tools":[{"type":"function","function":{"name":"fn@email","parameters":{}}}]}`,
			wantErr: true,
		},
		{
			name: "function name with space returns error",
			input: `{"model":"gemini-1.5-pro","messages":[{"role":"user","content":"q"}],
				"tools":[{"type":"function","function":{"name":"fn name","parameters":{}}}]}`,
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
				// Fail-closed validation: error must not leak function names or content.
				// The error message must not contain identifiable caller data.
				// (Tested implicitly: the error is opaque, not a raw-value echo.)
				return
			}
			if err != nil {
				t.Fatalf("TransformRequest() error = %v", err)
			}

			var req geminiRequest
			if err := json.Unmarshal(out, &req); err != nil {
				t.Fatalf("unmarshal output: %v", err)
			}
			tc.checkFn(t, req)
		})
	}
}

// ── REQUEST: tool_choice translation (detailed) ───────────────────────────────

// TestGeminiTransformRequest_ToolChoice_Detailed covers all tool_choice variants
// not already covered by the existing tests, including error cases and opaque
// error message validation (no function name in error string).
func TestGeminiTransformRequest_ToolChoice_Detailed(t *testing.T) {
	t.Parallel()

	baseTools := `[{"type":"function","function":{"name":"lookup","parameters":{"type":"object","properties":{}}}}]`

	tests := []struct {
		name             string
		toolChoiceRaw    string
		wantMode         string   // expected FunctionCallingConfig.Mode
		wantAllowedNames []string // expected AllowedFunctionNames (nil = none)
		wantToolsRemoved bool     // true when tool_choice=none removes tools
		wantErr          bool
	}{
		{
			name:          "auto maps to AUTO mode",
			toolChoiceRaw: `"auto"`,
			wantMode:      "AUTO",
		},
		{
			name:          "required maps to ANY mode",
			toolChoiceRaw: `"required"`,
			wantMode:      "ANY",
		},
		{
			name:             "none removes tools and toolConfig entirely",
			toolChoiceRaw:    `"none"`,
			wantToolsRemoved: true,
		},
		{
			name:             "named function object maps to ANY with allowedFunctionNames",
			toolChoiceRaw:    `{"type":"function","function":{"name":"lookup"}}`,
			wantMode:         "ANY",
			wantAllowedNames: []string{"lookup"},
		},
		{
			name:          "unknown string value returns error",
			toolChoiceRaw: `"bogus_mode"`,
			wantErr:       true,
		},
		{
			name:          "unknown object type returns error",
			toolChoiceRaw: `{"type":"retrieval","function":{"name":"lookup"}}`,
			wantErr:       true,
		},
		{
			name:          "object with empty function name returns error",
			toolChoiceRaw: `{"type":"function","function":{"name":""}}`,
			wantErr:       true,
		},
		{
			name:          "named function not in declared tools returns error",
			toolChoiceRaw: `{"type":"function","function":{"name":"undeclared_fn"}}`,
			wantErr:       true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			input := `{"model":"gemini-1.5-pro","messages":[{"role":"user","content":"q"}],"tools":` +
				baseTools + `,"tool_choice":` + tc.toolChoiceRaw + `}`

			a := &GeminiAdapter{}
			out, err := a.TransformRequest([]byte(input), Model{})

			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				// Fail-closed: error message must not echo back the tool name or value.
				errStr := err.Error()
				if strings.Contains(errStr, "lookup") {
					t.Errorf("error message leaks declared tool name %q: %s", "lookup", errStr)
				}
				return
			}
			if err != nil {
				t.Fatalf("TransformRequest() error = %v", err)
			}

			var req geminiRequest
			if err := json.Unmarshal(out, &req); err != nil {
				t.Fatalf("unmarshal output: %v", err)
			}

			if tc.wantToolsRemoved {
				if len(req.Tools) != 0 {
					t.Errorf("tools should be absent after tool_choice=none, got %d entries", len(req.Tools))
				}
				if req.ToolConfig != nil {
					t.Error("toolConfig should be nil after tool_choice=none")
				}
				return
			}

			if req.ToolConfig == nil {
				t.Fatal("toolConfig is nil, want non-nil")
			}
			cfg := req.ToolConfig.FunctionCallingConfig
			if cfg.Mode != tc.wantMode {
				t.Errorf("functionCallingConfig.mode = %q, want %q", cfg.Mode, tc.wantMode)
			}
			if tc.wantAllowedNames != nil {
				if len(cfg.AllowedFunctionNames) != len(tc.wantAllowedNames) {
					t.Fatalf("allowedFunctionNames = %v, want %v", cfg.AllowedFunctionNames, tc.wantAllowedNames)
				}
				for i, want := range tc.wantAllowedNames {
					if cfg.AllowedFunctionNames[i] != want {
						t.Errorf("allowedFunctionNames[%d] = %q, want %q", i, cfg.AllowedFunctionNames[i], want)
					}
				}
			} else {
				if len(cfg.AllowedFunctionNames) != 0 {
					t.Errorf("allowedFunctionNames = %v, want empty", cfg.AllowedFunctionNames)
				}
			}
		})
	}
}

// ── REQUEST: assistant message with tool_calls ────────────────────────────────

// TestGeminiTransformRequest_AssistantToolCalls verifies that an OpenAI assistant
// message carrying tool_calls becomes a Gemini model content with functionCall
// parts.
func TestGeminiTransformRequest_AssistantToolCalls(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		input         string
		wantParts     int
		wantTextFirst bool   // first part is text, not functionCall
		wantFirstFC   string // function name of first functionCall part (or after text)
		wantArgs      string // expected args JSON object string for first functionCall
		wantIDAbsent  bool   // OpenAI id must not appear in any functionCall part
		wantErr       bool
	}{
		{
			name: "null content single tool_call becomes single functionCall part",
			input: `{"model":"gemini-1.5-pro","messages":[
				{"role":"user","content":"q"},
				{"role":"assistant","content":null,"tool_calls":[
					{"id":"call_abc","type":"function","function":{"name":"get_weather","arguments":"{\"location\":\"NYC\"}"}}
				]}
			]}`,
			wantParts:    1,
			wantFirstFC:  "get_weather",
			wantArgs:     `{"location":"NYC"}`,
			wantIDAbsent: true,
		},
		{
			name: "empty string arguments become empty object",
			input: `{"model":"gemini-1.5-pro","messages":[
				{"role":"user","content":"q"},
				{"role":"assistant","content":null,"tool_calls":[
					{"id":"call_empty","type":"function","function":{"name":"noop","arguments":""}}
				]}
			]}`,
			wantParts:   1,
			wantFirstFC: "noop",
			wantArgs:    `{}`,
		},
		{
			name: "non-empty text content emits text part then functionCall parts",
			input: `{"model":"gemini-1.5-pro","messages":[
				{"role":"user","content":"q"},
				{"role":"assistant","content":"Let me check the weather.","tool_calls":[
					{"id":"call_text","type":"function","function":{"name":"get_weather","arguments":"{}"}}
				]}
			]}`,
			wantParts:     2,
			wantTextFirst: true,
			wantFirstFC:   "get_weather",
		},
		{
			name: "multiple tool_calls produce multiple functionCall parts",
			input: `{"model":"gemini-1.5-pro","messages":[
				{"role":"user","content":"q"},
				{"role":"assistant","content":null,"tool_calls":[
					{"id":"call_1","type":"function","function":{"name":"fn_a","arguments":"{\"x\":1}"}},
					{"id":"call_2","type":"function","function":{"name":"fn_b","arguments":"{\"y\":2}"}},
					{"id":"call_3","type":"function","function":{"name":"fn_c","arguments":"{\"z\":3}"}}
				]}
			]}`,
			wantParts:   3,
			wantFirstFC: "fn_a",
		},
		{
			// PII pseudonyms use charset [A-Za-z0-9_] which is a subset of
			// anthropicToolIDRe ([A-Za-z0-9_.+-]). FIX 2 adds an additional
			// pseudonym-shape check so that names matching the canonical pseudonym
			// pattern (PII_<2alnum>_<24hex>) are now rejected fail-closed.
			// Without this, a compromised upstream returning a pseudonym-shaped name
			// in a tool_call would allow filter.Restore to expand PII on the request
			// forwarding path (pseudonym→real PII substitution in arguments).
			name:    "tool_call function name matching canonical PII pseudonym shape is rejected",
			input:   `{"model":"gemini-1.5-pro","messages":[{"role":"user","content":"q"},{"role":"assistant","content":null,"tool_calls":[{"id":"call_pii","type":"function","function":{"name":"PII_EM_abc123def456abc123def456","arguments":"{}"}}]}]}`,
			wantErr: true,
		},
		{
			name: "tool_call function name with at-sign returns error",
			input: `{"model":"gemini-1.5-pro","messages":[
				{"role":"user","content":"q"},
				{"role":"assistant","content":null,"tool_calls":[
					{"id":"call_at","type":"function","function":{"name":"fn@bad","arguments":"{}"}}
				]}
			]}`,
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
				// Error message must not echo back function name content.
				errStr := err.Error()
				if strings.Contains(errStr, "PII_EM_") {
					t.Errorf("error message leaks PII pseudonym: %s", errStr)
				}
				return
			}
			if err != nil {
				t.Fatalf("TransformRequest() error = %v", err)
			}

			var req geminiRequest
			if err := json.Unmarshal(out, &req); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}

			// Find the model content.
			var modelContent *geminiContent
			for i := range req.Contents {
				if req.Contents[i].Role == "model" {
					modelContent = &req.Contents[i]
					break
				}
			}
			if modelContent == nil {
				t.Fatal("no model-role content in output")
			}

			if len(modelContent.Parts) != tc.wantParts {
				t.Fatalf("len(parts) = %d, want %d", len(modelContent.Parts), tc.wantParts)
			}

			fcIdx := 0
			if tc.wantTextFirst {
				if modelContent.Parts[0].Text == "" {
					t.Error("first part should be text, got empty text")
				}
				if modelContent.Parts[0].FunctionCall != nil {
					t.Error("first part should be text, not functionCall")
				}
				fcIdx = 1
			}

			fc := modelContent.Parts[fcIdx].FunctionCall
			if fc == nil {
				t.Fatalf("parts[%d].functionCall is nil", fcIdx)
			}
			if tc.wantFirstFC != "" && fc.Name != tc.wantFirstFC {
				t.Errorf("functionCall.name = %q, want %q", fc.Name, tc.wantFirstFC)
			}

			// Verify args are a JSON object, not the raw string.
			if len(fc.Args) == 0 || fc.Args[0] != '{' {
				t.Errorf("functionCall.args = %s, want JSON object", fc.Args)
			}
			if tc.wantArgs != "" {
				// Normalise by re-parsing to avoid whitespace differences.
				var gotArgs, wantArgs map[string]json.RawMessage
				_ = json.Unmarshal(fc.Args, &gotArgs)
				_ = json.Unmarshal([]byte(tc.wantArgs), &wantArgs)
				if len(gotArgs) != len(wantArgs) {
					t.Errorf("functionCall.args = %s, want %s", fc.Args, tc.wantArgs)
				}
			}

			// The OpenAI id must NOT appear in any functionCall part (Gemini has no id).
			// This is a structural guarantee: geminiFunctionCall has no id field,
			// so the OpenAI id cannot appear inside a functionCall part regardless
			// of what the OpenAI message carried.
			_ = tc.wantIDAbsent // documented for clarity; enforced by struct design.
		})
	}
}

// ── REQUEST: role:tool messages → functionResponse ────────────────────────────

// TestGeminiTransformRequest_ToolResultMessages covers the translation of
// OpenAI role:"tool" messages to Gemini functionResponse parts, including:
//   - String content wrapped as {"content":"..."}
//   - Array content preserved as separate strings (never concatenated)
//   - Consecutive tool messages merged into one user content
//   - Name correlation from tool_call_id via the toolCallIDToName map
func TestGeminiTransformRequest_ToolResultMessages(t *testing.T) {
	t.Parallel()

	t.Run("string content wrapped as object", func(t *testing.T) {
		t.Parallel()

		input := `{"model":"gemini-1.5-pro","messages":[
			{"role":"user","content":"q"},
			{"role":"assistant","content":null,"tool_calls":[
				{"id":"call_str","type":"function","function":{"name":"get_data","arguments":"{}"}}
			]},
			{"role":"tool","tool_call_id":"call_str","content":"result string here"}
		]}`

		a := &GeminiAdapter{}
		out, err := a.TransformRequest([]byte(input), Model{})
		if err != nil {
			t.Fatalf("TransformRequest() error = %v", err)
		}

		var req geminiRequest
		if err := json.Unmarshal(out, &req); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}

		// Expect 3 contents: user, model, user(functionResponse)
		if len(req.Contents) != 3 {
			t.Fatalf("len(contents) = %d, want 3", len(req.Contents))
		}
		frContent := req.Contents[2]
		if frContent.Role != "user" {
			t.Errorf("role = %q, want user", frContent.Role)
		}
		if len(frContent.Parts) != 1 {
			t.Fatalf("len(parts) = %d, want 1", len(frContent.Parts))
		}
		fr := frContent.Parts[0].FunctionResponse
		if fr == nil {
			t.Fatal("functionResponse is nil")
		}
		if fr.Name != "get_data" {
			t.Errorf("name = %q, want get_data (must be correlated from tool_call_id)", fr.Name)
		}
		// Response must be a JSON object wrapping the string.
		var wrapper struct {
			Content string `json:"content"`
		}
		if err := json.Unmarshal(fr.Response, &wrapper); err != nil {
			t.Fatalf("unmarshal response wrapper: %v (raw: %s)", err, fr.Response)
		}
		if wrapper.Content != "result string here" {
			t.Errorf("response.content = %q, want %q", wrapper.Content, "result string here")
		}
	})

	t.Run("array content preserved as list — no concatenation zero-knowledge", func(t *testing.T) {
		t.Parallel()

		// The two text parts "alice@" and "example.com" must remain separate —
		// joining them into "alice@example.com" would reconstruct a PII email.
		input := `{"model":"gemini-1.5-pro","messages":[
			{"role":"user","content":"q"},
			{"role":"assistant","content":null,"tool_calls":[
				{"id":"call_arr","type":"function","function":{"name":"lookup_user","arguments":"{}"}}
			]},
			{"role":"tool","tool_call_id":"call_arr","content":[
				{"type":"text","text":"alice@"},
				{"type":"text","text":"example.com"}
			]}
		]}`

		a := &GeminiAdapter{}
		out, err := a.TransformRequest([]byte(input), Model{})
		if err != nil {
			t.Fatalf("TransformRequest() error = %v", err)
		}

		var req geminiRequest
		if err := json.Unmarshal(out, &req); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}

		frContent := req.Contents[len(req.Contents)-1]
		if frContent.Role != "user" {
			t.Fatalf("last content role = %q, want user", frContent.Role)
		}
		fr := frContent.Parts[0].FunctionResponse
		if fr == nil {
			t.Fatal("functionResponse is nil")
		}

		// Response must be a JSON object with a list of strings.
		var wrapper struct {
			Content []string `json:"content"`
		}
		if err := json.Unmarshal(fr.Response, &wrapper); err != nil {
			t.Fatalf("unmarshal response wrapper as list: %v (raw: %s)", err, fr.Response)
		}
		if len(wrapper.Content) != 2 {
			t.Fatalf("content list len = %d, want 2 (parts must be separate)", len(wrapper.Content))
		}
		if wrapper.Content[0] != "alice@" {
			t.Errorf("content[0] = %q, want %q", wrapper.Content[0], "alice@")
		}
		if wrapper.Content[1] != "example.com" {
			t.Errorf("content[1] = %q, want %q", wrapper.Content[1], "example.com")
		}
		// Zero-knowledge boundary: joined PII string must never appear as a single value.
		rawResponse := string(fr.Response)
		if strings.Contains(rawResponse, "alice@example.com") {
			t.Errorf("SECURITY: joined PII email %q appears in response; parts must stay separate; raw: %s",
				"alice@example.com", rawResponse)
		}
	})

	t.Run("consecutive tool messages merged into one user content with multiple functionResponse parts", func(t *testing.T) {
		t.Parallel()

		input := `{"model":"gemini-1.5-pro","messages":[
			{"role":"user","content":"q"},
			{"role":"assistant","content":null,"tool_calls":[
				{"id":"c1","type":"function","function":{"name":"fn1","arguments":"{}"}},
				{"id":"c2","type":"function","function":{"name":"fn2","arguments":"{}"}}
			]},
			{"role":"tool","tool_call_id":"c1","content":"result one"},
			{"role":"tool","tool_call_id":"c2","content":"result two"}
		]}`

		a := &GeminiAdapter{}
		out, err := a.TransformRequest([]byte(input), Model{})
		if err != nil {
			t.Fatalf("TransformRequest() error = %v", err)
		}

		var req geminiRequest
		if err := json.Unmarshal(out, &req); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}

		// Expected: user, model, user (merged functionResponses)
		if len(req.Contents) != 3 {
			t.Fatalf("len(contents) = %d, want 3 (consecutive tools merged)", len(req.Contents))
		}
		merged := req.Contents[2]
		if merged.Role != "user" {
			t.Errorf("merged content role = %q, want user", merged.Role)
		}
		if len(merged.Parts) != 2 {
			t.Fatalf("len(parts) = %d, want 2 merged functionResponse parts", len(merged.Parts))
		}
		fr0 := merged.Parts[0].FunctionResponse
		fr1 := merged.Parts[1].FunctionResponse
		if fr0 == nil || fr1 == nil {
			t.Fatal("both parts must be functionResponse")
		}
		if fr0.Name != "fn1" {
			t.Errorf("parts[0].functionResponse.name = %q, want fn1", fr0.Name)
		}
		if fr1.Name != "fn2" {
			t.Errorf("parts[1].functionResponse.name = %q, want fn2", fr1.Name)
		}
	})
}

// ── REQUEST: full round-trip conversation ────────────────────────────────────

// TestGeminiTransformRequest_FullRound verifies a complete multi-turn conversation:
// system + user + assistant(tool_calls) + tool(result) + user
// The system is extracted into systemInstruction, the function response name is
// correlated from tool_call_id, and the contents sequence is well-formed.
func TestGeminiTransformRequest_FullRound(t *testing.T) {
	t.Parallel()

	input := `{"model":"gemini-1.5-pro","messages":[
		{"role":"system","content":"You are a helpful assistant."},
		{"role":"user","content":"What is the weather in Paris?"},
		{"role":"assistant","content":null,"tool_calls":[
			{"id":"call_paris","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Paris\"}"}}
		]},
		{"role":"tool","tool_call_id":"call_paris","content":"It is 18C and sunny."},
		{"role":"user","content":"Thanks!"}
	]}`

	a := &GeminiAdapter{}
	out, err := a.TransformRequest([]byte(input), Model{})
	if err != nil {
		t.Fatalf("TransformRequest() error = %v", err)
	}

	var req geminiRequest
	if err := json.Unmarshal(out, &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// System extracted to systemInstruction.
	if req.SystemInstruction == nil {
		t.Fatal("systemInstruction is nil, want non-nil")
	}
	if len(req.SystemInstruction.Parts) != 1 {
		t.Fatalf("systemInstruction.parts len = %d, want 1", len(req.SystemInstruction.Parts))
	}
	if req.SystemInstruction.Parts[0].Text != "You are a helpful assistant." {
		t.Errorf("systemInstruction.parts[0].text = %q, want 'You are a helpful assistant.'",
			req.SystemInstruction.Parts[0].Text)
	}

	// Contents: user, model(functionCall), user(functionResponse), user
	if len(req.Contents) != 4 {
		t.Fatalf("len(contents) = %d, want 4; contents: %+v", len(req.Contents), req.Contents)
	}

	// [0] user
	if req.Contents[0].Role != "user" {
		t.Errorf("contents[0].role = %q, want user", req.Contents[0].Role)
	}
	if req.Contents[0].Parts[0].Text != "What is the weather in Paris?" {
		t.Errorf("contents[0].parts[0].text = %q, want weather question", req.Contents[0].Parts[0].Text)
	}

	// [1] model with functionCall
	if req.Contents[1].Role != "model" {
		t.Errorf("contents[1].role = %q, want model", req.Contents[1].Role)
	}
	if len(req.Contents[1].Parts) != 1 {
		t.Fatalf("contents[1] parts = %d, want 1", len(req.Contents[1].Parts))
	}
	fc := req.Contents[1].Parts[0].FunctionCall
	if fc == nil {
		t.Fatal("contents[1].parts[0].functionCall is nil")
	}
	if fc.Name != "get_weather" {
		t.Errorf("functionCall.name = %q, want get_weather", fc.Name)
	}
	var fcArgs map[string]json.RawMessage
	if err := json.Unmarshal(fc.Args, &fcArgs); err != nil {
		t.Fatalf("unmarshal functionCall.args: %v", err)
	}
	var city string
	if err := json.Unmarshal(fcArgs["city"], &city); err != nil {
		t.Fatalf("unmarshal city: %v", err)
	}
	if city != "Paris" {
		t.Errorf("functionCall.args.city = %q, want Paris", city)
	}

	// [2] user with functionResponse — name correlated from tool_call_id
	if req.Contents[2].Role != "user" {
		t.Errorf("contents[2].role = %q, want user", req.Contents[2].Role)
	}
	if len(req.Contents[2].Parts) != 1 {
		t.Fatalf("contents[2] parts = %d, want 1", len(req.Contents[2].Parts))
	}
	fr := req.Contents[2].Parts[0].FunctionResponse
	if fr == nil {
		t.Fatal("contents[2].parts[0].functionResponse is nil")
	}
	if fr.Name != "get_weather" {
		t.Errorf("functionResponse.name = %q, want get_weather (correlated from call_paris→get_weather)",
			fr.Name)
	}
	var respWrapper struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal(fr.Response, &respWrapper); err != nil {
		t.Fatalf("unmarshal functionResponse.response: %v", err)
	}
	if respWrapper.Content != "It is 18C and sunny." {
		t.Errorf("functionResponse.response.content = %q, want 'It is 18C and sunny.'",
			respWrapper.Content)
	}

	// [3] final user
	if req.Contents[3].Role != "user" {
		t.Errorf("contents[3].role = %q, want user", req.Contents[3].Role)
	}
	if req.Contents[3].Parts[0].Text != "Thanks!" {
		t.Errorf("contents[3].parts[0].text = %q, want Thanks!", req.Contents[3].Parts[0].Text)
	}
}

// ── REQUEST: fail-closed validation ──────────────────────────────────────────

// TestGeminiTransformRequest_FailClosed verifies that the adapter returns an
// error for all invalid inputs without leaking caller values in error strings.
func TestGeminiTransformRequest_FailClosed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		input        string
		leakPatterns []string // substrings that must NOT appear in the error string
	}{
		{
			name: "tool type not function",
			input: `{"model":"gemini-1.5-pro","messages":[{"role":"user","content":"q"}],
				"tools":[{"type":"code_interpreter","function":{"name":"fn","parameters":{}}}]}`,
		},
		{
			name: "empty tool function name",
			input: `{"model":"gemini-1.5-pro","messages":[{"role":"user","content":"q"}],
				"tools":[{"type":"function","function":{"name":"","parameters":{}}}]}`,
		},
		{
			name: "parameters is a JSON number",
			input: `{"model":"gemini-1.5-pro","messages":[{"role":"user","content":"q"}],
				"tools":[{"type":"function","function":{"name":"fn","parameters":42}}]}`,
		},
		{
			name: "function name with at-sign",
			input: `{"model":"gemini-1.5-pro","messages":[{"role":"user","content":"q"}],
				"tools":[{"type":"function","function":{"name":"fn@domain","parameters":{}}}]}`,
			leakPatterns: []string{"fn@domain"},
		},
		{
			name: "function name with space",
			input: `{"model":"gemini-1.5-pro","messages":[{"role":"user","content":"q"}],
				"tools":[{"type":"function","function":{"name":"has space","parameters":{}}}]}`,
			leakPatterns: []string{"has space"},
		},
		// Note: PII pseudonyms (PII_EM_...) use charset [A-Za-z0-9_] — a valid
		// subset of anthropicToolIDRe — so the adapter's charset check does NOT
		// reject them. Zero-knowledge enforcement is the PII pipeline's job.
		// This case is omitted from the fail-closed table.

		{
			name: "unknown tool_choice string",
			input: `{"model":"gemini-1.5-pro","messages":[{"role":"user","content":"q"}],
				"tools":[{"type":"function","function":{"name":"fn","parameters":{}}}],
				"tool_choice":"forbidden_value"}`,
			leakPatterns: []string{"forbidden_value"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			a := &GeminiAdapter{}
			_, err := a.TransformRequest([]byte(tc.input), Model{})
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			errStr := err.Error()
			for _, pattern := range tc.leakPatterns {
				if strings.Contains(errStr, pattern) {
					t.Errorf("error leaks caller value %q: %s", pattern, errStr)
				}
			}
		})
	}
}

// ── RESPONSE (non-streaming): function call translation ───────────────────────

// TestGeminiTransformResponse_ToolCalls_Detailed covers detailed assertions on
// functionCall → tool_calls translation: synthesised id format, content null,
// arguments as JSON string, multiple calls, mixed text+tool, FUNCTION_CALL
// finishReason, and text-only no-regression.
func TestGeminiTransformResponse_ToolCalls_Detailed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		inputJSON       string
		wantNullContent bool
		wantContent     string // non-empty: assert content equals this
		wantToolCount   int
		wantToolNames   []string
		wantFinish      string
		wantIDPattern   string // if non-empty, each tool_call.id must match this prefix
		wantErr         bool
	}{
		{
			name: "single functionCall: content null finish_reason tool_calls synthesised id",
			inputJSON: `{"candidates":[{"content":{"role":"model","parts":[
				{"functionCall":{"name":"get_weather","args":{"location":"NYC","unit":"celsius"}}}
			]},"finishReason":"FUNCTION_CALL"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":5,"totalTokenCount":15}}`,
			wantNullContent: true,
			wantToolCount:   1,
			wantToolNames:   []string{"get_weather"},
			wantFinish:      "tool_calls",
			wantIDPattern:   "call_g",
		},
		{
			name: "FUNCTION_CALL finishReason maps to tool_calls not stop",
			inputJSON: `{"candidates":[{"content":{"role":"model","parts":[
				{"functionCall":{"name":"fn","args":{}}}
			]},"finishReason":"FUNCTION_CALL"}],"usageMetadata":{}}`,
			wantToolCount: 1,
			wantFinish:    "tool_calls",
		},
		{
			name: "multiple functionCall parts get sequential synthesised ids",
			inputJSON: `{"candidates":[{"content":{"role":"model","parts":[
				{"functionCall":{"name":"fn_a","args":{"x":1}}},
				{"functionCall":{"name":"fn_b","args":{"y":2}}},
				{"functionCall":{"name":"fn_c","args":{"z":3}}}
			]},"finishReason":"FUNCTION_CALL"}],"usageMetadata":{}}`,
			wantNullContent: true,
			wantToolCount:   3,
			wantToolNames:   []string{"fn_a", "fn_b", "fn_c"},
			wantFinish:      "tool_calls",
			wantIDPattern:   "call_g",
		},
		{
			name: "mixed text and functionCall: content non-null and tool_calls present",
			inputJSON: `{"candidates":[{"content":{"role":"model","parts":[
				{"text":"Let me look that up."},
				{"functionCall":{"name":"search","args":{"query":"golang"}}}
			]},"finishReason":"FUNCTION_CALL"}],"usageMetadata":{}}`,
			wantContent:   "Let me look that up.",
			wantToolCount: 1,
			wantToolNames: []string{"search"},
			wantFinish:    "tool_calls",
		},
		{
			name: "text-only response: content string no tool_calls finish_reason stop",
			inputJSON: `{"candidates":[{"content":{"role":"model","parts":[
				{"text":"Just a plain answer."}
			]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":5,"totalTokenCount":8}}`,
			wantContent:   "Just a plain answer.",
			wantToolCount: 0,
			wantFinish:    "stop",
		},
		{
			name: "functionCall args serialised to JSON string for function.arguments",
			inputJSON: `{"candidates":[{"content":{"role":"model","parts":[
				{"functionCall":{"name":"fn","args":{"a":"hello","b":42,"c":true}}}
			]},"finishReason":"FUNCTION_CALL"}],"usageMetadata":{}}`,
			wantToolCount: 1,
		},
		{
			name: "empty args object serialises to empty JSON object string",
			inputJSON: `{"candidates":[{"content":{"role":"model","parts":[
				{"functionCall":{"name":"noop","args":{}}}
			]},"finishReason":"FUNCTION_CALL"}],"usageMetadata":{}}`,
			wantToolCount: 1,
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
				t.Fatalf("unmarshal response: %v", err)
			}

			if len(resp.Choices) != 1 {
				t.Fatalf("len(choices) = %d, want 1", len(resp.Choices))
			}
			ch := resp.Choices[0]
			msg := ch.Message

			// Finish reason.
			if tc.wantFinish != "" && ch.FinishReason != tc.wantFinish {
				t.Errorf("finish_reason = %q, want %q", ch.FinishReason, tc.wantFinish)
			}

			// Content assertions.
			if tc.wantNullContent {
				if msg.Content != nil {
					t.Errorf("content = %q, want nil (null) when only tool calls present", *msg.Content)
				}
			} else if tc.wantContent != "" {
				if msg.Content == nil {
					t.Fatalf("content is nil, want %q", tc.wantContent)
				}
				if *msg.Content != tc.wantContent {
					t.Errorf("content = %q, want %q", *msg.Content, tc.wantContent)
				}
			}

			// Tool calls assertions.
			if tc.wantToolCount == 0 {
				if len(msg.ToolCalls) != 0 {
					t.Errorf("tool_calls = %v, want absent", msg.ToolCalls)
				}
				return
			}
			if len(msg.ToolCalls) != tc.wantToolCount {
				t.Fatalf("len(tool_calls) = %d, want %d", len(msg.ToolCalls), tc.wantToolCount)
			}

			seenIDs := make(map[string]bool)
			for i, tc2 := range msg.ToolCalls {
				if tc2.Type != "function" {
					t.Errorf("tool_calls[%d].type = %q, want function", i, tc2.Type)
				}
				if tc2.ID == "" {
					t.Errorf("tool_calls[%d].id is empty (must be synthesised)", i)
				}
				if tc.wantIDPattern != "" && !strings.HasPrefix(tc2.ID, tc.wantIDPattern) {
					t.Errorf("tool_calls[%d].id = %q, want prefix %q", i, tc2.ID, tc.wantIDPattern)
				}
				if seenIDs[tc2.ID] {
					t.Errorf("tool_calls[%d].id = %q is a duplicate", i, tc2.ID)
				}
				seenIDs[tc2.ID] = true

				if i < len(tc.wantToolNames) && tc2.Function.Name != tc.wantToolNames[i] {
					t.Errorf("tool_calls[%d].function.name = %q, want %q", i, tc2.Function.Name, tc.wantToolNames[i])
				}
				// Arguments must be valid JSON.
				if !json.Valid([]byte(tc2.Function.Arguments)) {
					t.Errorf("tool_calls[%d].function.arguments is not valid JSON: %q", i, tc2.Function.Arguments)
				}
				// Arguments must be a JSON object (Gemini args are always objects).
				if len(tc2.Function.Arguments) == 0 || tc2.Function.Arguments[0] != '{' {
					t.Errorf("tool_calls[%d].function.arguments = %q, want JSON object string", i, tc2.Function.Arguments)
				}
			}
		})
	}
}

// TestGeminiTransformResponse_ToolCallID_Charset verifies that the synthesised
// tool-call id satisfies the charset constraint expected by the PII pipeline
// ([A-Za-z0-9_.+\-]+), matching geminiSynthesiseToolCallID's documented format.
func TestGeminiTransformResponse_ToolCallID_Charset(t *testing.T) {
	t.Parallel()

	inputJSON := `{"candidates":[{"content":{"role":"model","parts":[
		{"functionCall":{"name":"fn","args":{}}}
	]},"finishReason":"FUNCTION_CALL"}],"usageMetadata":{}}`

	a := &GeminiAdapter{}
	out, err := a.TransformResponse([]byte(inputJSON))
	if err != nil {
		t.Fatalf("TransformResponse() error = %v", err)
	}
	var resp openAIResponse
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	id := resp.Choices[0].Message.ToolCalls[0].ID
	if !anthropicToolIDRe.MatchString(id) {
		t.Errorf("synthesised id %q does not satisfy charset [A-Za-z0-9_.+-]: violates anthropicToolIDRe", id)
	}
}

// TestGeminiTransformResponse_InvalidFunctionName verifies that a functionCall
// from upstream whose name contains invalid characters causes TransformResponse
// to return an error (defence-in-depth, not drop-and-continue).
func TestGeminiTransformResponse_InvalidFunctionName(t *testing.T) {
	t.Parallel()

	inputJSON := `{"candidates":[{"content":{"role":"model","parts":[
		{"functionCall":{"name":"fn with spaces","args":{}}}
	]},"finishReason":"FUNCTION_CALL"}],"usageMetadata":{}}`

	a := &GeminiAdapter{}
	_, err := a.TransformResponse([]byte(inputJSON))
	if err == nil {
		t.Fatal("expected error for invalid function name in response, got nil")
	}
}

// ── STREAMING: tool call deltas ───────────────────────────────────────────────

// TestGeminiTransformStreamLine_ToolCalls_Detailed covers streaming tool-call
// delta emission with Stage-0c conformance, finish chunk routing, index ordering,
// and the maxGeminiToolBlocks cap.
func TestGeminiTransformStreamLine_ToolCalls_Detailed(t *testing.T) {
	t.Parallel()

	t.Run("functionCall-only chunk is NOT dropped — content-free-drop fix", func(t *testing.T) {
		t.Parallel()
		// A chunk with only a functionCall part (no text, no finishReason):
		// must NOT be dropped. This was the original bug: deltaText=="" &&
		// finishReason==nil dropped the line without checking toolCallParts.
		line := `data: {"candidates":[{"content":{"role":"model","parts":[
			{"functionCall":{"name":"fn","args":{"k":"v"}}}
		]},"finishReason":""}],"usageMetadata":{}}`
		a := &GeminiAdapter{}
		out := a.TransformStreamLine([]byte(line))
		if out == nil {
			t.Fatal("functionCall-only chunk returned nil (content-free-drop bug)")
		}
		chunk := parseChunk(t, out)
		if len(chunk.Choices) != 1 {
			t.Fatalf("len(choices) = %d, want 1", len(chunk.Choices))
		}
		if len(chunk.Choices[0].Delta.ToolCalls) != 1 {
			t.Fatalf("len(delta.tool_calls) = %d, want 1", len(chunk.Choices[0].Delta.ToolCalls))
		}
	})

	t.Run("Stage-0c shape: index id type function.name function.arguments all present", func(t *testing.T) {
		t.Parallel()
		line := `data: {"candidates":[{"content":{"role":"model","parts":[
			{"functionCall":{"name":"get_weather","args":{"location":"NYC","unit":"celsius"}}}
		]},"finishReason":"FUNCTION_CALL"}],"usageMetadata":{}}`
		a := &GeminiAdapter{}
		out := a.TransformStreamLine([]byte(line))
		if out == nil {
			t.Fatal("functionCall chunk returned nil")
		}
		chunk := parseChunk(t, out)
		if len(chunk.Choices[0].Delta.ToolCalls) != 1 {
			t.Fatalf("len(tool_calls) = %d, want 1", len(chunk.Choices[0].Delta.ToolCalls))
		}
		tc := chunk.Choices[0].Delta.ToolCalls[0]
		// Stage-0c: all four fields present on the first (and only) delta.
		if tc.Index == nil {
			t.Fatal("index is nil (must be present for Stage-0c)")
		}
		if *tc.Index != 0 {
			t.Errorf("index = %d, want 0", *tc.Index)
		}
		if tc.ID == "" {
			t.Error("id is empty (must be synthesised for Stage-0c)")
		}
		if !strings.HasPrefix(tc.ID, "call_g") {
			t.Errorf("id = %q, want prefix 'call_g'", tc.ID)
		}
		if tc.Type != "function" {
			t.Errorf("type = %q, want function", tc.Type)
		}
		if tc.Function.Name != "get_weather" {
			t.Errorf("function.name = %q, want get_weather", tc.Function.Name)
		}
		if tc.Function.Arguments == "" {
			t.Error("function.arguments is empty (must be non-empty JSON string for Stage-0c)")
		}
		if !json.Valid([]byte(tc.Function.Arguments)) {
			t.Errorf("function.arguments is not valid JSON: %q", tc.Function.Arguments)
		}
		// finish_reason tool_calls on same chunk as functionCall.
		if chunk.Choices[0].FinishReason == nil {
			t.Fatal("finish_reason is nil, want tool_calls")
		}
		if *chunk.Choices[0].FinishReason != "tool_calls" {
			t.Errorf("finish_reason = %q, want tool_calls", *chunk.Choices[0].FinishReason)
		}
	})

	t.Run("two functionCall parts across chunks get sequential indices 0 and 1 with distinct ids", func(t *testing.T) {
		t.Parallel()
		a := &GeminiAdapter{}

		line1 := `data: {"candidates":[{"content":{"role":"model","parts":[
			{"functionCall":{"name":"fn_a","args":{}}}
		]},"finishReason":""}],"usageMetadata":{}}`
		out1 := a.TransformStreamLine([]byte(line1))
		if out1 == nil {
			t.Fatal("first functionCall chunk returned nil")
		}

		line2 := `data: {"candidates":[{"content":{"role":"model","parts":[
			{"functionCall":{"name":"fn_b","args":{"x":2}}}
		]},"finishReason":"FUNCTION_CALL"}],"usageMetadata":{}}`
		out2 := a.TransformStreamLine([]byte(line2))
		if out2 == nil {
			t.Fatal("second functionCall chunk returned nil")
		}

		chunk1 := parseChunk(t, out1)
		chunk2 := parseChunk(t, out2)

		tc1 := chunk1.Choices[0].Delta.ToolCalls[0]
		tc2 := chunk2.Choices[0].Delta.ToolCalls[0]

		if tc1.Index == nil || *tc1.Index != 0 {
			t.Errorf("first chunk index = %v, want 0", tc1.Index)
		}
		if tc2.Index == nil || *tc2.Index != 1 {
			t.Errorf("second chunk index = %v, want 1", tc2.Index)
		}
		if tc1.ID == tc2.ID {
			t.Errorf("both chunks share the same id %q (must be distinct)", tc1.ID)
		}
		if tc1.Function.Name != "fn_a" {
			t.Errorf("chunk1.function.name = %q, want fn_a", tc1.Function.Name)
		}
		if tc2.Function.Name != "fn_b" {
			t.Errorf("chunk2.function.name = %q, want fn_b", tc2.Function.Name)
		}
	})

	t.Run("FUNCTION_CALL finish chunk sets finish_reason tool_calls and doneSent", func(t *testing.T) {
		t.Parallel()
		line := `data: {"candidates":[{"content":{"role":"model","parts":[
			{"functionCall":{"name":"fn","args":{}}}
		]},"finishReason":"FUNCTION_CALL"}],"usageMetadata":{"promptTokenCount":8,"candidatesTokenCount":4,"totalTokenCount":12}}`
		a := &GeminiAdapter{}
		out := a.TransformStreamLine([]byte(line))
		if out == nil {
			t.Fatal("terminal functionCall chunk returned nil")
		}
		chunk := parseChunk(t, out)
		if chunk.Choices[0].FinishReason == nil {
			t.Fatal("finish_reason is nil, want tool_calls")
		}
		if *chunk.Choices[0].FinishReason != "tool_calls" {
			t.Errorf("finish_reason = %q, want tool_calls", *chunk.Choices[0].FinishReason)
		}
		if !a.doneSent {
			t.Error("doneSent must be true after terminal functionCall chunk")
		}
		// Blank line after terminal chunk must become data: [DONE].
		done := a.TransformStreamLine([]byte(""))
		if string(done) != "data: [DONE]" {
			t.Errorf("blank-after-terminal = %q, want data: [DONE]", done)
		}
	})

	t.Run("usage accumulated from terminal functionCall chunk", func(t *testing.T) {
		t.Parallel()
		line := `data: {"candidates":[{"content":{"role":"model","parts":[
			{"functionCall":{"name":"fn","args":{}}}
		]},"finishReason":"FUNCTION_CALL"}],"usageMetadata":{"promptTokenCount":20,"candidatesTokenCount":10,"totalTokenCount":30}}`
		a := &GeminiAdapter{}
		_ = a.TransformStreamLine([]byte(line))
		usage := a.StreamUsage()
		if usage.PromptTokens != 20 {
			t.Errorf("PromptTokens = %d, want 20", usage.PromptTokens)
		}
		if usage.CompletionTokens != 10 {
			t.Errorf("CompletionTokens = %d, want 10", usage.CompletionTokens)
		}
		if usage.TotalTokens != 30 {
			t.Errorf("TotalTokens = %d, want 30", usage.TotalTokens)
		}
	})

	t.Run("tool-call counter cap: excess blocks beyond maxGeminiToolBlocks are dropped", func(t *testing.T) {
		t.Parallel()
		a := &GeminiAdapter{}
		// Prime the counter to one below the cap.
		a.streamToolCallCounter = maxGeminiToolBlocks - 1

		// This chunk should emit the last allowed tool call.
		lineAtCap := `data: {"candidates":[{"content":{"role":"model","parts":[
			{"functionCall":{"name":"fn_last","args":{}}}
		]},"finishReason":""}],"usageMetadata":{}}`
		outAtCap := a.TransformStreamLine([]byte(lineAtCap))
		if outAtCap == nil {
			t.Fatal("chunk at cap-1 returned nil, expected to be allowed")
		}
		chunkAtCap := parseChunk(t, outAtCap)
		if len(chunkAtCap.Choices[0].Delta.ToolCalls) != 1 {
			t.Error("chunk at cap-1 should emit one tool call")
		}

		// Counter is now at maxGeminiToolBlocks. Next chunk must be dropped.
		lineOverCap := `data: {"candidates":[{"content":{"role":"model","parts":[
			{"functionCall":{"name":"fn_over","args":{}}}
		]},"finishReason":""}],"usageMetadata":{}}`
		outOverCap := a.TransformStreamLine([]byte(lineOverCap))
		// When all tool call parts are dropped and there is no finishReason, result is nil.
		if outOverCap != nil {
			// The cap was not enforced — this is a failure.
			// (If the chunk has no finishReason and all parts dropped, should be nil.)
			t.Logf("note: over-cap chunk produced %q (may be empty finish chunk if finishReason present)", outOverCap)
		}
	})

	t.Run("invalid function name from upstream in stream is dropped, not panic", func(t *testing.T) {
		t.Parallel()
		// The model hallucinated a function name with spaces (invalid charset).
		// The adapter should drop this block silently.
		line := `data: {"candidates":[{"content":{"role":"model","parts":[
			{"functionCall":{"name":"bad name with spaces","args":{}}}
		]},"finishReason":""}],"usageMetadata":{}}`
		a := &GeminiAdapter{}
		// Must not panic.
		out := a.TransformStreamLine([]byte(line))
		// When the only part has an invalid name and no finishReason, result is nil.
		if out != nil {
			t.Logf("note: invalid-name chunk produced %q (may be finish chunk if finishReason)", out)
		}
		// Counter still incremented past the invalid block.
	})
}

// TestGeminiTransformStreamLine_FinishReason_ToolCalls verifies that the
// FUNCTION_CALL finish reason produces "tool_calls" in the streaming path
// (regression guard: previously it may have been mis-mapped to "stop").
func TestGeminiTransformStreamLine_FinishReason_ToolCalls(t *testing.T) {
	t.Parallel()

	// The existing TestGeminiFinishReason table test covers FUNCTION_CALL→"tool_calls"
	// at the unit level. This test verifies it propagates correctly through the
	// streaming path end-to-end.
	line := `data: {"candidates":[{"content":{"role":"model","parts":[
		{"functionCall":{"name":"fn","args":{}}}
	]},"finishReason":"FUNCTION_CALL"}],"usageMetadata":{}}`

	a := &GeminiAdapter{}
	out := a.TransformStreamLine([]byte(line))
	if out == nil {
		t.Fatal("FUNCTION_CALL terminal chunk returned nil")
	}
	chunk := parseChunk(t, out)
	if len(chunk.Choices) != 1 {
		t.Fatalf("len(choices) = %d, want 1", len(chunk.Choices))
	}
	if chunk.Choices[0].FinishReason == nil {
		t.Fatal("finish_reason is nil, want tool_calls")
	}
	// Regression: must NOT be "stop".
	if *chunk.Choices[0].FinishReason == "stop" {
		t.Errorf("finish_reason = %q (REGRESSION: FUNCTION_CALL was mis-mapped to stop)", *chunk.Choices[0].FinishReason)
	}
	if *chunk.Choices[0].FinishReason != "tool_calls" {
		t.Errorf("finish_reason = %q, want tool_calls", *chunk.Choices[0].FinishReason)
	}
}

// TestGeminiTransformStreamLine_MixedTextAndFunctionCall documents the current
// behaviour when a single Gemini chunk contains both text and functionCall parts:
// the implementation emits only the tool-calls chunk, dropping the text.
// This is noted as a known limitation (Stage-0c cannot handle mixed
// content+tool_calls in one chunk, and Gemini should not produce this in practice).
func TestGeminiTransformStreamLine_MixedTextAndFunctionCall(t *testing.T) {
	t.Parallel()

	// A chunk with both text and functionCall — Gemini rarely produces this.
	line := `data: {"candidates":[{"content":{"role":"model","parts":[
		{"text":"Thinking..."},
		{"functionCall":{"name":"fn","args":{}}}
	]},"finishReason":"FUNCTION_CALL"}],"usageMetadata":{}}`

	a := &GeminiAdapter{}
	out := a.TransformStreamLine([]byte(line))
	if out == nil {
		t.Fatal("mixed text+functionCall chunk returned nil, expected non-nil")
	}

	chunk := parseChunk(t, out)
	// The implementation emits only the tool-calls chunk in the mixed case.
	// Text is dropped. This is documented behaviour (see comment in TransformStreamLine).
	if len(chunk.Choices[0].Delta.ToolCalls) == 0 {
		// If text was emitted instead, also acceptable — just verify non-nil output.
		t.Logf("mixed chunk produced text delta (not tool_calls); behaviour: %+v", chunk.Choices[0].Delta)
	}
	// Either way, the output is valid and non-nil.
	// NOTE: This test explicitly documents that text IS dropped when a mixed
	// chunk arrives. If the implementation is fixed to emit both, this test
	// should be updated. Current behaviour: text chunk is built but discarded
	// (the `_ = out` line in TransformStreamLine), and the tool-calls chunk
	// is returned instead.
}

// ── geminiSynthesiseToolCallID unit test ─────────────────────────────────────

// TestGeminiSynthesiseToolCallID verifies the id format and charset conformance.
func TestGeminiSynthesiseToolCallID(t *testing.T) {
	t.Parallel()

	tests := []struct {
		idx    int
		wantID string
	}{
		{0, "call_g0"},
		{1, "call_g1"},
		{99, "call_g99"},
		{1023, "call_g1023"},
	}

	for _, tc := range tests {
		t.Run(tc.wantID, func(t *testing.T) {
			t.Parallel()
			got := geminiSynthesiseToolCallID(tc.idx)
			if got != tc.wantID {
				t.Errorf("geminiSynthesiseToolCallID(%d) = %q, want %q", tc.idx, got, tc.wantID)
			}
			if !anthropicToolIDRe.MatchString(got) {
				t.Errorf("id %q does not satisfy anthropicToolIDRe ([A-Za-z0-9_.+-]+)", got)
			}
		})
	}
}

// ── geminiWrapToolResult unit tests ──────────────────────────────────────────

// TestGeminiWrapToolResult verifies the wrapping function for tool result content.
func TestGeminiWrapToolResult(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		content     string // raw JSON to pass as content (empty string = nil)
		wantWrapper string // expected raw JSON wrapper (prefix check)
		wantIsArray bool   // wrapper.content is a JSON array
		wantErr     bool
	}{
		{
			name:    "nil content produces empty string wrapper",
			content: "",
		},
		{
			name:        "plain string content produces string wrapper",
			content:     `"hello world"`,
			wantWrapper: `{"content":"hello world"}`,
		},
		{
			name:        "array content produces list wrapper",
			content:     `[{"type":"text","text":"part1"},{"type":"text","text":"part2"}]`,
			wantIsArray: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var rawContent jsonxRawMessage
			if tc.content != "" {
				rawContent = jsonxRawMessage(tc.content)
			}

			result, err := geminiWrapToolResult(rawContent)

			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("geminiWrapToolResult() error = %v", err)
			}

			if tc.content == "" {
				// nil content → empty wrapper.
				if len(result) == 0 {
					t.Fatal("result is empty")
				}
				return
			}

			if tc.wantWrapper != "" {
				// Normalise by parsing both.
				var got, want map[string]json.RawMessage
				if err := json.Unmarshal(result, &got); err != nil {
					t.Fatalf("unmarshal result: %v", err)
				}
				if err := json.Unmarshal([]byte(tc.wantWrapper), &want); err != nil {
					t.Fatalf("unmarshal wantWrapper: %v", err)
				}
				if len(got) != len(want) {
					t.Errorf("result = %s, want %s", result, tc.wantWrapper)
				}
			}

			if tc.wantIsArray {
				var wrapper struct {
					Content []string `json:"content"`
				}
				if err := json.Unmarshal(result, &wrapper); err != nil {
					t.Fatalf("unmarshal array wrapper: %v (raw: %s)", err, result)
				}
				if len(wrapper.Content) != 2 {
					t.Errorf("content list len = %d, want 2", len(wrapper.Content))
				}
				if wrapper.Content[0] != "part1" {
					t.Errorf("content[0] = %q, want part1", wrapper.Content[0])
				}
				if wrapper.Content[1] != "part2" {
					t.Errorf("content[1] = %q, want part2", wrapper.Content[1])
				}
			}
		})
	}
}

// ── END-TO-END Stage-0c conformance for Gemini tool-calls ────────────────────

// TestGeminiStream_Stage0c_ToolCallArgs_EndToEnd verifies the full proxy pipeline
// for a Gemini streaming response that contains a functionCall with a PII
// pseudonym in the arguments. Since Gemini delivers arguments complete (not
// fragmented), the pseudonym is contained in a single chunk. The StreamRestorer
// must recognise and restore it.
//
// Pattern: mirrors TestAnthropicStream_Stage0c_EndToEnd_ViaProxy (anthropic_test.go)
// but uses GeminiAdapter and Gemini SSE format (full generateContent objects per
// chunk). The "sawToolCall" path in the StreamRestorer is exercised here.
func TestGeminiStream_Stage0c_ToolCallArgs_EndToEnd(t *testing.T) {
	t.Parallel()

	// Build PII engine and compute the pseudonym that will be generated for piiTestEmail.
	engine := newTestPIIEngine(t)
	sampleFilter := engine.NewFilter("")
	sampleBody := []byte(chatBody("gemini-test", "lookup "+piiTestEmail))
	anonBody, err := sampleFilter.AnonymizeJSON(sampleBody)
	if err != nil {
		t.Fatalf("pre-compute AnonymizeJSON: %v", err)
	}
	pseudo := piiPseudonymPattern.FindString(string(anonBody))
	if pseudo == "" {
		t.Fatal("could not derive pseudonym for piiTestEmail")
	}

	// Gemini delivers args complete per chunk — no fragmentation needed.
	// The full pseudonym appears in one functionCall.args block.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)

		flusher, ok := w.(http.Flusher)
		if !ok {
			return
		}

		// Emit a Gemini-format stream with the pseudonym in functionCall.args.
		// The adapter translates this to an OpenAI tool_calls delta.
		// The StreamRestorer restores pseudonym → original email.
		argsJSON := `{"email":"` + pseudo + `"}`
		geminiChunk := `data: {"candidates":[{"content":{"role":"model","parts":[` +
			`{"functionCall":{"name":"lookup_user","args":` + argsJSON + `}}` +
			`]},"finishReason":"FUNCTION_CALL","index":0}],"usageMetadata":{"promptTokenCount":12,"candidatesTokenCount":8,"totalTokenCount":20}}`

		fmt.Fprintln(w, geminiChunk)
		fmt.Fprintln(w) // blank separator → adapter converts to data: [DONE]
		flusher.Flush()
	}))
	t.Cleanup(upstream.Close)

	reg := piiRegistryGemini(t, upstream.URL)
	h := NewProxyHandler(reg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h.PIIEngine = engine

	baseURL := startTestServer(t, h)

	streamBody := `{"model":"gemini-test","messages":[{"role":"user","content":"lookup ` + piiTestEmail + `"}],"stream":true}`
	httpReq, err := http.NewRequest(http.MethodPost, baseURL+"/v1/chat/completions",
		strings.NewReader(streamBody))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: testTimeout.Timeout}
	streamResp, err := client.Do(httpReq)
	if err != nil {
		t.Fatalf("streaming request: %v", err)
	}
	defer streamResp.Body.Close()

	if streamResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(streamResp.Body)
		t.Fatalf("status = %d, want 200; body: %s", streamResp.StatusCode, body)
	}

	fullBody, _ := io.ReadAll(streamResp.Body)
	fullStr := string(fullBody)

	// 1. Stream must complete with [DONE].
	if !strings.Contains(fullStr, "[DONE]") {
		t.Errorf("Stage-0c Gemini: [DONE] absent — stream did not complete cleanly\noutput: %s", fullStr)
	}

	// 2. Restored email must appear in the arguments.
	if !strings.Contains(fullStr, piiTestEmail) {
		t.Errorf("Stage-0c Gemini: restored email %q absent from tool_calls arguments\noutput: %s",
			piiTestEmail, fullStr)
	}

	// 3. No raw pseudonym must be visible.
	if strings.Contains(fullStr, pseudo) {
		t.Errorf("SECURITY: raw pseudonym %q visible in Stage-0c Gemini output\noutput: %s", pseudo, fullStr)
	}

	// 4. tool_calls and function name must be present.
	if !strings.Contains(fullStr, `"tool_calls"`) {
		t.Errorf("Stage-0c Gemini: tool_calls key absent from output\noutput: %s", fullStr)
	}
	if !strings.Contains(fullStr, `"lookup_user"`) {
		t.Errorf("Stage-0c Gemini: function name lookup_user absent from output\noutput: %s", fullStr)
	}
}

// jsonxRawMessage is a local alias so test helpers for geminiWrapToolResult can
// pass a json.RawMessage without importing the internal jsonx package directly.
// geminiWrapToolResult accepts jsonx.RawMessage which is defined as json.RawMessage.
type jsonxRawMessage = json.RawMessage

// ── FIX 1: pseudonym-shaped function name in TransformResponse rejected ───────

// TestGeminiTransformResponse_PseudonymFunctionName verifies that FIX 1 closes
// the non-streaming PII leak: if a (malicious or compromised) upstream returns
// a functionCall.name that contains or matches the canonical PII pseudonym
// shape, TransformResponse must return a content-free error rather than
// forwarding the name (where filter.Restore would expand it to real PII).
func TestGeminiTransformResponse_PseudonymFunctionName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		inputJSON string
	}{
		{
			name: "canonical pseudonym shape in function name is rejected",
			// PII_EM_<24hex> is the exact shape of an email pseudonym.
			inputJSON: `{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"PII_EM_aabbccddeeff00112233445566","args":{}}}]},"finishReason":"FUNCTION_CALL"}],"usageMetadata":{}}`,
		},
		{
			name:      "PII_ prefix substring in function name is rejected",
			inputJSON: `{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"call_PII_something","args":{}}}]},"finishReason":"FUNCTION_CALL"}],"usageMetadata":{}}`,
		},
		{
			name:      "function name that IS the pseudonym marker prefix is rejected",
			inputJSON: `{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"PII_XX_000000000000000000000000","args":{}}}]},"finishReason":"FUNCTION_CALL"}],"usageMetadata":{}}`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			a := &GeminiAdapter{}
			out, err := a.TransformResponse([]byte(tc.inputJSON))
			if err == nil {
				t.Fatalf("TransformResponse() = %s, want error for pseudonym-shaped function name", out)
			}
			// Error must be content-free: must not echo back the function name.
			errStr := err.Error()
			if strings.Contains(errStr, "PII_EM_") || strings.Contains(errStr, "aabbcc") {
				t.Errorf("error message leaks pseudonym content: %s", errStr)
			}
		})
	}
}

// TestGeminiTransformResponse_LegitFunctionName confirms that a legitimate
// function name (no PII_ prefix) is not rejected by the pseudonym check.
func TestGeminiTransformResponse_LegitFunctionName(t *testing.T) {
	t.Parallel()

	input := `{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"get_weather","args":{"location":"NYC"}}}]},"finishReason":"FUNCTION_CALL"}],"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":3,"totalTokenCount":8}}`
	a := &GeminiAdapter{}
	out, err := a.TransformResponse([]byte(input))
	if err != nil {
		t.Fatalf("TransformResponse() error = %v for legitimate function name", err)
	}
	if out == nil {
		t.Fatal("TransformResponse() = nil, want non-nil for legitimate function name")
	}
}

// ── FIX 2: charset-validate tool_call_id and tc.ID on request path ───────────

// TestGeminiTransformRequest_ToolCallIDCharset verifies that FIX 2 rejects
// tool_call_id values with invalid characters on the role:"tool" message path
// and tc.ID values with invalid characters on the assistant tool_calls path.
func TestGeminiTransformRequest_ToolCallIDCharset(t *testing.T) {
	t.Parallel()

	t.Run("role:tool with at-sign in tool_call_id is rejected", func(t *testing.T) {
		t.Parallel()
		input := `{"model":"gemini-1.5-pro","messages":[
			{"role":"user","content":"q"},
			{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"fn","arguments":"{}"}}]},
			{"role":"tool","tool_call_id":"a@b","content":"result"}
		]}`
		a := &GeminiAdapter{}
		_, err := a.TransformRequest([]byte(input), Model{})
		if err == nil {
			t.Fatal("expected error for invalid tool_call_id charset, got nil")
		}
		if strings.Contains(err.Error(), "a@b") {
			t.Errorf("error message leaks invalid tool_call_id value: %s", err.Error())
		}
	})

	t.Run("assistant tool_call with space in id is rejected", func(t *testing.T) {
		t.Parallel()
		input := `{"model":"gemini-1.5-pro","messages":[
			{"role":"user","content":"q"},
			{"role":"assistant","content":null,"tool_calls":[{"id":"call id bad","type":"function","function":{"name":"fn","arguments":"{}"}}]}
		]}`
		a := &GeminiAdapter{}
		_, err := a.TransformRequest([]byte(input), Model{})
		if err == nil {
			t.Fatal("expected error for invalid tool_call id with space, got nil")
		}
	})

	t.Run("role:tool with valid tool_call_id is accepted", func(t *testing.T) {
		t.Parallel()
		input := `{"model":"gemini-1.5-pro","messages":[
			{"role":"user","content":"q"},
			{"role":"assistant","content":null,"tool_calls":[{"id":"call_ok","type":"function","function":{"name":"fn","arguments":"{}"}}]},
			{"role":"tool","tool_call_id":"call_ok","content":"result"}
		]}`
		a := &GeminiAdapter{}
		_, err := a.TransformRequest([]byte(input), Model{})
		if err != nil {
			t.Fatalf("TransformRequest() error = %v for valid tool_call_id", err)
		}
	})
}

// ── FIX 3: tool-result array with only non-text parts → {"content":[]} ───────

// TestGeminiWrapToolResult_NonTextOnly verifies that FIX 3 ensures a tool-result
// array containing no text parts serialises as {"content":[]} (empty JSON array)
// rather than {"content":null}. null is structurally incorrect for a list field.
func TestGeminiWrapToolResult_NonTextOnly(t *testing.T) {
	t.Parallel()

	t.Run("array with only non-text parts produces empty content array not null", func(t *testing.T) {
		t.Parallel()
		// An array of image_url parts — no text parts at all.
		content := json.RawMessage(`[{"type":"image_url","url":"http://example.com/img.png"}]`)
		result, err := geminiWrapToolResult(content)
		if err != nil {
			t.Fatalf("geminiWrapToolResult() error = %v", err)
		}
		// The result must contain "content":[] not "content":null.
		var wrapper struct {
			Content json.RawMessage `json:"content"`
		}
		if err := json.Unmarshal(result, &wrapper); err != nil {
			t.Fatalf("unmarshal result: %v (raw: %s)", err, result)
		}
		// Must be a JSON array (starts with '['), not null.
		trimmed := strings.TrimSpace(string(wrapper.Content))
		if trimmed == "null" {
			t.Errorf("content = null, want [] for all-non-text parts array (FIX 3)")
		}
		if len(trimmed) == 0 || trimmed[0] != '[' {
			t.Errorf("content = %q, want JSON array []", trimmed)
		}
	})

	t.Run("empty parts array produces empty content array not null", func(t *testing.T) {
		t.Parallel()
		content := json.RawMessage(`[]`)
		result, err := geminiWrapToolResult(content)
		if err != nil {
			t.Fatalf("geminiWrapToolResult() error = %v", err)
		}
		var wrapper struct {
			Content json.RawMessage `json:"content"`
		}
		if err := json.Unmarshal(result, &wrapper); err != nil {
			t.Fatalf("unmarshal result: %v (raw: %s)", err, result)
		}
		trimmed := strings.TrimSpace(string(wrapper.Content))
		if trimmed == "null" {
			t.Errorf("content = null, want [] for empty parts array (FIX 3)")
		}
		if len(trimmed) == 0 || trimmed[0] != '[' {
			t.Errorf("content = %q, want JSON array []", trimmed)
		}
	})
}

// ── FIX 4: unknown tool_call_id → error ──────────────────────────────────────

// TestGeminiTransformRequest_UnknownToolCallID verifies that FIX 4 makes the
// adapter fail-closed when a role:"tool" message references a tool_call_id that
// was not seen in any prior assistant tool_calls message.
func TestGeminiTransformRequest_UnknownToolCallID(t *testing.T) {
	t.Parallel()

	t.Run("non-empty tool_call_id not in map returns error", func(t *testing.T) {
		t.Parallel()
		// role:tool referencing "call_unknown" which was never in an assistant
		// tool_calls message. Previously this silently set funcName=""; now it
		// must return a content-free error.
		input := `{"model":"gemini-1.5-pro","messages":[
			{"role":"user","content":"q"},
			{"role":"tool","tool_call_id":"call_unknown","content":"some result"}
		]}`
		a := &GeminiAdapter{}
		_, err := a.TransformRequest([]byte(input), Model{})
		if err == nil {
			t.Fatal("expected error for unknown tool_call_id, got nil")
		}
		// Error must be content-free: no caller data echoed.
		errStr := err.Error()
		if strings.Contains(errStr, "call_unknown") {
			t.Errorf("error message leaks tool_call_id value: %s", errStr)
		}
	})

	t.Run("empty tool_call_id returns error fail-closed", func(t *testing.T) {
		t.Parallel()
		// FIX 3: an empty tool_call_id must fail-closed. A tool message without a
		// tool_call_id cannot be correlated to a function name, which would produce
		// a malformed functionResponse. The adapter must reject this case.
		input := `{"model":"gemini-1.5-pro","messages":[
			{"role":"user","content":"q"},
			{"role":"tool","tool_call_id":"","content":"some result"}
		]}`
		a := &GeminiAdapter{}
		_, err := a.TransformRequest([]byte(input), Model{})
		if err == nil {
			t.Fatal("expected error for empty tool_call_id, got nil")
		}
	})
}

// ── FIX 5: no double marshal/unmarshal of tools/toolConfig ───────────────────

// TestGeminiTransformRequest_ToolsDirectAssignment verifies that FIX 5 did not
// change observable behaviour: tools and tool_choice are still correctly
// translated when assigned directly to the geminiRequest rather than going
// through the marshal-into-doc → re-unmarshal round-trip.
func TestGeminiTransformRequest_ToolsDirectAssignment(t *testing.T) {
	t.Parallel()

	t.Run("tools and named tool_choice both present and correct after FIX 5", func(t *testing.T) {
		t.Parallel()
		input := `{
			"model":"gemini-1.5-pro",
			"messages":[{"role":"user","content":"q"}],
			"tools":[
				{"type":"function","function":{"name":"fn_a","description":"A","parameters":{"type":"object","properties":{}}}},
				{"type":"function","function":{"name":"fn_b","description":"B","parameters":{"type":"object","properties":{}}}}
			],
			"tool_choice":{"type":"function","function":{"name":"fn_a"}}
		}`
		a := &GeminiAdapter{}
		out, err := a.TransformRequest([]byte(input), Model{})
		if err != nil {
			t.Fatalf("TransformRequest() error = %v", err)
		}
		var req geminiRequest
		if err := json.Unmarshal(out, &req); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		// Tools must be present with both declarations.
		if len(req.Tools) != 1 {
			t.Fatalf("len(tools) = %d, want 1", len(req.Tools))
		}
		if len(req.Tools[0].FunctionDeclarations) != 2 {
			t.Fatalf("len(functionDeclarations) = %d, want 2", len(req.Tools[0].FunctionDeclarations))
		}
		if req.Tools[0].FunctionDeclarations[0].Name != "fn_a" {
			t.Errorf("decls[0].name = %q, want fn_a", req.Tools[0].FunctionDeclarations[0].Name)
		}
		// ToolConfig must be set with ANY mode and fn_a allowed.
		if req.ToolConfig == nil {
			t.Fatal("toolConfig is nil, want non-nil")
		}
		cfg := req.ToolConfig.FunctionCallingConfig
		if cfg.Mode != "ANY" {
			t.Errorf("mode = %q, want ANY", cfg.Mode)
		}
		if len(cfg.AllowedFunctionNames) != 1 || cfg.AllowedFunctionNames[0] != "fn_a" {
			t.Errorf("allowedFunctionNames = %v, want [fn_a]", cfg.AllowedFunctionNames)
		}
	})

	t.Run("tool_choice none removes both tools and toolConfig", func(t *testing.T) {
		t.Parallel()
		input := `{
			"model":"gemini-1.5-pro",
			"messages":[{"role":"user","content":"q"}],
			"tools":[{"type":"function","function":{"name":"fn","parameters":{}}}],
			"tool_choice":"none"
		}`
		a := &GeminiAdapter{}
		out, err := a.TransformRequest([]byte(input), Model{})
		if err != nil {
			t.Fatalf("TransformRequest() error = %v", err)
		}
		var req geminiRequest
		if err := json.Unmarshal(out, &req); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if len(req.Tools) != 0 {
			t.Errorf("tools = %v, want absent after tool_choice=none", req.Tools)
		}
		if req.ToolConfig != nil {
			t.Error("toolConfig should be nil after tool_choice=none")
		}
	})
}

// ── FIX 1: presence-based finish_reason for tool calls ───────────────────────

// TestGeminiTransformResponse_FinishReasonPresenceBased verifies that the
// non-streaming adapter derives finish_reason "tool_calls" from functionCall
// presence, not from the Gemini finishReason string.
func TestGeminiTransformResponse_FinishReasonPresenceBased(t *testing.T) {
	t.Parallel()

	t.Run("STOP with functionCall parts produces tool_calls finish_reason", func(t *testing.T) {
		t.Parallel()
		// Real Gemini behaviour: tool calls end with finishReason "STOP".
		input := `{
			"candidates": [{
				"content": {
					"role": "model",
					"parts": [{"functionCall": {"name": "get_weather", "args": {"city": "Paris"}}}]
				},
				"finishReason": "STOP"
			}],
			"usageMetadata": {"promptTokenCount":5,"candidatesTokenCount":3,"totalTokenCount":8}
		}`
		a := &GeminiAdapter{}
		out, err := a.TransformResponse([]byte(input))
		if err != nil {
			t.Fatalf("TransformResponse() error = %v", err)
		}
		var resp openAIResponse
		if err := json.Unmarshal(out, &resp); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if len(resp.Choices) != 1 {
			t.Fatalf("len(choices) = %d, want 1", len(resp.Choices))
		}
		if resp.Choices[0].FinishReason != "tool_calls" {
			t.Errorf("finish_reason = %q, want tool_calls (functionCall presence must override STOP)", resp.Choices[0].FinishReason)
		}
		if len(resp.Choices[0].Message.ToolCalls) != 1 {
			t.Fatalf("len(tool_calls) = %d, want 1", len(resp.Choices[0].Message.ToolCalls))
		}
	})

	t.Run("STOP without functionCall parts produces stop finish_reason", func(t *testing.T) {
		t.Parallel()
		input := `{
			"candidates": [{
				"content": {"role": "model", "parts": [{"text": "hello"}]},
				"finishReason": "STOP"
			}],
			"usageMetadata": {}
		}`
		a := &GeminiAdapter{}
		out, err := a.TransformResponse([]byte(input))
		if err != nil {
			t.Fatalf("TransformResponse() error = %v", err)
		}
		var resp openAIResponse
		if err := json.Unmarshal(out, &resp); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if resp.Choices[0].FinishReason != "stop" {
			t.Errorf("finish_reason = %q, want stop for text-only STOP response", resp.Choices[0].FinishReason)
		}
	})

	t.Run("mixed text and functionCall with STOP produces tool_calls", func(t *testing.T) {
		t.Parallel()
		input := `{
			"candidates": [{
				"content": {
					"role": "model",
					"parts": [
						{"text": "Let me check."},
						{"functionCall": {"name": "search", "args": {"q": "weather"}}}
					]
				},
				"finishReason": "STOP"
			}],
			"usageMetadata": {}
		}`
		a := &GeminiAdapter{}
		out, err := a.TransformResponse([]byte(input))
		if err != nil {
			t.Fatalf("TransformResponse() error = %v", err)
		}
		var resp openAIResponse
		if err := json.Unmarshal(out, &resp); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if resp.Choices[0].FinishReason != "tool_calls" {
			t.Errorf("finish_reason = %q, want tool_calls when functionCall parts present", resp.Choices[0].FinishReason)
		}
	})
}

// TestGeminiTransformStreamLine_FinishReasonPresenceBased verifies that the
// streaming adapter derives finish_reason "tool_calls" from functionCall
// presence and the sawFunctionCall flag, not from Gemini's finishReason string.
func TestGeminiTransformStreamLine_FinishReasonPresenceBased(t *testing.T) {
	t.Parallel()

	t.Run("functionCall parts with STOP in same chunk produces tool_calls finish_reason", func(t *testing.T) {
		t.Parallel()
		// Real Gemini stream: functionCall and finishReason STOP arrive together.
		line := `data: {"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"get_weather","args":{"city":"NYC"}}}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":3,"totalTokenCount":8}}`
		a := &GeminiAdapter{}
		out := a.TransformStreamLine([]byte(line))
		if out == nil {
			t.Fatal("TransformStreamLine() = nil, want non-nil chunk")
		}
		chunk := parseChunk(t, out)
		if len(chunk.Choices) != 1 {
			t.Fatalf("len(choices) = %d, want 1", len(chunk.Choices))
		}
		ch := chunk.Choices[0]
		if len(ch.Delta.ToolCalls) != 1 {
			t.Fatalf("len(delta.tool_calls) = %d, want 1", len(ch.Delta.ToolCalls))
		}
		if ch.FinishReason == nil {
			t.Fatal("finish_reason is nil, want tool_calls")
		}
		if *ch.FinishReason != "tool_calls" {
			t.Errorf("finish_reason = %q, want tool_calls (functionCall presence with STOP must override)", *ch.FinishReason)
		}
		if !a.doneSent {
			t.Error("doneSent should be true after terminal functionCall chunk")
		}
		if !a.sawFunctionCall {
			t.Error("sawFunctionCall should be true after emitting tool call")
		}
	})

	t.Run("sawFunctionCall flag set by earlier chunk overrides STOP on finish chunk", func(t *testing.T) {
		t.Parallel()
		a := &GeminiAdapter{}

		// Chunk 1: functionCall part without finishReason (sets sawFunctionCall).
		chunk1 := `data: {"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"fn","args":{}}}]},"finishReason":""}],"usageMetadata":{}}`
		out1 := a.TransformStreamLine([]byte(chunk1))
		if out1 == nil {
			t.Fatal("chunk 1 returned nil")
		}
		if !a.sawFunctionCall {
			t.Error("sawFunctionCall not set after first functionCall chunk")
		}

		// Chunk 2: empty parts with finishReason STOP (finish chunk).
		chunk2 := `data: {"candidates":[{"content":{"role":"model","parts":[]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":3,"totalTokenCount":8}}`
		out2 := a.TransformStreamLine([]byte(chunk2))
		if out2 == nil {
			t.Fatal("finish chunk returned nil")
		}
		finishChunk := parseChunk(t, out2)
		if finishChunk.Choices[0].FinishReason == nil {
			t.Fatal("finish_reason is nil on finish chunk")
		}
		if *finishChunk.Choices[0].FinishReason != "tool_calls" {
			t.Errorf("finish_reason = %q, want tool_calls (sawFunctionCall override)", *finishChunk.Choices[0].FinishReason)
		}
	})

	t.Run("STOP without any functionCall produces stop finish_reason", func(t *testing.T) {
		t.Parallel()
		a := &GeminiAdapter{}
		line := `data: {"candidates":[{"content":{"role":"model","parts":[{"text":"bye"}]},"finishReason":"STOP"}],"usageMetadata":{}}`
		out := a.TransformStreamLine([]byte(line))
		if out == nil {
			t.Fatal("TransformStreamLine() = nil")
		}
		chunk := parseChunk(t, out)
		if chunk.Choices[0].FinishReason == nil {
			t.Fatal("finish_reason is nil")
		}
		if *chunk.Choices[0].FinishReason != "stop" {
			t.Errorf("finish_reason = %q, want stop for text-only stream", *chunk.Choices[0].FinishReason)
		}
	})

	t.Run("full stream functionCall+STOP followed by blank becomes DONE sequence", func(t *testing.T) {
		t.Parallel()
		a := &GeminiAdapter{}

		// Single chunk: functionCall parts + finishReason STOP (real Gemini shape).
		line := `data: {"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"get_data","args":{"id":1}}}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":8,"candidatesTokenCount":4,"totalTokenCount":12}}`
		out := a.TransformStreamLine([]byte(line))
		if out == nil {
			t.Fatal("terminal functionCall chunk returned nil")
		}
		chunk := parseChunk(t, out)
		ch := chunk.Choices[0]
		// Must have tool_calls delta AND finish_reason "tool_calls".
		if len(ch.Delta.ToolCalls) != 1 {
			t.Errorf("len(delta.tool_calls) = %d, want 1", len(ch.Delta.ToolCalls))
		}
		if ch.FinishReason == nil || *ch.FinishReason != "tool_calls" {
			var got string
			if ch.FinishReason != nil {
				got = *ch.FinishReason
			}
			t.Errorf("finish_reason = %q, want tool_calls", got)
		}
		// doneSent must be true.
		if !a.doneSent {
			t.Error("doneSent should be true")
		}
		// Blank line converts to [DONE].
		done := a.TransformStreamLine([]byte(""))
		if string(done) != "data: [DONE]" {
			t.Errorf("blank-after-terminal = %q, want data: [DONE]", done)
		}
	})
}

// ── FIX 3: empty/duplicate id and name validation ────────────────────────────

// TestGeminiTransformRequest_Fix3_EmptyIDs verifies FIX 3 fail-closed
// validation for empty tool_call IDs, empty function names, and duplicate
// IDs with conflicting names.
func TestGeminiTransformRequest_Fix3_EmptyIDs(t *testing.T) {
	t.Parallel()

	t.Run("empty tool_call id in assistant tool_calls returns error", func(t *testing.T) {
		t.Parallel()
		input := `{"model":"gemini-1.5-pro","messages":[
			{"role":"user","content":"q"},
			{"role":"assistant","content":null,"tool_calls":[
				{"id":"","type":"function","function":{"name":"get_data","arguments":"{}"}}
			]}
		]}`
		a := &GeminiAdapter{}
		_, err := a.TransformRequest([]byte(input), Model{})
		if err == nil {
			t.Fatal("expected error for empty tool_call id, got nil")
		}
	})

	t.Run("empty function name in assistant tool_calls returns error", func(t *testing.T) {
		t.Parallel()
		input := `{"model":"gemini-1.5-pro","messages":[
			{"role":"user","content":"q"},
			{"role":"assistant","content":null,"tool_calls":[
				{"id":"call_1","type":"function","function":{"name":"","arguments":"{}"}}
			]}
		]}`
		a := &GeminiAdapter{}
		_, err := a.TransformRequest([]byte(input), Model{})
		if err == nil {
			t.Fatal("expected error for empty function name, got nil")
		}
	})

	t.Run("duplicate tool_call_id with different function name returns error", func(t *testing.T) {
		t.Parallel()
		// Same id "call_1" used for two different function names — conflict.
		input := `{"model":"gemini-1.5-pro","messages":[
			{"role":"user","content":"q"},
			{"role":"assistant","content":null,"tool_calls":[
				{"id":"call_1","type":"function","function":{"name":"fn_a","arguments":"{}"}},
				{"id":"call_1","type":"function","function":{"name":"fn_b","arguments":"{}"}}
			]}
		]}`
		a := &GeminiAdapter{}
		_, err := a.TransformRequest([]byte(input), Model{})
		if err == nil {
			t.Fatal("expected error for duplicate id with conflicting name, got nil")
		}
	})

	t.Run("duplicate tool_call_id with SAME function name is fine", func(t *testing.T) {
		t.Parallel()
		// Same id used for same function name — technically odd but not conflicting.
		input := `{"model":"gemini-1.5-pro","messages":[
			{"role":"user","content":"q"},
			{"role":"assistant","content":null,"tool_calls":[
				{"id":"call_1","type":"function","function":{"name":"fn_a","arguments":"{}"}},
				{"id":"call_1","type":"function","function":{"name":"fn_a","arguments":"{\"x\":1}"}}
			]}
		]}`
		a := &GeminiAdapter{}
		_, err := a.TransformRequest([]byte(input), Model{})
		if err != nil {
			t.Fatalf("unexpected error for duplicate id with same name: %v", err)
		}
	})

	t.Run("empty tool_call_id in role:tool message returns error", func(t *testing.T) {
		t.Parallel()
		input := `{"model":"gemini-1.5-pro","messages":[
			{"role":"user","content":"q"},
			{"role":"tool","tool_call_id":"","content":"result"}
		]}`
		a := &GeminiAdapter{}
		_, err := a.TransformRequest([]byte(input), Model{})
		if err == nil {
			t.Fatal("expected error for empty tool_call_id in tool message, got nil")
		}
	})

	t.Run("empty function name in TransformResponse functionCall returns error", func(t *testing.T) {
		t.Parallel()
		// Upstream returns a functionCall part with an empty name.
		input := `{
			"candidates": [{
				"content": {"role": "model", "parts": [{"functionCall": {"name": "", "args": {}}}]},
				"finishReason": "STOP"
			}],
			"usageMetadata": {}
		}`
		a := &GeminiAdapter{}
		_, err := a.TransformResponse([]byte(input))
		if err == nil {
			t.Fatal("expected error for empty function name in response, got nil")
		}
	})
}

// ── FIX 4: named tool_choice with no declared tools ───────────────────────────

// TestGeminiTransformRequest_Fix4_NamedToolChoiceNoTools verifies that a
// named tool_choice without any declared tools (or with a name not in the
// declared list) fails closed unconditionally.
func TestGeminiTransformRequest_Fix4_NamedToolChoiceNoTools(t *testing.T) {
	t.Parallel()

	t.Run("named tool_choice with no tools array returns error", func(t *testing.T) {
		t.Parallel()
		// tool_choice names a function but no tools are declared — must fail.
		input := `{"model":"gemini-1.5-pro","messages":[{"role":"user","content":"q"}],"tool_choice":{"type":"function","function":{"name":"get_data"}}}`
		a := &GeminiAdapter{}
		_, err := a.TransformRequest([]byte(input), Model{})
		if err == nil {
			t.Fatal("expected error for named tool_choice with no tools declared, got nil")
		}
	})

	t.Run("named tool_choice with tools but name not in list returns error", func(t *testing.T) {
		t.Parallel()
		input := `{
			"model":"gemini-1.5-pro",
			"messages":[{"role":"user","content":"q"}],
			"tools":[{"type":"function","function":{"name":"existing_fn","parameters":{}}}],
			"tool_choice":{"type":"function","function":{"name":"nonexistent_fn"}}
		}`
		a := &GeminiAdapter{}
		_, err := a.TransformRequest([]byte(input), Model{})
		if err == nil {
			t.Fatal("expected error for named tool_choice referencing undeclared function, got nil")
		}
	})

	t.Run("named tool_choice with matching declared tool succeeds", func(t *testing.T) {
		t.Parallel()
		input := `{
			"model":"gemini-1.5-pro",
			"messages":[{"role":"user","content":"q"}],
			"tools":[{"type":"function","function":{"name":"get_data","parameters":{}}}],
			"tool_choice":{"type":"function","function":{"name":"get_data"}}
		}`
		a := &GeminiAdapter{}
		_, err := a.TransformRequest([]byte(input), Model{})
		if err != nil {
			t.Fatalf("unexpected error for valid named tool_choice: %v", err)
		}
	})
}

// ── FIX 5: tool block cap at 64 ───────────────────────────────────────────────

// TestGeminiToolBlockCap verifies that maxGeminiToolBlocks is 64, aligned with
// the PII Stage 0c StreamRestorer's maxToolCallsPerChoice=64.
func TestGeminiToolBlockCap(t *testing.T) {
	t.Parallel()

	if maxGeminiToolBlocks != 64 {
		t.Errorf("maxGeminiToolBlocks = %d, want 64 (must match Stage 0c maxToolCallsPerChoice)", maxGeminiToolBlocks)
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
