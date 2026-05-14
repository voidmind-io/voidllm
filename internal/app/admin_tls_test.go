package app

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/voidmind-io/voidllm/internal/api/health"
	"github.com/voidmind-io/voidllm/internal/config"
)

// freePort opens 127.0.0.1:0, captures the port, closes the listener, returns
// the port. Brief reuse race — callers must poll the server with retry.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port
}

// genSelfSignedCert writes a self-signed RSA-2048 cert + key to t.TempDir().
// Cert is valid for 127.0.0.1, ::1, localhost; 1-hour validity. Returns paths.
func genSelfSignedCert(t *testing.T) (string, string) {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa.GenerateKey: %v", err)
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "voidllm-test"},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
		DNSNames:     []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("CreateCertificate: %v", err)
	}
	dir := t.TempDir()
	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(priv)})
	if err := os.WriteFile(certPath, certPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath
}

// newAdminTLSTestApp synthesises a minimal *Application: only the fields
// startListening() touches. No DB, no auth, no admin handler. /healthz only.
func newAdminTLSTestApp(t *testing.T, proxyPort, adminPort int, tlsEnabled bool, certPath, keyPath string) *Application {
	t.Helper()
	proxyApp := fiber.New()
	proxyApp.Get("/healthz", health.Liveness())
	adminApp := fiber.New()
	adminApp.Get("/healthz", health.Liveness())
	return &Application{
		cfg: &config.Config{Server: config.ServerConfig{
			Proxy: config.ProxyConfig{Port: proxyPort},
			Admin: config.AdminConfig{
				Port: adminPort,
				TLS:  config.TLSConfig{Enabled: tlsEnabled, Cert: certPath, Key: keyPath},
			},
		}},
		log:      slog.New(slog.DiscardHandler),
		proxyApp: proxyApp,
		adminApp: adminApp,
	}
}

func waitTLS(addr string, deadline time.Time) bool {
	for time.Now().Before(deadline) {
		c, err := tls.Dial("tcp", addr, &tls.Config{InsecureSkipVerify: true}) //nolint:gosec
		if err == nil {
			_ = c.Close()
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

func waitHTTP(addr string, deadline time.Time) bool {
	for time.Now().Before(deadline) {
		resp, err := http.Get("http://" + addr + "/healthz") //nolint:noctx
		if err == nil {
			_ = resp.Body.Close()
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func TestAdminTLS_Enabled(t *testing.T) {
	certPath, keyPath := genSelfSignedCert(t)
	adminPort := freePort(t)
	a := newAdminTLSTestApp(t, freePort(t), adminPort, true, certPath, keyPath)
	a.startListening()
	t.Cleanup(func() { _ = a.adminApp.Shutdown(); _ = a.proxyApp.Shutdown() })

	addr := fmt.Sprintf("127.0.0.1:%d", adminPort)
	if !waitTLS(addr, time.Now().Add(3*time.Second)) {
		t.Fatalf("admin TLS never came up on %s", addr)
	}
	client := &http.Client{
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}, //nolint:gosec
		Timeout:   2 * time.Second,
	}
	resp, err := client.Get("https://" + addr + "/healthz")
	if err != nil {
		t.Fatalf("https GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	if _, err := (&http.Client{Timeout: time.Second}).Get("http://" + addr + "/healthz"); err == nil { //nolint:noctx
		t.Fatalf("plain HTTP should have failed against TLS server")
	}
}

func TestAdminTLS_Disabled(t *testing.T) {
	adminPort := freePort(t)
	a := newAdminTLSTestApp(t, freePort(t), adminPort, false, "", "")
	a.startListening()
	t.Cleanup(func() { _ = a.adminApp.Shutdown(); _ = a.proxyApp.Shutdown() })

	addr := fmt.Sprintf("127.0.0.1:%d", adminPort)
	if !waitHTTP(addr, time.Now().Add(3*time.Second)) {
		t.Fatalf("admin HTTP never came up on %s", addr)
	}
	if c, err := tls.Dial("tcp", addr, &tls.Config{InsecureSkipVerify: true}); err == nil { //nolint:gosec
		_ = c.Close()
		t.Fatalf("TLS handshake unexpectedly succeeded against plain HTTP server")
	}
}

func TestAdminTLS_SinglePortWarn(t *testing.T) {
	buf := &syncBuf{}
	logger := slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	a := &Application{
		cfg: &config.Config{Server: config.ServerConfig{
			Proxy: config.ProxyConfig{Port: 8080},
			Admin: config.AdminConfig{Port: 0, TLS: config.TLSConfig{Enabled: true, Cert: "x", Key: "y"}},
		}},
		log: logger,
	}
	a.warnIfSinglePortTLS(0)
	if !strings.Contains(buf.String(), "admin TLS configured but ignored in single-port mode") {
		t.Fatalf("expected warn log, got: %q", buf.String())
	}

	// Regression: TLS disabled → no warn.
	buf2 := &syncBuf{}
	a.log = slog.New(slog.NewTextHandler(buf2, &slog.HandlerOptions{Level: slog.LevelWarn}))
	a.cfg.Server.Admin.TLS.Enabled = false
	a.warnIfSinglePortTLS(0)
	if buf2.String() != "" {
		t.Fatalf("expected no log when TLS disabled, got: %q", buf2.String())
	}
}
