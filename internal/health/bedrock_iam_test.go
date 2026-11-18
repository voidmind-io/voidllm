package health_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/voidmind-io/voidllm/internal/config"
	"github.com/voidmind-io/voidllm/internal/health"
)

// fakeSigner records Sign calls and stamps a recognizable Authorization.
type fakeSigner struct {
	mu    sync.Mutex
	calls []signCall
	err   error
}

type signCall struct {
	body    string
	region  string
	roleARN string
}

func (f *fakeSigner) Sign(_ context.Context, req *http.Request, body []byte, region, roleARN string) error {
	f.mu.Lock()
	f.calls = append(f.calls, signCall{string(body), region, roleARN})
	f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=FAKE")
	return nil
}

// awsURLFor returns a valid eu-central-1 Bedrock VPC endpoint URL that
// awsRouteTransport routes to the local test server at serverURL, so IAM tests
// pass the Bedrock host check. The server port is encoded in the endpoint ID.
func awsURLFor(serverURL string) string {
	u, err := url.Parse(serverURL)
	if err != nil {
		return serverURL
	}
	return "https://vpce-" + u.Port() + ".bedrock-runtime.eu-central-1.vpce.amazonaws.com"
}

// fakeEndpointRe matches exactly the hosts produced by awsURLFor.
var fakeEndpointRe = regexp.MustCompile(`^vpce-([0-9]+)\.bedrock-runtime\.eu-central-1\.vpce\.amazonaws\.com$`)

// awsRouteTransport rewrites awsURLFor hosts to the local server.
type awsRouteTransport struct{}

func (awsRouteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if m := fakeEndpointRe.FindStringSubmatch(req.URL.Hostname()); m != nil {
		r := req.Clone(req.Context())
		r.URL.Scheme = "http"
		r.URL.Host = "127.0.0.1:" + m[1]
		r.Host = r.URL.Host
		return http.DefaultTransport.RoundTrip(r)
	}
	return http.DefaultTransport.RoundTrip(req)
}

func iamTarget() health.ProbeTarget {
	return health.ProbeTarget{
		ModelName: "claude", Provider: "bedrock", BaseURL: "https://bedrock-runtime.eu-central-1.amazonaws.com", APIKey: "stale-key",
		Region: "eu-central-1", BedrockModelID: "m-v1:0", BedrockAuth: "iam",
		BedrockRoleARN: "arn:aws:iam::123456789012:role/r",
	}
}

func TestBuildProbeRequest_BedrockIAMNoBearer(t *testing.T) {
	t.Parallel()
	req, err := health.BuildProbeRequest(context.Background(), health.IntentChat, iamTarget())
	if err != nil {
		t.Fatal(err)
	}
	if got := req.Header.Get("Authorization"); got != "" {
		t.Errorf("Authorization = %q, want empty for iam", got)
	}
}

func TestSignProbeRequest(t *testing.T) {
	t.Parallel()
	apiKeyTarget := iamTarget()
	apiKeyTarget.BedrockAuth = "api_key"
	emptyAuthTarget := iamTarget()
	emptyAuthTarget.BedrockAuth = ""
	openai := health.ProbeTarget{Provider: "openai", BaseURL: "https://x", BedrockAuth: "iam"}

	tests := []struct {
		name       string
		target     health.ProbeTarget
		signer     *fakeSigner
		nilSigner  bool
		wantErr    error
		wantSigned bool
	}{
		{name: "iam signs", target: iamTarget(), signer: &fakeSigner{}, wantSigned: true},
		{name: "iam signer failure", target: iamTarget(), signer: &fakeSigner{err: errors.New("leaky sdk detail SECRET")}, wantErr: health.ErrBedrockCredentials},
		{name: "iam non-AWS endpoint refused", target: func() health.ProbeTarget {
			x := iamTarget()
			x.BaseURL = "https://evil.example.com"
			return x
		}(), signer: &fakeSigner{}, wantErr: health.ErrBedrockCredentials},
		{name: "iam http endpoint refused", target: func() health.ProbeTarget {
			x := iamTarget()
			x.BaseURL = "http://bedrock-runtime.eu-central-1.amazonaws.com"
			return x
		}(), signer: &fakeSigner{}, wantErr: health.ErrBedrockCredentials},
		{name: "iam nil signer", target: iamTarget(), nilSigner: true, wantErr: health.ErrBedrockCredentials},
		{name: "api_key is a no-op", target: apiKeyTarget, signer: &fakeSigner{}},
		{name: "empty auth is a no-op", target: emptyAuthTarget, signer: &fakeSigner{}},
		{name: "non-bedrock is a no-op", target: openai, signer: &fakeSigner{}},
		{name: "api_key with nil signer is a no-op", target: apiKeyTarget, nilSigner: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req, err := health.BuildProbeRequest(context.Background(), health.IntentChat, tc.target)
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			before := req.Header.Get("Authorization")
			var signer interface {
				Sign(context.Context, *http.Request, []byte, string, string) error
			}
			if !tc.nilSigner {
				signer = tc.signer
			}
			err = health.SignProbeRequest(context.Background(), signer, req, tc.target)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if err != nil && strings.Contains(err.Error(), "SECRET") {
				t.Errorf("error leaks signer detail: %v", err)
			}
			signed := req.Header.Get("Authorization") == "AWS4-HMAC-SHA256 Credential=FAKE"
			if signed != tc.wantSigned {
				t.Errorf("signed = %v, want %v", signed, tc.wantSigned)
			}
			if !tc.wantSigned && req.Header.Get("Authorization") != before && err == nil {
				t.Errorf("no-op modified Authorization")
			}
			if tc.wantSigned {
				c := tc.signer.calls
				if len(c) != 1 || c[0].region != "eu-central-1" || c[0].roleARN != tc.target.BedrockRoleARN ||
					!strings.Contains(c[0].body, `"maxTokens":1`) {
					t.Errorf("sign calls = %+v", c)
				}
			}
		})
	}
}

// TestSignProbeRequest_ReplacesPreexistingAuthorization guards against a
// bearer header surviving into a signed IAM request.
func TestSignProbeRequest_ReplacesPreexistingAuthorization(t *testing.T) {
	t.Parallel()
	req, _ := http.NewRequest(http.MethodPost, "https://h.example/model/m/converse", nil)
	req.Header.Set("Authorization", "Bearer leftover")
	err := health.SignProbeRequest(context.Background(), &fakeSigner{err: errors.New("x")}, req, iamTarget())
	if !errors.Is(err, health.ErrBedrockCredentials) {
		t.Fatalf("err = %v", err)
	}
	if got := req.Header.Get("Authorization"); got != "" {
		t.Errorf("Authorization = %q, want removed", got)
	}
}

func TestChecker_BedrockIAM(t *testing.T) {
	t.Parallel()

	type seenReq struct{ method, auth string }
	run := func(t *testing.T, signer *fakeSigner) (*health.ModelHealth, []seenReq) {
		t.Helper()
		var mu sync.Mutex
		var seen []seenReq
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			mu.Lock()
			seen = append(seen, seenReq{r.Method, r.Header.Get("Authorization")})
			mu.Unlock()
			_, _ = io.WriteString(w, `{"output":{"message":{"role":"assistant","content":[{"text":"p"}]}},"stopReason":"max_tokens","usage":{"inputTokens":1,"outputTokens":1}}`)
		}))
		t.Cleanup(srv.Close)

		reg := newRegistryWithConfig(t, config.ModelConfig{
			Provider: "bedrock", BaseURL: awsURLFor(srv.URL), Region: "eu-central-1", BedrockModelID: "m-v1",
			BedrockAuth: "iam", BedrockRoleARN: "arn:aws:iam::123456789012:role/r",
		})
		c := health.NewChecker(reg, cfg(true, true, true), newLogger())
		c.SetHTTPClientForTest(&http.Client{Transport: awsRouteTransport{}})
		if signer != nil {
			c.SetSigner(signer)
		}
		stop := c.Start()
		t.Cleanup(stop)
		mh, ok := c.GetHealth("test-model")
		if !ok {
			t.Fatal("probe did not run")
		}
		mu.Lock()
		defer mu.Unlock()
		return mh, append([]seenReq(nil), seen...)
	}

	t.Run("signed functional probe, unauthenticated reachability GET", func(t *testing.T) {
		t.Parallel()
		signer := &fakeSigner{}
		mh, seen := run(t, signer)
		if mh.Status != "healthy" {
			t.Fatalf("status = %q (%q)", mh.Status, mh.LastError)
		}
		var gets, posts int
		for _, s := range seen {
			switch s.method {
			case http.MethodGet:
				gets++
				if s.auth != "" {
					t.Errorf("reachability GET sent Authorization %q for iam", s.auth)
				}
			case http.MethodPost:
				posts++
				if !strings.HasPrefix(s.auth, "AWS4-HMAC-SHA256") {
					t.Errorf("POST Authorization = %q, want SigV4", s.auth)
				}
			}
		}
		if gets == 0 || posts != 1 {
			t.Errorf("gets=%d posts=%d", gets, posts)
		}
		if len(signer.calls) != 1 || signer.calls[0].roleARN != "arn:aws:iam::123456789012:role/r" || signer.calls[0].region != "eu-central-1" {
			t.Errorf("sign calls = %+v", signer.calls)
		}
	})

	t.Run("no signer reports sanitized credentials failure", func(t *testing.T) {
		t.Parallel()
		mh, seen := run(t, nil)
		if mh.FunctionalOK == nil || *mh.FunctionalOK {
			t.Fatalf("FunctionalOK = %v, want false", mh.FunctionalOK)
		}
		if mh.Status != "degraded" {
			t.Errorf("status = %q", mh.Status)
		}
		if mh.FunctionalError != health.ErrBedrockCredentials.Error() {
			t.Errorf("FunctionalError = %q", mh.FunctionalError)
		}
		for _, s := range seen {
			if s.method == http.MethodPost {
				t.Error("POST sent despite missing signer")
			}
		}
	})

	t.Run("signer error is sanitized", func(t *testing.T) {
		t.Parallel()
		mh, _ := run(t, &fakeSigner{err: errors.New("sts body SECRET-TOKEN")})
		if strings.Contains(mh.LastError, "SECRET") || strings.Contains(mh.FunctionalError, "SECRET") {
			t.Errorf("leak: %q / %q", mh.LastError, mh.FunctionalError)
		}
		if mh.FunctionalError != health.ErrBedrockCredentials.Error() {
			t.Errorf("FunctionalError = %q", mh.FunctionalError)
		}
	})
}
