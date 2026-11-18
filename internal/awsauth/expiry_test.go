package awsauth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
)

// TestCredentialsCache_ExpiryWindowApplied verifies credentials that expire
// within the safety window are refreshed before use.
func TestCredentialsCache_ExpiryWindowApplied(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	p := aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
		calls.Add(1)
		return aws.Credentials{
			AccessKeyID: "AKIA", SecretAccessKey: "s",
			CanExpire: true, Expires: time.Now().Add(2 * time.Minute),
		}, nil
	})
	cache := newCredentialsCache(p)
	for i := 0; i < 3; i++ {
		if _, err := cache.Retrieve(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	// 2 minutes is inside the minimum effective window (2.5 minutes), so every
	// retrieval must refresh.
	if got := calls.Load(); got != 3 {
		t.Errorf("provider calls = %d, want 3 (credentials always inside expiry window)", got)
	}

	var longCalls atomic.Int32
	long := aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
		longCalls.Add(1)
		return aws.Credentials{
			AccessKeyID: "AKIA", SecretAccessKey: "s",
			CanExpire: true, Expires: time.Now().Add(30 * time.Minute),
		}, nil
	})
	longCache := newCredentialsCache(long)
	for i := 0; i < 3; i++ {
		if _, err := longCache.Retrieve(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if got := longCalls.Load(); got != 1 {
		t.Errorf("long-lived provider calls = %d, want 1", got)
	}
}

// TestSigner_BaseAndRoleCachesUseExpiryWindow verifies the Signer's own base
// and role caches refresh near-expiry credentials.
func TestSigner_BaseAndRoleCachesUseExpiryWindow(t *testing.T) {
	t.Parallel()
	var baseCalls, roleCalls atomic.Int32
	near := func(counter *atomic.Int32) aws.CredentialsProvider {
		return aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			counter.Add(1)
			return aws.Credentials{AccessKeyID: "AKIA", SecretAccessKey: "s", CanExpire: true, Expires: time.Now().Add(2 * time.Minute)}, nil
		})
	}
	s := newSigner(
		withBaseProvider(near(&baseCalls)),
		withRoleProviderFactory(func(aws.Config, string, string) aws.CredentialsProvider { return near(&roleCalls) }),
	)
	for i := 0; i < 2; i++ {
		if err := s.Sign(context.Background(), newReq(t, "/model/x/converse", nil), nil, "eu-central-1", ""); err != nil {
			t.Fatal(err)
		}
		if err := s.Sign(context.Background(), newReq(t, "/model/x/converse", nil), nil, "eu-central-1", "arn:aws:iam::123456789012:role/r"); err != nil {
			t.Fatal(err)
		}
	}
	if baseCalls.Load() != 2 || roleCalls.Load() != 2 {
		t.Errorf("base calls = %d, role calls = %d, want 2 each", baseCalls.Load(), roleCalls.Load())
	}
}

// TestCredentialsError_NeverPrintsWrappedError is a regression test: no
// formatting path may reveal the wrapped SDK error text.
func TestCredentialsError_NeverPrintsWrappedError(t *testing.T) {
	t.Parallel()
	const secret = "SECRET-BODY-TEXT"
	err := &CredentialsError{Category: CategoryCredentialsUnavailable, err: errors.New(secret)}

	for _, format := range []string{"%v", "%+v", "%#v", "%s", "%q", "%d"} {
		if out := fmt.Sprintf(format, err); strings.Contains(out, secret) {
			t.Errorf("format %s leaked wrapped error: %q", format, out)
		}
	}
	if got := fmt.Sprintf("%+v", err); got != CategoryCredentialsUnavailable {
		t.Errorf("%%+v = %q, want category only", got)
	}
	wrapped := fmt.Errorf("sign: %w", err)
	if out := fmt.Sprintf("%+v", wrapped); strings.Contains(out, secret) {
		t.Errorf("wrapped %%+v leaked: %q", out)
	}

	var buf strings.Builder
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	logger.Info("x", slog.Any("err", err))
	logger.Info("y", slog.Any("err", wrapped))
	if strings.Contains(buf.String(), secret) {
		t.Errorf("slog output leaked wrapped error: %s", buf.String())
	}
	if !strings.Contains(buf.String(), CategoryCredentialsUnavailable) {
		t.Errorf("slog output missing category: %s", buf.String())
	}
	if !errors.Is(err, err.err) {
		t.Error("wrapped error must stay reachable through errors.Is")
	}
}
