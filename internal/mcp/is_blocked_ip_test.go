package mcp_test

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"github.com/voidmind-io/voidllm/internal/mcp"
)

func TestIsBlockedIP(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		ip   net.IP
		want bool
	}{
		{"ipv4 loopback", net.ParseIP("127.0.0.1"), true},
		{"ipv6 loopback", net.ParseIP("::1"), true},
		{"private 10/8", net.ParseIP("10.0.0.1"), true},
		{"private 172.16/12", net.ParseIP("172.16.0.1"), true},
		{"private 192.168/16", net.ParseIP("192.168.1.1"), true},
		{"cloud metadata", net.ParseIP("169.254.169.254"), true},
		{"link-local v4", net.ParseIP("169.254.1.1"), true},
		{"link-local v6", net.ParseIP("fe80::1"), true},
		{"link-local multicast v6", net.ParseIP("ff02::1"), true},
		{"unspecified v4", net.ParseIP("0.0.0.0"), true},
		{"unspecified v6", net.ParseIP("::"), true},
		{"mapped loopback", net.ParseIP("::ffff:127.0.0.1"), true},
		{"mapped private", net.ParseIP("::ffff:10.0.0.1"), true},
		{"shared space low", net.ParseIP("100.64.0.1"), true},
		{"shared space high", net.ParseIP("100.127.255.254"), true},
		{"nil", nil, true},
		{"public 8.8.8.8", net.ParseIP("8.8.8.8"), false},
		{"public 1.1.1.1", net.ParseIP("1.1.1.1"), false},
		{"just below shared space", net.ParseIP("100.63.255.255"), false},
		{"just above shared space", net.ParseIP("100.128.0.1"), false},
		{"public v6", net.ParseIP("2606:4700::1111"), false},
		{"mapped public", net.ParseIP("::ffff:8.8.8.8"), false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := mcp.IsBlockedIP(tc.ip); got != tc.want {
				t.Errorf("IsBlockedIP(%v) = %v, want %v", tc.ip, got, tc.want)
			}
		})
	}
}

func TestSSRFSafeTransport_UnspecifiedAddressDial(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	t.Cleanup(srv.Close)

	_, port, err := net.SplitHostPort(srv.Listener.Addr().String())
	if err != nil {
		t.Fatalf("split addr: %v", err)
	}

	hasV6Loopback := false
	if l, lerr := net.Listen("tcp6", "[::1]:0"); lerr == nil {
		hasV6Loopback = true
		_ = l.Close()
	}

	tests := []struct {
		name         string
		url          string
		allowPrivate bool
		needsV6      bool
		wantErr      bool
	}{
		{"guarded refuses 0.0.0.0", "http://0.0.0.0:" + port + "/", false, false, true},
		{"guarded refuses [::]", "http://[::]:" + port + "/", false, true, true},
		{"unrestricted reaches 0.0.0.0", "http://0.0.0.0:" + port + "/", true, false, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if tc.needsV6 && !hasV6Loopback {
				t.Skip("no IPv6 loopback available")
			}
			client := &http.Client{Transport: mcp.NewSSRFSafeTransport(tc.allowPrivate)}
			t.Cleanup(client.CloseIdleConnections)

			resp, err := client.Get(tc.url)
			if resp != nil {
				_ = resp.Body.Close()
			}
			if tc.wantErr {
				if err == nil {
					t.Fatalf("GET %s succeeded, want dial refusal", tc.url)
				}
				return
			}
			if err != nil {
				t.Fatalf("GET %s: %v", tc.url, err)
			}
			if resp.StatusCode != http.StatusOK {
				t.Errorf("status = %d, want 200", resp.StatusCode)
			}
		})
	}
}

func TestIsBlockedIP_TunnelingForms(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		ip   string
		want bool
	}{
		{"v4-compatible loopback", "::7f00:1", true},
		{"v4-compatible private", "::a00:1", true},
		{"v4-compatible metadata", "::a9fe:a9fe", true},
		{"v4-compatible public", "::808:808", false},
		{"6to4 loopback", "2002:7f00:1::1", true},
		{"6to4 private", "2002:0a00:0001::1", true},
		{"6to4 shared space", "2002:6440:1::1", true},
		{"6to4 public", "2002:0808:0808::1", false},
		{"nat64 well-known loopback", "64:ff9b::7f00:1", true},
		{"nat64 well-known metadata", "64:ff9b::a9fe:a9fe", true},
		{"nat64 well-known public", "64:ff9b::808:808", false},
		{"nat64 local-use private", "64:ff9b:1::a00:1", true},
		{"nat64 local-use public-looking blocked entirely", "64:ff9b:1::808:808", true},
		{"nat64 local-use /48 layout private", "64:ff9b:1:a00:0:100:808:808", true},
		{"nat64 local-use /48 layout public", "64:ff9b:1:808:8:0:808:0", true},
		{"teredo", "2001:0:4136:e378:8000:63bf:3fff:fdd2", true},
		{"teredo with public-looking client", "2001::1", true},
		{"2001:db8 documentation not teredo", "2001:db8::1", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ip := net.ParseIP(tc.ip)
			if ip == nil {
				t.Fatalf("bad test address %q", tc.ip)
			}
			if got := mcp.IsBlockedIP(ip); got != tc.want {
				t.Errorf("IsBlockedIP(%s) = %v, want %v", tc.ip, got, tc.want)
			}
		})
	}
}

func TestIsBlockedAddr_ZoneAndInvalid(t *testing.T) {
	t.Parallel()

	if !mcp.IsBlockedAddr(netip.MustParseAddr("fe80::1%eth0")) {
		t.Error("zoned link-local must be blocked")
	}
	if mcp.IsBlockedAddr(netip.MustParseAddr("2606:4700::1111%eth0")) {
		t.Error("zoned public address must not be blocked")
	}
	if !mcp.IsBlockedAddr(netip.Addr{}) {
		t.Error("invalid address must be blocked")
	}
}

func TestCheckDialAddress(t *testing.T) {
	t.Parallel()

	const internalMsg = "connection to internal address blocked"
	const metadataMsg = "connection to cloud metadata service blocked"

	tests := []struct {
		name    string
		address string
		wantMsg string
	}{
		{"zoned link-local", "[fe80::1%lo]:80", internalMsg},
		{"zoned loopback", "[::1%lo]:80", internalMsg},
		{"unparseable host", "not-an-ip.example:80", internalMsg},
		{"empty host", ":80", internalMsg},
		{"loopback", "127.0.0.1:80", internalMsg},
		{"mapped private", "[::ffff:10.0.0.1]:80", internalMsg},
		{"6to4 loopback", "[2002:7f00:1::1]:80", internalMsg},
		{"metadata", "169.254.169.254:80", metadataMsg},
		{"mapped metadata", "[::ffff:169.254.169.254]:80", metadataMsg},
		{"public v4", "8.8.8.8:443", ""},
		{"public v6", "[2606:4700::1111]:443", ""},
		{"bare public host without port", "8.8.8.8", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := mcp.CheckDialAddress(tc.address)
			if tc.wantMsg == "" {
				if err != nil {
					t.Fatalf("CheckDialAddress(%q) = %v, want nil", tc.address, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("CheckDialAddress(%q) = nil, want %q", tc.address, tc.wantMsg)
			}
			if err.Error() != tc.wantMsg {
				t.Errorf("CheckDialAddress(%q) = %q, want exactly %q (no address in text)", tc.address, err.Error(), tc.wantMsg)
			}
		})
	}
}

func TestSSRFSafeTransport_DialErrorDoesNotLeakAddress(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	t.Cleanup(srv.Close)

	_, port, err := net.SplitHostPort(srv.Listener.Addr().String())
	if err != nil {
		t.Fatalf("split addr: %v", err)
	}

	client := &http.Client{Transport: mcp.NewSSRFSafeTransport(false)}
	t.Cleanup(client.CloseIdleConnections)

	resp, err := client.Get(srv.URL)
	if resp != nil {
		_ = resp.Body.Close()
	}
	if err == nil {
		t.Fatal("request to loopback succeeded, want dial refusal")
	}
	if !errors.Is(err, mcp.ErrDialInternalBlocked) {
		t.Fatalf("error %v does not wrap ErrDialInternalBlocked", err)
	}

	got := mcp.SanitizeDialError(fmt.Errorf("transport: %w", err)).Error()
	if got != "connection to internal address blocked" {
		t.Errorf("sanitized error = %q, want fixed message", got)
	}
	for _, leak := range []string{"127.0.0.1", port} {
		if strings.Contains(got, leak) {
			t.Errorf("sanitized error %q contains %q", got, leak)
		}
	}

	other := errors.New("some other failure")
	if mcp.SanitizeDialError(other) != other {
		t.Error("non-dial errors must pass through unchanged")
	}
}
