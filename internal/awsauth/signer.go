// Package awsauth signs outbound HTTP requests with AWS Signature Version 4
// using the AWS default credential chain, optionally assuming an IAM role.
//
// Credentials are resolved lazily on first use so that constructing a Signer
// performs no network calls. Neither credentials nor SDK error text are ever
// exposed through returned errors: failures are reported as *CredentialsError
// whose message is a short fixed category.
package awsauth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

const (
	// serviceName is the SigV4 service name for the Bedrock runtime API.
	serviceName = "bedrock"
	// roleSessionName is the STS session name used when assuming a role.
	roleSessionName = "voidllm"
	// payloadHashHeader carries the hex SHA-256 of the request body.
	payloadHashHeader = "X-Amz-Content-Sha256"

	// CategoryCredentialsUnavailable is reported when the base credentials
	// cannot be loaded or retrieved.
	CategoryCredentialsUnavailable = "credentials_unavailable"
	// CategoryAssumeRoleFailed is reported when assuming the configured role fails.
	CategoryAssumeRoleFailed = "assume_role_failed"
	// CategorySigningFailed is reported when computing the signature fails.
	CategorySigningFailed = "signing_failed"
)

// CredentialsError reports a failure to obtain AWS credentials or to sign a
// request. Error returns only the short Category so that accidental logging
// or propagation leaks nothing; the underlying SDK error is reachable through
// Unwrap for errors.Is and errors.As but must never be logged or returned to
// clients, because it may embed response bodies.
type CredentialsError struct {
	// Category is a short fixed identifier such as "credentials_unavailable".
	Category string
	err      error
}

// Error returns the failure category only.
func (e *CredentialsError) Error() string { return e.Category }

// LogValue implements slog.LogValuer and exposes only the category.
func (e *CredentialsError) LogValue() slog.Value {
	return slog.StringValue(e.Category)
}

// Format implements fmt.Formatter so every verb (%v, %+v, %s, %q) prints only
// the category and never the wrapped SDK error.
func (e *CredentialsError) Format(f fmt.State, verb rune) {
	if verb == 'q' {
		_, _ = fmt.Fprintf(f, "%q", e.Category)
		return
	}
	_, _ = io.WriteString(f, e.Category)
}

// Unwrap returns the underlying SDK error.
func (e *CredentialsError) Unwrap() error { return e.err }

const (
	// credentialsExpiryWindow makes cached credentials refresh this long before
	// they expire, so a request never signs with credentials about to lapse.
	credentialsExpiryWindow = 5 * time.Minute
	// credentialsExpiryJitterFrac randomizes the refresh point within the
	// window so concurrent caches do not refresh in lockstep.
	credentialsExpiryJitterFrac = 0.5
)

// newCredentialsCache wraps p in a credentials cache with the package expiry
// window and jitter.
func newCredentialsCache(p aws.CredentialsProvider) *aws.CredentialsCache {
	return aws.NewCredentialsCache(p, func(o *aws.CredentialsCacheOptions) {
		o.ExpiryWindow = credentialsExpiryWindow
		o.ExpiryWindowJitterFrac = credentialsExpiryJitterFrac
	})
}

// roleProviderFactory builds a credentials provider that assumes roleARN in
// region on top of the base configuration.
type roleProviderFactory func(base aws.Config, roleARN, region string) aws.CredentialsProvider

// baseLoader loads the base credentials provider and configuration.
type baseLoader func(ctx context.Context) (aws.Config, error)

// Signer signs HTTP requests for AWS Bedrock with SigV4. It is safe for
// concurrent use.
type Signer struct {
	signer *v4.Signer
	load   baseLoader
	newRP  roleProviderFactory

	mu    sync.Mutex
	base  *aws.Config
	roles map[string]aws.CredentialsProvider
}

// signerOption customizes a Signer; it exists for tests.
type signerOption func(*Signer)

// withBaseProvider makes the Signer use provider as its base credentials
// instead of the AWS default chain.
func withBaseProvider(provider aws.CredentialsProvider) signerOption {
	return func(s *Signer) {
		s.load = func(context.Context) (aws.Config, error) {
			return aws.Config{Credentials: newCredentialsCache(provider)}, nil
		}
	}
}

// withRoleProviderFactory overrides how role-assuming providers are built.
func withRoleProviderFactory(f roleProviderFactory) signerOption {
	return func(s *Signer) { s.newRP = f }
}

// NewSigner returns a Signer backed by the AWS default credential chain. No
// network calls are made until the first Sign call.
func NewSigner() *Signer {
	return newSigner()
}

func newSigner(opts ...signerOption) *Signer {
	s := &Signer{
		signer: v4.NewSigner(),
		load: func(ctx context.Context) (aws.Config, error) {
			cfg, err := awsconfig.LoadDefaultConfig(ctx)
			if err != nil {
				return aws.Config{}, err
			}
			cfg.Credentials = newCredentialsCache(cfg.Credentials)
			return cfg, nil
		},
		newRP: func(base aws.Config, roleARN, region string) aws.CredentialsProvider {
			cfg := base.Copy()
			cfg.Region = region
			return stscreds.NewAssumeRoleProvider(sts.NewFromConfig(cfg), roleARN, func(o *stscreds.AssumeRoleOptions) {
				o.RoleSessionName = roleSessionName
			})
		},
		roles: make(map[string]aws.CredentialsProvider),
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// baseConfig returns the lazily loaded base configuration. A failed load is
// not cached, so the next call retries.
func (s *Signer) baseConfig(ctx context.Context) (aws.Config, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.base != nil {
		return *s.base, nil
	}
	cfg, err := s.load(ctx)
	if err != nil {
		return aws.Config{}, err
	}
	s.base = &cfg
	return cfg, nil
}

// provider returns the credentials provider for roleARN in region, or the
// base provider when roleARN is empty.
func (s *Signer) provider(ctx context.Context, region, roleARN string) (aws.CredentialsProvider, error) {
	cfg, err := s.baseConfig(ctx)
	if err != nil {
		return nil, &CredentialsError{Category: CategoryCredentialsUnavailable, err: err}
	}
	if roleARN == "" {
		return cfg.Credentials, nil
	}
	key := roleARN + "|" + region
	s.mu.Lock()
	defer s.mu.Unlock()
	if p, ok := s.roles[key]; ok {
		return p, nil
	}
	p := newCredentialsCache(s.newRP(cfg, roleARN, region))
	s.roles[key] = p
	return p, nil
}

// Sign adds a SigV4 signature for the Bedrock service to req. body must be the
// exact bytes sent as the request body. When roleARN is non-empty the signature
// uses credentials from assuming that role. Failures to obtain credentials are
// returned as *CredentialsError.
func (s *Signer) Sign(ctx context.Context, req *http.Request, body []byte, region, roleARN string) error {
	prov, err := s.provider(ctx, region, roleARN)
	if err != nil {
		return err
	}
	if prov == nil {
		return &CredentialsError{Category: CategoryCredentialsUnavailable, err: errors.New("no credentials provider")}
	}
	creds, err := prov.Retrieve(ctx)
	if err != nil {
		category := CategoryCredentialsUnavailable
		if roleARN != "" {
			category = CategoryAssumeRoleFailed
		}
		return &CredentialsError{Category: category, err: err}
	}

	sum := sha256.Sum256(body)
	payloadHash := hex.EncodeToString(sum[:])
	req.Header.Set(payloadHashHeader, payloadHash)

	if err := s.signer.SignHTTP(ctx, creds, req, payloadHash, serviceName, region, time.Now().UTC()); err != nil {
		return &CredentialsError{Category: CategorySigningFailed, err: fmt.Errorf("sign http: %w", err)}
	}
	return nil
}
