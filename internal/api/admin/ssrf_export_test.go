package admin

import "net/http"

// ValidateMCPServerURL exposes validateMCPServerURL for black-box tests.
var ValidateMCPServerURL = validateMCPServerURL

// IsPrivateHost exposes isPrivateHost for black-box tests.
var IsPrivateHost = isPrivateHost

// NewSSODiscoveryClientAllowPrivate exposes the SSO discovery client builder
// with the dial guard disabled so tests can reach local servers.
func NewSSODiscoveryClientAllowPrivate() *http.Client {
	return newSSODiscoveryClientWithGuard(true)
}

// NewSSODiscoveryClient exposes the production SSO discovery client builder.
var NewSSODiscoveryClient = newSSODiscoveryClient
