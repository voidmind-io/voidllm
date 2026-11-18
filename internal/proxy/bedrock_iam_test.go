package proxy

// bedrock_iam_test.go covers bedrock_auth "iam": requests are SigV4 signed via
// the configured BedrockSigner, never carry a Bearer token, fail over between
// deployments on signing failure and never log or return credential detail.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/voidmind-io/voidllm/internal/awsauth"
	"github.com/voidmind-io/voidllm/internal/config"
)

const (
	iamSecretKey   = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYIAMSECRETKEY"
	iamSessionTok  = "IQoJb3JpZ2luX2VjEIAMSESSIONTOKEN"
	iamAccessKeyID = "AKIAIAMTESTKEY000000"
	iamRoleGood    = "arn:aws:iam::123456789012:role/good"
	iamRoleBad     = "arn:aws:iam::123456789012:role/bad"
)

// fakeBedrockSigner records calls and fails for roles listed in failRoles.
type fakeBedrockSigner struct {
	mu        sync.Mutex
	failRoles map[string]error
	failAll   error
	calls     []fakeSignCall
}

type fakeSignCall struct {
	region, role, uri, body string
	hadBearer               bool
}

func (f *fakeBedrockSigner) Sign(_ context.Context, req *http.Request, body []byte, region, roleARN string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fakeSignCall{
		region: region, role: roleARN, uri: req.URL.EscapedPath(), body: string(body),
		hadBearer: strings.HasPrefix(req.Header.Get("Authorization"), "Bearer"),
	})
	if f.failAll != nil {
		return f.failAll
	}
	if err, ok := f.failRoles[roleARN]; ok {
		return err
	}
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=FAKE/"+region+"/bedrock/aws4_request, Signature=abc")
	return nil
}

func (f *fakeBedrockSigner) snapshot() []fakeSignCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeSignCall(nil), f.calls...)
}

func bhIAMRegistry(t *testing.T, upstreamURL, apiKey string) *Registry {
	t.Helper()
	reg, err := NewRegistry([]config.ModelConfig{{
		Name: "claude", Provider: "bedrock", BaseURL: upstreamURL, APIKey: apiKey,
		Region: "eu-central-1", BedrockModelID: bhModelID, BedrockAuth: "iam", BedrockRoleARN: iamRoleGood,
	}})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	return reg
}

func assertNoCredsInLogs(t *testing.T, logs string) {
	t.Helper()
	for _, s := range []string{iamSecretKey, iamSessionTok, "SECRET-FROM-SDK"} {
		if strings.Contains(logs, s) {
			t.Errorf("logs leak %q:\n%s", s, logs)
		}
	}
}

func TestBedrockHandler_IAM_SignedNoBearer(t *testing.T) {
	t.Parallel()

	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%v", stream), func(t *testing.T) {
			t.Parallel()
			up := newBHUpstream(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
				if strings.HasSuffix(r.URL.Path, "/converse-stream") {
					bhWriteFrames(w, bhFrameStart(), bhFrameText("hi"), bhFrameStop("end_turn"), bhFrameMeta(1, 1, 0, 0))
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, bhOKJSON)
			})
			env := newBHEnv(t, bhIAMRegistry(t, up.URL, ""))
			signer := &fakeBedrockSigner{}
			env.h.AWSSigner = signer

			s := ""
			path := "/converse"
			if stream {
				s = `"stream":true,`
				path = "/converse-stream"
			}
			resp := env.post(t, `{"model":"claude",`+s+`"messages":[{"role":"user","content":"hi"}]}`)
			if resp.Status != 200 {
				t.Fatalf("status %d: %s", resp.Status, resp.Body)
			}
			reqs := up.requests()
			if len(reqs) != 1 {
				t.Fatalf("upstream requests = %d", len(reqs))
			}
			auth := reqs[0].Header.Get("Authorization")
			if !strings.HasPrefix(auth, "AWS4-HMAC-SHA256 ") || strings.Contains(auth, "Bearer") {
				t.Errorf("Authorization = %q, want SigV4 and no Bearer", auth)
			}
			if reqs[0].RequestURI != "/model/"+bhModelEsc+path {
				t.Errorf("URI = %q", reqs[0].RequestURI)
			}
			calls := signer.snapshot()
			if len(calls) != 1 {
				t.Fatalf("sign calls = %d", len(calls))
			}
			c := calls[0]
			if c.region != "eu-central-1" || c.role != iamRoleGood || c.hadBearer {
				t.Errorf("sign call = %+v", c)
			}
			if c.body != string(reqs[0].Body) {
				t.Errorf("signed body differs from the body sent upstream:\nsigned: %s\nsent:   %s", c.body, reqs[0].Body)
			}
			if c.uri != "/model/"+bhModelEsc+path {
				t.Errorf("signed URI = %q", c.uri)
			}
			assertNoCredsInLogs(t, env.logs.String())
		})
	}
}

func TestBedrockHandler_APIKeyModeUnchanged(t *testing.T) {
	t.Parallel()
	up := newBHUpstream(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, bhOKJSON)
	})
	// Both explicit "api_key" and empty (legacy) auth use Bearer, and the signer is untouched.
	for _, auth := range []string{"api_key", ""} {
		t.Run("auth="+auth, func(t *testing.T) {
			t.Parallel()
			reg, err := NewRegistry([]config.ModelConfig{{
				Name: "claude", Provider: "bedrock", BaseURL: up.URL, APIKey: bhAPIKey,
				Region: "eu-central-1", BedrockModelID: bhModelID, BedrockAuth: auth,
			}})
			if err != nil {
				t.Fatal(err)
			}
			env := newBHEnv(t, reg)
			signer := &fakeBedrockSigner{}
			env.h.AWSSigner = signer
			resp := env.post(t, `{"model":"claude","messages":[{"role":"user","content":"hi"}]}`)
			if resp.Status != 200 {
				t.Fatalf("status %d: %s", resp.Status, resp.Body)
			}
			reqs := up.requests()
			if got := reqs[len(reqs)-1].Header.Get("Authorization"); got != "Bearer "+bhAPIKey {
				t.Errorf("Authorization = %q, want Bearer", got)
			}
			if len(signer.snapshot()) != 0 {
				t.Error("signer must not be used in api_key mode")
			}
		})
	}
}

func TestBedrockHandler_IAM_NoSignerConfigured(t *testing.T) {
	t.Parallel()
	up := newBHUpstream(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, bhOKJSON)
	})
	env := newBHEnv(t, bhIAMRegistry(t, up.URL, ""))
	resp := env.post(t, `{"model":"claude","messages":[{"role":"user","content":"hi"}]}`)
	if resp.Status != http.StatusBadGateway || !strings.Contains(resp.Body, "Bedrock IAM credentials unavailable") {
		t.Fatalf("status %d body %s", resp.Status, resp.Body)
	}
	if n := len(up.requests()); n != 0 {
		t.Errorf("unsigned request reached upstream (%d)", n)
	}
}

func TestBedrockHandler_IAM_WithAPIKeyIsConfigError(t *testing.T) {
	t.Parallel()
	up := newBHUpstream(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, bhOKJSON)
	})
	env := newBHEnv(t, bhIAMRegistry(t, up.URL, bhAPIKey))
	signer := &fakeBedrockSigner{}
	env.h.AWSSigner = signer
	resp := env.post(t, `{"model":"claude","messages":[{"role":"user","content":"hi"}]}`)
	if resp.Status != http.StatusInternalServerError || !strings.Contains(resp.Body, "invalid bedrock configuration") {
		t.Fatalf("status %d body %s", resp.Status, resp.Body)
	}
	if len(up.requests()) != 0 || len(signer.snapshot()) != 0 {
		t.Error("invalid config must not sign or reach upstream")
	}
	if strings.Contains(resp.Body, bhAPIKey) || strings.Contains(env.logs.String(), bhAPIKey) {
		t.Error("api key leaked")
	}
}

func TestBedrockHandler_IAM_SignFailureFailsOverToSecondDeployment(t *testing.T) {
	t.Parallel()

	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%v", stream), func(t *testing.T) {
			t.Parallel()
			up1 := newBHUpstream(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
				t.Error("first deployment must not receive a request when signing fails")
				w.WriteHeader(500)
			})
			up2 := newBHUpstream(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
				if strings.HasSuffix(r.URL.Path, "/converse-stream") {
					bhWriteFrames(w, bhFrameStart(), bhFrameText("from-two"), bhFrameStop("end_turn"), bhFrameMeta(1, 1, 0, 0))
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, strings.Replace(bhOKJSON, "pong", "from-two", 1))
			})
			dep1 := Deployment{Name: "d1", Provider: "bedrock", BaseURL: up1.URL, Region: "eu-central-1", BedrockModelID: "eu.model-one-v1:0", BedrockAuth: "iam", BedrockRoleARN: iamRoleBad, Priority: 1}
			dep2 := Deployment{Name: "d2", Provider: "bedrock", BaseURL: up2.URL, Region: "us-east-1", BedrockModelID: "us.model-two-v1:0", BedrockAuth: "iam", BedrockRoleARN: iamRoleGood, Priority: 2}
			env := newBHEnv(t, twoDeploymentRegistry("claude", dep1, dep2))
			env.h.Router = &staticDeploymentPicker{deps: []Deployment{dep1, dep2}}
			signer := &fakeBedrockSigner{failRoles: map[string]error{
				iamRoleBad: &awsauth.CredentialsError{Category: awsauth.CategoryAssumeRoleFailed},
			}}
			env.h.AWSSigner = signer

			s := ""
			if stream {
				s = `"stream":true,`
			}
			resp := env.post(t, `{"model":"claude",`+s+`"messages":[{"role":"user","content":"hi"}]}`)
			if resp.Status != 200 || !strings.Contains(resp.Body, "from-two") {
				t.Fatalf("status %d body %s", resp.Status, resp.Body)
			}
			if n := len(up1.requests()); n != 0 {
				t.Errorf("dep1 requests = %d", n)
			}
			r2 := up2.requests()
			if len(r2) != 1 {
				t.Fatalf("dep2 requests = %d", len(r2))
			}
			if !strings.HasPrefix(r2[0].Header.Get("Authorization"), "AWS4-HMAC-SHA256 ") {
				t.Errorf("dep2 Authorization = %q", r2[0].Header.Get("Authorization"))
			}
			if !strings.HasPrefix(r2[0].RequestURI, "/model/us.model-two-v1%3A0/converse") {
				t.Errorf("dep2 URI = %q", r2[0].RequestURI)
			}
			calls := signer.snapshot()
			if len(calls) != 2 {
				t.Fatalf("sign calls = %+v", calls)
			}
			if calls[0].role != iamRoleBad || calls[0].region != "eu-central-1" || calls[1].role != iamRoleGood || calls[1].region != "us-east-1" {
				t.Errorf("sign calls (region/role not re-resolved per deployment) = %+v", calls)
			}
			if !strings.Contains(calls[1].uri, "us.model-two-v1%3A0") {
				t.Errorf("second signature covers wrong path %q", calls[1].uri)
			}
			if calls[1].hadBearer {
				t.Error("bearer leaked into second signing attempt")
			}
			logs := env.logs.String()
			if !strings.Contains(logs, awsauth.CategoryAssumeRoleFailed) {
				t.Errorf("category not logged:\n%s", logs)
			}
			assertNoCredsInLogs(t, logs)
		})
	}
}

func TestBedrockHandler_IAM_AllSignersFail(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
	}{
		{"credentials error", &awsauth.CredentialsError{Category: awsauth.CategoryCredentialsUnavailable}},
		{"arbitrary error with secrets", errors.New("SECRET-FROM-SDK " + iamSecretKey + " " + iamSessionTok + " " + iamAccessKeyID)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			up := newBHUpstream(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
				t.Error("no request may reach upstream")
				w.WriteHeader(500)
			})
			dep1 := Deployment{Name: "d1", Provider: "bedrock", BaseURL: up.URL, Region: "eu-central-1", BedrockModelID: "m1-v1:0", BedrockAuth: "iam", Priority: 1}
			dep2 := Deployment{Name: "d2", Provider: "bedrock", BaseURL: up.URL, Region: "us-east-1", BedrockModelID: "m2-v1:0", BedrockAuth: "iam", Priority: 2}
			env := newBHEnv(t, twoDeploymentRegistry("claude", dep1, dep2))
			env.h.Router = &staticDeploymentPicker{deps: []Deployment{dep1, dep2}}
			signer := &fakeBedrockSigner{failAll: tc.err}
			env.h.AWSSigner = signer

			resp := env.post(t, `{"model":"claude","messages":[{"role":"user","content":"hi"}]}`)
			if resp.Status != http.StatusBadGateway {
				t.Fatalf("status %d body %s", resp.Status, resp.Body)
			}
			if !strings.Contains(resp.Body, "Bedrock IAM credentials unavailable") {
				t.Errorf("body = %s", resp.Body)
			}
			for _, s := range []string{iamSecretKey, iamSessionTok, iamAccessKeyID, "SECRET-FROM-SDK"} {
				if strings.Contains(resp.Body, s) {
					t.Errorf("response leaks %q", s)
				}
			}
			if n := len(signer.snapshot()); n != 2 {
				t.Errorf("sign calls = %d, want 2 (both deployments tried)", n)
			}
			assertNoCredsInLogs(t, env.logs.String())
		})
	}
}

// TestBedrockHandler_IAM_RealSignerEndToEnd drives the real awsauth signer
// with static credentials supplied through the environment (no network, no
// shared config files) and checks the upstream sees a well-formed SigV4
// request without Bearer. It cannot be parallel because of t.Setenv.
func TestBedrockHandler_IAM_RealSignerEndToEnd(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", iamAccessKeyID)
	t.Setenv("AWS_SECRET_ACCESS_KEY", iamSecretKey)
	t.Setenv("AWS_SESSION_TOKEN", iamSessionTok)
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	t.Setenv("AWS_CONFIG_FILE", "/nonexistent/aws-config")
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", "/nonexistent/aws-credentials")
	t.Setenv("AWS_PROFILE", "")

	up := newBHUpstream(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, bhOKJSON)
	})
	reg, err := NewRegistry([]config.ModelConfig{{
		Name: "claude", Provider: "bedrock", BaseURL: up.URL, Region: "eu-central-1", BedrockModelID: bhModelID, BedrockAuth: "iam",
	}})
	if err != nil {
		t.Fatal(err)
	}
	env := newBHEnv(t, reg)
	env.h.AWSSigner = awsauth.NewSigner()

	resp := env.post(t, `{"model":"claude","messages":[{"role":"user","content":"hi"}]}`)
	if resp.Status != 200 {
		t.Fatalf("status %d: %s", resp.Status, resp.Body)
	}
	reqs := up.requests()
	if len(reqs) != 1 {
		t.Fatalf("upstream requests = %d", len(reqs))
	}
	h := reqs[0].Header
	auth := h.Get("Authorization")
	if !strings.HasPrefix(auth, "AWS4-HMAC-SHA256 Credential="+iamAccessKeyID+"/") ||
		!strings.Contains(auth, "/eu-central-1/bedrock/aws4_request") {
		t.Errorf("Authorization = %q", auth)
	}
	if strings.Contains(auth, "Bearer") {
		t.Errorf("Authorization has Bearer: %q", auth)
	}
	for _, want := range []string{"host", "x-amz-content-sha256", "x-amz-date", "x-amz-security-token"} {
		if !strings.Contains(auth, want) {
			t.Errorf("SignedHeaders missing %s: %q", want, auth)
		}
	}
	if h.Get("X-Amz-Security-Token") != iamSessionTok || h.Get("X-Amz-Content-Sha256") == "" || h.Get("X-Amz-Date") == "" {
		t.Errorf("amz headers = %v", h)
	}
	if strings.Contains(auth, iamSecretKey) {
		t.Error("secret appears in Authorization")
	}
	assertNoCredsInLogs(t, env.logs.String())
}
