package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// OAuthConfig holds the Client Credentials Flow configuration for an MCP server.
type OAuthConfig struct {
	// TokenURL is the OAuth2 token endpoint (must use HTTPS). Optional when
	// ServerURL is set — auto-discovery is attempted via RFC 8414 when empty.
	TokenURL string
	// ServerURL is the base URL of the MCP server. Used for RFC 8414 token
	// endpoint auto-discovery when TokenURL is empty.
	ServerURL string
	// ClientID is the OAuth2 client identifier.
	ClientID string
	// ClientSecret is the plaintext (decrypted) OAuth2 client secret.
	ClientSecret string
	// Scopes is an optional space-separated list of OAuth2 scopes to request.
	Scopes string
}

// cachedToken holds an access token alongside its expiry time.
type cachedToken struct {
	accessToken string
	expiresAt   time.Time
}

// OAuthTokenManager caches and refreshes OAuth2 access tokens for MCP servers
// using the Client Credentials Flow (RFC 6749 Section 4.4). All methods are
// safe for concurrent use.
type OAuthTokenManager struct {
	mu     sync.RWMutex
	tokens map[string]*cachedToken
	client *http.Client
	flight singleflight.Group
}

// NewOAuthTokenManager creates a token manager with the given HTTP client.
// When client is nil a default client with a 30-second timeout is used.
func NewOAuthTokenManager(client *http.Client) *OAuthTokenManager {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	return &OAuthTokenManager{
		tokens: make(map[string]*cachedToken),
		client: client,
	}
}

// GetToken returns a valid access token for the server identified by serverID.
// Cached tokens are reused until they expire. Expired or missing tokens trigger
// a new Client Credentials grant. Concurrent callers for the same serverID are
// deduplicated via singleflight — only one fetch runs at a time.
func (m *OAuthTokenManager) GetToken(ctx context.Context, serverID string, cfg OAuthConfig) (string, error) {
	// Fast path: read lock only, no contention with other readers.
	m.mu.RLock()
	if t, ok := m.tokens[serverID]; ok && time.Now().Before(t.expiresAt) {
		m.mu.RUnlock()
		return t.accessToken, nil
	}
	m.mu.RUnlock()

	// Slow path: singleflight deduplicates concurrent fetches for the same server.
	v, err, _ := m.flight.Do(serverID, func() (any, error) {
		// Double-check under read lock; another goroutine may have populated the
		// cache while this one was waiting for the singleflight slot.
		m.mu.RLock()
		if t, ok := m.tokens[serverID]; ok && time.Now().Before(t.expiresAt) {
			m.mu.RUnlock()
			return t.accessToken, nil
		}
		m.mu.RUnlock()

		tokenURL := cfg.TokenURL
		if tokenURL == "" && cfg.ServerURL != "" {
			discovered, discErr := m.discoverTokenURL(ctx, cfg.ServerURL)
			if discErr != nil {
				return nil, fmt.Errorf("oauth token_url not configured and auto-discovery failed: %w", discErr)
			}
			tokenURL = discovered
		}
		if tokenURL == "" {
			return nil, fmt.Errorf("oauth token_url is empty and no server URL available for auto-discovery")
		}

		token, expiresIn, err := m.fetchToken(ctx, tokenURL, cfg)
		if err != nil {
			return nil, err
		}

		// Cache with 30-second buffer before actual expiry to account for clock skew
		// and token validation overhead on the upstream server.
		expiry := time.Now().Add(time.Duration(expiresIn)*time.Second - 30*time.Second)
		if expiry.Before(time.Now()) {
			expiry = time.Now().Add(30 * time.Second) // minimum 30-second cache
		}

		m.mu.Lock()
		m.tokens[serverID] = &cachedToken{accessToken: token, expiresAt: expiry}
		m.mu.Unlock()

		return token, nil
	})
	if err != nil {
		return "", err
	}
	return v.(string), nil
}

// Evict removes the cached token for the given server ID. It is a no-op when
// the server has no cached token. Call this on server deletion or credential
// rotation to force a fresh grant on the next GetToken call.
func (m *OAuthTokenManager) Evict(serverID string) {
	m.mu.Lock()
	delete(m.tokens, serverID)
	m.mu.Unlock()
}

// discoverTokenURL attempts RFC 8414 OAuth Authorization Server Metadata
// discovery at serverURL/.well-known/oauth-authorization-server and returns
// the token_endpoint from the response.
func (m *OAuthTokenManager) discoverTokenURL(ctx context.Context, serverURL string) (string, error) {
	base := strings.TrimSuffix(serverURL, "/")
	wellKnownURL := base + "/.well-known/oauth-authorization-server"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, wellKnownURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json")

	resp, err := m.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("discovery request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("discovery endpoint returned %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}

	var meta struct {
		TokenEndpoint string `json:"token_endpoint"`
	}
	if err := json.Unmarshal(body, &meta); err != nil {
		return "", err
	}
	if meta.TokenEndpoint == "" {
		return "", fmt.Errorf("no token_endpoint in discovery response")
	}
	if !strings.HasPrefix(meta.TokenEndpoint, "https://") {
		return "", fmt.Errorf("discovered token_endpoint must use HTTPS")
	}
	return meta.TokenEndpoint, nil
}

// fetchToken performs a Client Credentials grant against tokenURL and returns
// the access token together with the server-advertised expiry in seconds. A
// default of 3600 seconds is used when the server omits expires_in.
func (m *OAuthTokenManager) fetchToken(ctx context.Context, tokenURL string, cfg OAuthConfig) (token string, expiresIn int64, err error) {
	form := url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {cfg.ClientID},
		"client_secret": {cfg.ClientSecret},
	}
	if cfg.Scopes != "" {
		form.Set("scope", cfg.Scopes)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL,
		strings.NewReader(form.Encode()))
	if err != nil {
		return "", 0, fmt.Errorf("build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := m.client.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("token request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// Discard the body — error response content must not be forwarded as it
		// may contain sensitive information from the upstream authorization server.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return "", 0, fmt.Errorf("token endpoint returned HTTP %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 1 MiB limit
	if err != nil {
		return "", 0, fmt.Errorf("read token response: %w", err)
	}

	var tokenResp struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return "", 0, fmt.Errorf("parse token response: %w", err)
	}
	if tokenResp.AccessToken == "" {
		return "", 0, fmt.Errorf("token response missing access_token")
	}

	expiresIn = tokenResp.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = 3600 // default 1 hour when the server does not advertise expiry
	}

	return tokenResp.AccessToken, expiresIn, nil
}
