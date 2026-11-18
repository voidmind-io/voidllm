package awsauth

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
)

const (
	testAKID    = "AKIDEXAMPLE0000TEST"
	testSecret  = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYSECRETKEYZZZ"
	testToken   = "FwoGZXIvYXdzEJr//////////SESSIONTOKENVALUE"
	testRegion  = "eu-central-1"
	escapedPath = "/model/eu.anthropic.claude-haiku-4-5-20251001-v1%3A0/converse"
)

func staticProvider(token string) aws.CredentialsProvider {
	return aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
		return aws.Credentials{AccessKeyID: testAKID, SecretAccessKey: testSecret, SessionToken: token, Source: "test"}, nil
	})
}

func newReq(t *testing.T, path string, body []byte) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, "https://bedrock-runtime.eu-central-1.amazonaws.com"+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	return req
}

func sha(body []byte) string {
	s := sha256.Sum256(body)
	return hex.EncodeToString(s[:])
}

func parseAuth(t *testing.T, h string) (cred, signedHeaders, sig string) {
	t.Helper()
	if !strings.HasPrefix(h, "AWS4-HMAC-SHA256 ") {
		t.Fatalf("Authorization = %q, want AWS4-HMAC-SHA256 prefix", h)
	}
	for _, part := range strings.Split(strings.TrimPrefix(h, "AWS4-HMAC-SHA256 "), ", ") {
		k, v, _ := strings.Cut(part, "=")
		switch k {
		case "Credential":
			cred = v
		case "SignedHeaders":
			signedHeaders = v
		case "Signature":
			sig = v
		}
	}
	return
}

func TestSign_HeaderShape(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		token string
	}{
		{"static credentials", ""},
		{"with session token", testToken},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newSigner(withBaseProvider(staticProvider(tc.token)))
			body := []byte(`{"messages":[]}`)
			req := newReq(t, escapedPath, body)
			if err := s.Sign(context.Background(), req, body, testRegion, ""); err != nil {
				t.Fatalf("Sign: %v", err)
			}
			cred, signed, sig := parseAuth(t, req.Header.Get("Authorization"))
			date := time.Now().UTC().Format("20060102")
			wantCred := testAKID + "/" + date + "/" + testRegion + "/bedrock/aws4_request"
			// Tolerate a UTC date rollover between Sign and the check.
			yesterday := time.Now().UTC().Add(-24 * time.Hour).Format("20060102")
			if cred != wantCred && cred != testAKID+"/"+yesterday+"/"+testRegion+"/bedrock/aws4_request" {
				t.Errorf("Credential = %q, want %q", cred, wantCred)
			}
			if sig == "" {
				t.Error("empty signature")
			}
			for _, h := range []string{"host", "x-amz-content-sha256", "x-amz-date"} {
				if !strings.Contains(";"+signed+";", ";"+h+";") {
					t.Errorf("SignedHeaders %q missing %s", signed, h)
				}
			}
			hasToken := strings.Contains(";"+signed+";", ";x-amz-security-token;")
			if tc.token != "" {
				if !hasToken {
					t.Errorf("SignedHeaders %q missing x-amz-security-token", signed)
				}
				if got := req.Header.Get("X-Amz-Security-Token"); got != tc.token {
					t.Errorf("X-Amz-Security-Token = %q", got)
				}
			} else if hasToken || req.Header.Get("X-Amz-Security-Token") != "" {
				t.Errorf("security token present without session token: %q", signed)
			}
			if got := req.Header.Get("X-Amz-Content-Sha256"); got != sha(body) {
				t.Errorf("X-Amz-Content-Sha256 = %q, want %q", got, sha(body))
			}
			if req.Header.Get("X-Amz-Date") == "" {
				t.Error("X-Amz-Date not set")
			}
		})
	}
}

func TestSign_EmptyBodyHash(t *testing.T) {
	t.Parallel()
	s := newSigner(withBaseProvider(staticProvider("")))
	req := newReq(t, "/model/x/converse", nil)
	if err := s.Sign(context.Background(), req, nil, testRegion, ""); err != nil {
		t.Fatal(err)
	}
	if got := req.Header.Get("X-Amz-Content-Sha256"); got != sha(nil) {
		t.Errorf("hash = %q, want %q", got, sha(nil))
	}
}

// TestSign_SignatureMatchesSDK signs through Signer, then re-derives the
// signature with an independent v4.Signer using the X-Amz-Date the Signer
// chose, so the comparison is exact without a clock hook.
func TestSign_SignatureMatchesSDK(t *testing.T) {
	t.Parallel()
	body := []byte(`{"messages":[{"role":"user","content":[{"text":"ping"}]}]}`)
	creds := aws.Credentials{AccessKeyID: testAKID, SecretAccessKey: testSecret, SessionToken: testToken}

	tests := []struct {
		name string
		path string
	}{
		{name: "escaped model id path uses default (double) encoding", path: escapedPath},
		{name: "plain path", path: "/model/plain-model/converse"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newSigner(withBaseProvider(staticProvider(testToken)))
			req := newReq(t, tc.path, body)
			if err := s.Sign(context.Background(), req, body, testRegion, ""); err != nil {
				t.Fatal(err)
			}
			_, _, got := parseAuth(t, req.Header.Get("Authorization"))
			ts, err := time.Parse("20060102T150405Z", req.Header.Get("X-Amz-Date"))
			if err != nil {
				t.Fatalf("parse X-Amz-Date: %v", err)
			}

			sign := func(disable bool) string {
				clone := newReq(t, tc.path, body)
				clone.Header.Set("X-Amz-Content-Sha256", sha(body))
				signer := v4.NewSigner(func(o *v4.SignerOptions) { o.DisableURIPathEscaping = disable })
				if err := signer.SignHTTP(context.Background(), creds, clone, sha(body), "bedrock", testRegion, ts); err != nil {
					t.Fatal(err)
				}
				_, _, sig := parseAuth(t, clone.Header.Get("Authorization"))
				return sig
			}
			if want := sign(false); got != want {
				t.Errorf("signature = %s, want SDK default-escaping %s", got, want)
			}
			if strings.Contains(tc.path, "%3A") {
				if other := sign(true); got == other {
					t.Errorf("signature equals DisableURIPathEscaping=true result; escaped path not double-encoded")
				}
			}
		})
	}
}

func TestSign_RoleCachePerRoleAndRegion(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	seen := sync.Map{}
	factory := func(_ aws.Config, roleARN, region string) aws.CredentialsProvider {
		calls.Add(1)
		seen.Store(roleARN+"|"+region, true)
		return aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{AccessKeyID: "ROLE" + region, SecretAccessKey: testSecret, SessionToken: roleARN}, nil
		})
	}
	s := newSigner(withBaseProvider(staticProvider("")), withRoleProviderFactory(factory))

	const roleA = "arn:aws:iam::123456789012:role/a"
	const roleB = "arn:aws:iam::123456789012:role/b"
	keys := []struct{ role, region string }{
		{roleA, "eu-central-1"}, {roleA, "us-east-1"}, {roleB, "eu-central-1"},
	}
	var wg sync.WaitGroup
	for i := 0; i < 30; i++ {
		k := keys[i%len(keys)]
		wg.Add(1)
		go func() {
			defer wg.Done()
			body := []byte("{}")
			req := newReq(t, "/model/x/converse", body)
			if err := s.Sign(context.Background(), req, body, k.region, k.role); err != nil {
				t.Errorf("Sign: %v", err)
				return
			}
			if !strings.HasPrefix(req.Header.Get("Authorization"), "AWS4-HMAC-SHA256 Credential=ROLE"+k.region+"/") {
				t.Errorf("wrong credentials used: %q", req.Header.Get("Authorization"))
			}
			if req.Header.Get("X-Amz-Security-Token") != k.role {
				t.Errorf("session token = %q, want role %q", req.Header.Get("X-Amz-Security-Token"), k.role)
			}
		}()
	}
	wg.Wait()
	if got := calls.Load(); got != int32(len(keys)) {
		t.Errorf("factory called %d times, want %d", got, len(keys))
	}
}

func TestSign_RoleFailureCategory(t *testing.T) {
	t.Parallel()
	secretErr := errors.New("sts said: SECRET-IN-SDK-ERROR " + testSecret)
	factory := func(aws.Config, string, string) aws.CredentialsProvider {
		return aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) { return aws.Credentials{}, secretErr })
	}
	s := newSigner(withBaseProvider(staticProvider(testToken)), withRoleProviderFactory(factory))
	req := newReq(t, "/model/x/converse", nil)
	err := s.Sign(context.Background(), req, nil, testRegion, "arn:aws:iam::123456789012:role/a")
	var ce *CredentialsError
	if !errors.As(err, &ce) {
		t.Fatalf("err = %v, want *CredentialsError", err)
	}
	if ce.Category != CategoryAssumeRoleFailed || err.Error() != CategoryAssumeRoleFailed {
		t.Errorf("category = %q, Error() = %q", ce.Category, err.Error())
	}
	if !errors.Is(err, secretErr) {
		t.Error("Unwrap should expose the cause")
	}
	assertNoSecret(t, err.Error())
	if req.Header.Get("Authorization") != "" {
		t.Error("Authorization set despite failure")
	}
}

func TestSign_BaseRetrievalFailureCategory(t *testing.T) {
	t.Parallel()
	secretErr := errors.New("imds: " + testSecret + testToken)
	prov := aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) { return aws.Credentials{}, secretErr })
	s := newSigner(withBaseProvider(prov))
	err := s.Sign(context.Background(), newReq(t, "/model/x/converse", nil), nil, testRegion, "")
	var ce *CredentialsError
	if !errors.As(err, &ce) || ce.Category != CategoryCredentialsUnavailable {
		t.Fatalf("err = %v", err)
	}
	assertNoSecret(t, err.Error())
}

func TestSign_BaseLoadFailureRetried(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	s := newSigner(withBaseProvider(staticProvider("")))
	s.load = func(context.Context) (aws.Config, error) {
		if calls.Add(1) == 1 {
			return aws.Config{}, errors.New("load failed " + testSecret)
		}
		return aws.Config{Credentials: aws.NewCredentialsCache(staticProvider(""))}, nil
	}

	req1 := newReq(t, "/model/x/converse", nil)
	err := s.Sign(context.Background(), req1, nil, testRegion, "")
	var ce *CredentialsError
	if !errors.As(err, &ce) || ce.Category != CategoryCredentialsUnavailable {
		t.Fatalf("first call err = %v", err)
	}
	assertNoSecret(t, err.Error())

	req2 := newReq(t, "/model/x/converse", nil)
	if err := s.Sign(context.Background(), req2, nil, testRegion, ""); err != nil {
		t.Fatalf("second call should succeed after retry: %v", err)
	}
	// Successful load is cached.
	if err := s.Sign(context.Background(), newReq(t, "/model/x/converse", nil), nil, testRegion, ""); err != nil {
		t.Fatal(err)
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("load calls = %d, want 2", got)
	}
}

func TestSign_NilProvider(t *testing.T) {
	t.Parallel()
	s := newSigner()
	s.load = func(context.Context) (aws.Config, error) { return aws.Config{}, nil }
	err := s.Sign(context.Background(), newReq(t, "/model/x/converse", nil), nil, testRegion, "")
	var ce *CredentialsError
	if !errors.As(err, &ce) || ce.Category != CategoryCredentialsUnavailable {
		t.Fatalf("err = %v", err)
	}
}

func TestCredentialsError(t *testing.T) {
	t.Parallel()
	cause := errors.New("cause with " + testSecret)
	var err error = &CredentialsError{Category: CategorySigningFailed, err: cause}
	if err.Error() != CategorySigningFailed {
		t.Errorf("Error() = %q", err.Error())
	}
	if !errors.Is(err, cause) {
		t.Error("errors.Is(cause) = false")
	}
	var ce *CredentialsError
	if !errors.As(err, &ce) {
		t.Error("errors.As failed")
	}
	assertNoSecret(t, err.Error())
}

func TestNewSigner_NoNetworkOnConstruction(t *testing.T) {
	t.Parallel()
	if NewSigner() == nil {
		t.Fatal("NewSigner returned nil")
	}
}

func assertNoSecret(t *testing.T, s string) {
	t.Helper()
	for _, secret := range []string{testSecret, testToken, "SECRET-IN-SDK-ERROR"} {
		if strings.Contains(s, secret) {
			t.Errorf("error string %q leaks %q", s, secret)
		}
	}
}
