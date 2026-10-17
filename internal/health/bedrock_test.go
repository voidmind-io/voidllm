package health_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/voidmind-io/voidllm/internal/config"
	"github.com/voidmind-io/voidllm/internal/health"
)

const bedrockProbeModelID = "eu.anthropic.claude-sonnet-4-5-20250929-v1:0"

func TestBuildProbeRequest_BedrockChat(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		target     health.ProbeTarget
		wantURL    string
		wantAuth   string
		wantNoAuth bool
	}{
		{
			name: "regional endpoint",
			target: health.ProbeTarget{
				ModelName: "claude", Provider: "bedrock", BaseURL: "https://bedrock-runtime.eu-central-1.amazonaws.com",
				APIKey: "bk", Region: "eu-central-1", BedrockModelID: bedrockProbeModelID,
			},
			wantURL:  "https://bedrock-runtime.eu-central-1.amazonaws.com/model/eu.anthropic.claude-sonnet-4-5-20250929-v1%3A0/converse",
			wantAuth: "Bearer bk",
		},
		{
			name: "trailing slash override",
			target: health.ProbeTarget{
				ModelName: "claude", Provider: "bedrock", BaseURL: "https://gw.example.com/", APIKey: "bk", BedrockModelID: "m-v1:0",
			},
			wantURL:  "https://gw.example.com/model/m-v1%3A0/converse",
			wantAuth: "Bearer bk",
		},
		{
			name: "arn model id escaped once",
			target: health.ProbeTarget{
				ModelName: "claude", Provider: "bedrock", BaseURL: "https://h.example", APIKey: "bk",
				BedrockModelID: "arn:aws:bedrock:eu-central-1:123456789012:inference-profile/eu.x-v1:0",
			},
			wantURL:  "https://h.example/model/arn%3Aaws%3Abedrock%3Aeu-central-1%3A123456789012%3Ainference-profile%2Feu.x-v1%3A0/converse",
			wantAuth: "Bearer bk",
		},
		{
			name: "no key no authorization header",
			target: health.ProbeTarget{
				ModelName: "claude", Provider: "bedrock", BaseURL: "https://h.example", BedrockModelID: "m",
			},
			wantURL:    "https://h.example/model/m/converse",
			wantNoAuth: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req, err := health.BuildProbeRequest(context.Background(), health.IntentChat, tc.target)
			if err != nil {
				t.Fatalf("BuildProbeRequest: %v", err)
			}
			if req.Method != http.MethodPost {
				t.Errorf("Method = %q, want POST", req.Method)
			}
			if got := req.URL.Scheme + "://" + req.URL.Host + req.URL.EscapedPath(); got != tc.wantURL {
				t.Errorf("URL = %q, want %q", got, tc.wantURL)
			}
			if strings.Contains(req.URL.EscapedPath(), "%25") {
				t.Errorf("path %q is double encoded", req.URL.EscapedPath())
			}
			if req.URL.RawQuery != "" {
				t.Errorf("unexpected query %q", req.URL.RawQuery)
			}
			if tc.wantNoAuth {
				if v := req.Header.Get("Authorization"); v != "" {
					t.Errorf("Authorization = %q, want none", v)
				}
			} else if got := req.Header.Get("Authorization"); got != tc.wantAuth {
				t.Errorf("Authorization = %q, want %q", got, tc.wantAuth)
			}
			if req.Header.Get("Content-Type") != "application/json" || req.Header.Get("Accept") != "application/json" {
				t.Errorf("headers = %v", req.Header)
			}
			if req.Header.Get("x-api-key") != "" {
				t.Errorf("x-api-key must not be set for bedrock")
			}

			body := readBody(t, req)
			msgs := body["messages"].([]any)
			if len(msgs) != 1 {
				t.Fatalf("messages = %v", msgs)
			}
			m := msgs[0].(map[string]any)
			blocks := m["content"].([]any)
			if m["role"] != "user" || len(blocks) != 1 || blocks[0].(map[string]any)["text"] == nil {
				t.Errorf("message = %v", m)
			}
			inf := body["inferenceConfig"].(map[string]any)
			if inf["maxTokens"] != float64(1) {
				t.Errorf("inferenceConfig = %v, want maxTokens 1", inf)
			}
			for k := range body {
				switch k {
				case "messages", "inferenceConfig":
				default:
					t.Errorf("unexpected probe body key %q", k)
				}
			}
		})
	}
}

func TestBuildProbeRequest_BedrockNotApplicable(t *testing.T) {
	t.Parallel()

	target := health.ProbeTarget{
		ModelName: "claude", Provider: "bedrock", BaseURL: "https://h.example", APIKey: "k",
		Region: "eu-central-1", BedrockModelID: "m",
	}
	for name, intent := range map[string]health.ProbeIntent{
		"models list": health.IntentModelsList,
		"embeddings":  health.IntentEmbeddings,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			req, err := health.BuildProbeRequest(context.Background(), intent, target)
			if !errors.Is(err, health.ErrProbeNotApplicable) {
				t.Fatalf("err = %v, want ErrProbeNotApplicable", err)
			}
			if req != nil {
				t.Error("request must be nil when not applicable")
			}
		})
	}
}

func TestChecker_BedrockProbes(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var seen []string
	var auths []string
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		seen = append(seen, r.Method+" "+r.RequestURI)
		auths = append(auths, r.Header.Get("Authorization"))
		bodies = append(bodies, string(b))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"output":{"message":{"role":"assistant","content":[{"text":"p"}]}},"stopReason":"max_tokens","usage":{"inputTokens":1,"outputTokens":1}}`)
	}))
	t.Cleanup(srv.Close)

	reg := newRegistryWithConfig(t, config.ModelConfig{
		Provider: "bedrock", BaseURL: srv.URL, APIKey: "bk", Region: "eu-central-1", BedrockModelID: bedrockProbeModelID,
	})
	c := health.NewChecker(reg, cfg(true, true, true), newLogger())
	stop := c.Start()
	t.Cleanup(stop)

	mh, ok := c.GetHealth("test-model")
	if !ok {
		t.Fatal("probe did not run")
	}
	if mh.HealthOK == nil || !*mh.HealthOK {
		t.Errorf("HealthOK = %v, want true", mh.HealthOK)
	}
	if mh.ModelsOK != nil {
		t.Errorf("ModelsOK = %v, want nil (models list is not applicable to bedrock)", *mh.ModelsOK)
	}
	if mh.FunctionalOK == nil || !*mh.FunctionalOK {
		t.Errorf("FunctionalOK = %v, want true", mh.FunctionalOK)
	}
	if mh.Status != "healthy" {
		t.Errorf("Status = %q, want healthy (error %q)", mh.Status, mh.LastError)
	}

	mu.Lock()
	defer mu.Unlock()
	wantPath := "POST /model/eu.anthropic.claude-sonnet-4-5-20250929-v1%3A0/converse"
	var converse int
	for i, s := range seen {
		if s == wantPath {
			converse++
			if auths[i] != "Bearer bk" {
				t.Errorf("functional probe Authorization = %q", auths[i])
			}
			if !strings.Contains(bodies[i], `"maxTokens":1`) {
				t.Errorf("functional probe body = %s", bodies[i])
			}
		}
		if strings.Contains(s, "/models") || strings.Contains(s, "embeddings") || strings.Contains(s, "chat/completions") {
			t.Errorf("non-bedrock probe request sent: %s", s)
		}
	}
	if converse != 1 {
		t.Errorf("converse probes = %d (all requests %v), want 1", converse, seen)
	}
}

func TestChecker_BedrockDeploymentsCarryFields(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	paths := map[string]bool{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths[r.RequestURI] = true
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	reg := newRegistryWithConfig(t, config.ModelConfig{
		Provider: "bedrock", Strategy: "priority",
		Deployments: []config.DeploymentConfig{
			{Name: "a", Provider: "bedrock", BaseURL: srv.URL, Region: "eu-central-1", BedrockModelID: "eu.model-a-v1:0", Priority: 1},
			{Name: "b", Provider: "bedrock", BaseURL: srv.URL, Region: "us-east-1", BedrockModelID: "us.model-b-v1:0", Priority: 2},
		},
	})
	c := health.NewChecker(reg, cfg(false, false, true), newLogger())
	stop := c.Start()
	t.Cleanup(stop)

	for _, key := range []string{"test-model/a", "test-model/b"} {
		mh, ok := c.GetHealth(key)
		if !ok || mh.FunctionalOK == nil || !*mh.FunctionalOK {
			t.Errorf("%s: health = %+v", key, mh)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if !paths["/model/eu.model-a-v1%3A0/converse"] || !paths["/model/us.model-b-v1%3A0/converse"] {
		t.Errorf("deployment probes = %v", paths)
	}
}

func TestChecker_BedrockProbeFailureIsDegraded(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.Header().Set("X-Amzn-Errortype", "AccessDeniedException")
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"message":"User: arn:aws:iam::123456789012:user/x is not authorized"}`)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	reg := newRegistryWithConfig(t, config.ModelConfig{
		Provider: "bedrock", BaseURL: srv.URL, APIKey: "bk", Region: "eu-central-1", BedrockModelID: "m-v1",
	})
	c := health.NewChecker(reg, cfg(true, true, true), newLogger())
	stop := c.Start()
	t.Cleanup(stop)

	mh, _ := c.GetHealth("test-model")
	if mh == nil || mh.FunctionalOK == nil || *mh.FunctionalOK {
		t.Fatalf("FunctionalOK = %+v, want false", mh)
	}
	if mh.Status != "degraded" {
		t.Errorf("Status = %q, want degraded", mh.Status)
	}
	if strings.Contains(mh.LastError, "123456789012") || strings.Contains(mh.FunctionalError, "123456789012") {
		t.Errorf("health error leaks upstream message: %q / %q", mh.LastError, mh.FunctionalError)
	}
}
