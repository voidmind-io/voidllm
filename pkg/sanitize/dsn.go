// Package sanitize provides utilities for redacting sensitive values from
// strings before they appear in logs or error messages.
package sanitize

import (
	"net/url"
	"regexp"
)

// reKVPassword matches a password key-value pair in a DSN string, e.g.
// "password=secret" or "password = secret", case-insensitively.
var reKVPassword = regexp.MustCompile(`(?i)(password\s*=\s*)(\S+)`)

// reURLPassword matches the password portion of a URL authority section, e.g.
// "://user:s3cr3t@" captures the password between the colon and the "@".
var reURLPassword = regexp.MustCompile(`(://[^:@/]*:)([^@]*)(@)`)

// DSN removes credentials from database connection strings so they are safe
// to include in log output and error messages.
//
// URL-style DSNs (e.g. "postgres://user:pass@host/db") have the password
// component replaced with "***". Key-value style DSNs (e.g.
// "host=x password=secret dbname=y") have the password value replaced with
// "***". DSNs that contain neither form are returned unchanged.
func DSN(dsn string) string {
	// Detect URL-style DSNs by parsing and checking for a password. If one is
	// present, redact it with a regex substitution so the result contains the
	// literal string "***" rather than a percent-encoded sentinel.
	if u, err := url.Parse(dsn); err == nil && u.User != nil {
		if _, hasPass := u.User.Password(); hasPass {
			return reURLPassword.ReplaceAllString(dsn, "${1}***${3}")
		}
	}

	// Key-value style: redact the value that follows "password=".
	return reKVPassword.ReplaceAllString(dsn, "${1}***")
}
