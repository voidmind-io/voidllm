package admin_test

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/voidmind-io/voidllm/internal/api/admin"
	"github.com/voidmind-io/voidllm/internal/auth"
	"github.com/voidmind-io/voidllm/internal/cache"
	"github.com/voidmind-io/voidllm/internal/config"
	"github.com/voidmind-io/voidllm/internal/db"
	"github.com/voidmind-io/voidllm/internal/license"
)

func TestValidateMCPServerURL_BlockedLiterals(t *testing.T) {
	t.Parallel()

	type testCase struct {
		name         string
		url          string
		allowPrivate bool
		wantErr      bool
	}
	var tests []testCase

	add := func(name, url string, allow, wantErr bool) {
		tests = append(tests, testCase{name, url, allow, wantErr})
	}
	for _, u := range []string{
		"http://0.0.0.0",
		"http://0.0.0.0:8080",
		"http://[::]:8080",
		"http://[::ffff:127.0.0.1]/",
		"http://100.64.0.1",
		"http://LOCALHOST",
		"http://10.0.0.5/mcp",
		"http://169.254.169.254/",
		"http://[fe80::1%25eth0]/",
		"http://[::1%25lo]:8080/",
		"http://[2002:7f00:1::1]/",
		"http://[64:ff9b::7f00:1]/",
		"http://[2001:0:4136:e378:8000:63bf:3fff:fdd2]/",
		"http://[::7f00:1]/",
	} {
		add("rejects "+u, u, false, true)
		add("allowPrivate accepts "+u, u, true, false)
	}
	add("accepts public literal", "https://8.8.8.8/", false, false)

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := admin.ValidateMCPServerURL(tc.url, tc.allowPrivate)
			if (err != nil) != tc.wantErr {
				t.Errorf("ValidateMCPServerURL(%q, %v) error = %v, wantErr %v", tc.url, tc.allowPrivate, err, tc.wantErr)
			}
		})
	}
}

func TestIsPrivateHost_BlockedLiterals(t *testing.T) {
	t.Parallel()

	tests := []struct {
		host string
		want bool
	}{
		{"0.0.0.0", true},
		{"::", true},
		{"::ffff:127.0.0.1", true},
		{"::ffff:10.0.0.1", true},
		{"100.64.0.1", true},
		{"100.127.255.254", true},
		{"127.0.0.1", true},
		{"::1", true},
		{"169.254.169.254", true},
		{"fe80::1", true},
		{"fe80::1%eth0", true},
		{"fe80::1%25eth0", true},
		{"::1%lo", true},
		{"2606:4700::1111%eth0", false},
		{"bogus%eth0", true},
		{"%eth0", true},
		{"2002:7f00:1::1", true},
		{"64:ff9b::a9fe:a9fe", true},
		{"2001:0:1::1", true},
		{"LOCALHOST", true},
		{"8.8.8.8", false},
		{"100.128.0.1", false},
		{"2606:4700::1111", false},
		{"example.com", false},
	}
	for _, tc := range tests {
		t.Run(tc.host, func(t *testing.T) {
			t.Parallel()
			if got := admin.IsPrivateHost(tc.host); got != tc.want {
				t.Errorf("IsPrivateHost(%q) = %v, want %v", tc.host, got, tc.want)
			}
		})
	}
}

// countingListener counts accepted connections.
type countingListener struct {
	net.Listener
	accepted atomic.Int64
}

func (l *countingListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err == nil {
		l.accepted.Add(1)
	}
	return c, err
}

func setupSSOTestApp(t *testing.T, dsn string, logOut io.Writer) (*fiber.App, string) {
	t.Helper()

	ctx := context.Background()
	database, err := db.Open(ctx, config.DatabaseConfig{
		Driver:          "sqlite",
		DSN:             dsn,
		MaxOpenConns:    1,
		MaxIdleConns:    1,
		ConnMaxLifetime: time.Minute,
	})
	if err != nil {
		t.Fatalf("open test DB: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := db.RunMigrations(ctx, database.SQL(), db.SQLiteDialect{}, slog.Default()); err != nil {
		t.Fatalf("run migrations: %v", err)
	}

	keyCache := cache.New[string, auth.KeyInfo]()
	handler := &admin.Handler{
		DB:            database,
		HMACSecret:    testHMACSecret,
		EncryptionKey: testEncryptionKey,
		KeyCache:      keyCache,
		License:       license.NewHolder(license.Verify("", true)),
		Log:           slog.New(slog.NewTextHandler(logOut, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
	app := fiber.New()
	admin.RegisterRoutes(app, handler, keyCache, testHMACSecret, nil)
	key := addTestKey(t, keyCache, auth.RoleOrgAdmin, "org-sso")
	return app, key
}

func postSSOTest(t *testing.T, app *fiber.App, key, issuer string) (int, string) {
	t.Helper()
	body := `{"issuer":"` + issuer + `"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/orgs/org-sso/sso/test", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := app.Test(req, fiber.TestConfig{Timeout: 30 * time.Second})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, string(raw)
}

func TestTestSSOConnection_IssuerRules(t *testing.T) {
	t.Parallel()

	app, key := setupSSOTestApp(t, "file:TestTestSSOConnection_IssuerRules?mode=memory&cache=private", io.Discard)

	tests := []struct {
		name     string
		issuer   string
		wantBody string
	}{
		{"plain http rejected", "http://idp.example.com", "issuer must use https"},
		{"plain http localhost rejected", "http://localhost:8080", "issuer must use https"},
		{"https loopback literal", "https://127.0.0.1/", "must not point to a private address"},
		{"https unspecified v6", "https://[::]/", "must not point to a private address"},
		{"https unspecified v4", "https://0.0.0.0/", "must not point to a private address"},
		{"https shared space", "https://100.64.0.1/", "must not point to a private address"},
		{"https mapped loopback", "https://[::ffff:127.0.0.1]/", "must not point to a private address"},
		{"https localhost name", "https://localhost/", "must not point to a private address"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			status, body := postSSOTest(t, app, key, tc.issuer)
			if status != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body: %s", status, body)
			}
			if !strings.Contains(body, tc.wantBody) {
				t.Errorf("body %q does not contain %q", body, tc.wantBody)
			}
		})
	}
}

func TestTestSSOConnection_DiscoveryFailureIsFixedMessage(t *testing.T) {
	t.Parallel()

	// A hostname (not an IP literal) passes the literal check, so it reaches
	// the dial-time guard. Use a name that resolves locally to loopback
	// without needing DNS; skip when the host does not provide it.
	const hostName = "ip6-localhost"
	addrs, err := net.LookupHost(hostName)
	if err != nil || len(addrs) == 0 {
		t.Skipf("%s does not resolve on this host", hostName)
	}
	for _, a := range addrs {
		if !net.ParseIP(a).IsLoopback() {
			t.Skipf("%s resolves to non-loopback %s", hostName, a)
		}
	}

	const marker = "MARKER-7f3a9c-upstream-body"

	l, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skipf("no IPv6 loopback: %v", err)
	}
	cl := &countingListener{Listener: l}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, marker, http.StatusInternalServerError)
	}))
	srv.Listener = cl
	srv.StartTLS()
	t.Cleanup(srv.Close)

	_, port, err := net.SplitHostPort(l.Addr().String())
	if err != nil {
		t.Fatalf("split addr: %v", err)
	}

	logs := &syncBuffer{}
	app, key := setupSSOTestApp(t, "file:TestTestSSOConnection_Discovery?mode=memory&cache=private", logs)

	status, body := postSSOTest(t, app, key, "https://"+hostName+":"+port)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", status, body)
	}
	if !strings.Contains(body, `"status":"error"`) || !strings.Contains(body, `"message":"issuer discovery failed"`) {
		t.Errorf("body = %s, want status error with fixed message", body)
	}
	for _, leak := range []string{marker, hostName, port, "dial", "blocked", "x509"} {
		if strings.Contains(body, leak) {
			t.Errorf("response body contains %q: %s", leak, body)
		}
	}
	if strings.Contains(logs.String(), marker) {
		t.Errorf("log output contains upstream marker: %s", logs.String())
	}
	if !strings.Contains(logs.String(), "org-sso") {
		t.Errorf("log output missing org_id: %s", logs.String())
	}
	if n := cl.accepted.Load(); n != 0 {
		t.Errorf("listener accepted %d connections, want 0 (dial guard must refuse)", n)
	}
}

func TestValidateMCPServerURL_ZonedHostRules(t *testing.T) {
	t.Parallel()

	// A host that looks like a zoned literal but does not parse must be rejected.
	if err := admin.ValidateMCPServerURL("http://[bogus%25eth0]/", false); err == nil {
		t.Error("unparseable zoned host accepted, want rejection")
	}
	if err := admin.ValidateMCPServerURL("http://[fe80::1%25eth0]/", true); err != nil {
		t.Errorf("allowPrivate must accept zoned literal, got %v", err)
	}
}

func TestSSODiscoveryClient_RedirectNotFollowed(t *testing.T) {
	t.Parallel()

	var targetHits atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		targetHits.Add(1)
		_, _ = io.WriteString(w, "target")
	}))
	t.Cleanup(target.Close)

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	t.Cleanup(redirector.Close)

	client := admin.NewSSODiscoveryClientAllowPrivate()
	t.Cleanup(client.CloseIdleConnections)

	resp, err := client.Get(redirector.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Errorf("status = %d, want 302 (redirect must not be followed)", resp.StatusCode)
	}
	if n := targetHits.Load(); n != 0 {
		t.Errorf("redirect target received %d requests, want 0", n)
	}
}

func TestSSODiscoveryClient_ProductionGuardRefusesLoopback(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	t.Cleanup(srv.Close)

	client := admin.NewSSODiscoveryClient()
	t.Cleanup(client.CloseIdleConnections)
	resp, err := client.Get(srv.URL)
	if resp != nil {
		_ = resp.Body.Close()
	}
	if err == nil {
		t.Fatal("production discovery client reached a loopback server")
	}
}

func TestSSODiscoveryClient_BodySizeLimit(t *testing.T) {
	t.Parallel()

	const chunk = 64 << 10
	var written atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		buf := make([]byte, chunk)
		for i := 0; i < 64; i++ { // up to 4 MiB
			n, err := w.Write(buf)
			written.Add(int64(n))
			if err != nil {
				return
			}
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
	}))
	t.Cleanup(srv.Close)

	client := admin.NewSSODiscoveryClientAllowPrivate()
	t.Cleanup(client.CloseIdleConnections)

	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	n, err := io.Copy(io.Discard, resp.Body)
	if err == nil {
		t.Fatalf("read %d bytes without error, want size-limit error", n)
	}
	if !strings.Contains(err.Error(), "too large") {
		t.Errorf("error = %v, want size-limit error", err)
	}
	if n > 1<<20 {
		t.Errorf("delivered %d bytes to the reader, want at most 1 MiB", n)
	}

	small := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(make([]byte, 1<<20))
	}))
	t.Cleanup(small.Close)
	resp2, err := client.Get(small.URL)
	if err != nil {
		t.Fatalf("GET small: %v", err)
	}
	defer resp2.Body.Close()
	if m, err := io.Copy(io.Discard, resp2.Body); err != nil || m != 1<<20 {
		t.Errorf("exactly-at-limit body: read %d, err %v; want full read without error", m, err)
	}
}
