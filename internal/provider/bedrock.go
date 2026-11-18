package provider

import (
	"errors"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
)

// Bedrock is the provider name for AWS Bedrock (Converse API).
const Bedrock = "bedrock"

var (
	bedrockRegionRe  = regexp.MustCompile(`^[a-z]{2}(-[a-z]+)+-[0-9]{1,2}$`)
	bedrockModelIDRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]*$`)
	bedrockRoleARNRe = regexp.MustCompile(`^arn:aws[a-z-]*:iam::\d{12}:role/[\w+=,.@/-]{1,512}$`)
)

// Bedrock authentication modes.
const (
	// BedrockAuthAPIKey authenticates with a Bedrock API key sent as a Bearer token.
	BedrockAuthAPIKey = "api_key"
	// BedrockAuthIAM authenticates with AWS SigV4 using the AWS credential chain.
	BedrockAuthIAM = "iam"
)

// ValidBedrockRoleARN reports whether arn is a syntactically valid IAM role ARN.
func ValidBedrockRoleARN(arn string) bool {
	return bedrockRoleARNRe.MatchString(arn)
}

// ErrBedrockIAMEndpoint is returned by ValidBedrockIAMEndpoint when an endpoint
// is not an AWS HTTPS host that SigV4 credentials may be sent to.
var ErrBedrockIAMEndpoint = errors.New("bedrock iam requires an https base_url on an amazonaws.com, amazonaws.com.cn or api.aws host")

// bedrockIAMHostSuffixes are the DNS suffixes SigV4-signed requests may target.
var bedrockIAMHostSuffixes = []string{".amazonaws.com", ".amazonaws.com.cn", ".api.aws"}

// ValidBedrockIAMEndpoint reports whether baseURL is an endpoint to which
// IAM-signed requests may be sent: https, no userinfo, a DNS host name (not an
// IP literal) ending in .amazonaws.com, .amazonaws.com.cn or .api.aws,
// compared case-insensitively. It returns ErrBedrockIAMEndpoint otherwise.
// This prevents a misconfigured or malicious base_url from receiving signed
// requests and the credential-derived signature headers.
func ValidBedrockIAMEndpoint(baseURL string) error {
	u, err := url.Parse(baseURL)
	if err != nil || u.Scheme != "https" || u.User != nil {
		return ErrBedrockIAMEndpoint
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if host == "" {
		return ErrBedrockIAMEndpoint
	}
	if _, perr := netip.ParseAddr(host); perr == nil {
		return ErrBedrockIAMEndpoint
	}
	for _, suffix := range bedrockIAMHostSuffixes {
		if strings.HasSuffix(host, suffix) && len(host) > len(suffix) {
			return nil
		}
	}
	return ErrBedrockIAMEndpoint
}

// NormalizeBedrockAuth returns the effective Bedrock auth mode, mapping the
// empty string to BedrockAuthAPIKey.
func NormalizeBedrockAuth(auth string) string {
	if auth == "" {
		return BedrockAuthAPIKey
	}
	return auth
}

// ValidateBedrockAuth checks a Bedrock auth configuration and returns a short
// error message, or the empty string when the combination is valid. auth may be
// empty (meaning api_key). hasAPIKey reports whether an upstream API key is set.
func ValidateBedrockAuth(auth, roleARN string, hasAPIKey bool) string {
	switch NormalizeBedrockAuth(auth) {
	case BedrockAuthAPIKey:
		if roleARN != "" {
			return "bedrock_role_arn requires bedrock_auth iam"
		}
		if !hasAPIKey {
			return "api_key is required for bedrock when bedrock_auth is api_key"
		}
	case BedrockAuthIAM:
		if hasAPIKey {
			return "api_key must be empty when bedrock_auth is iam"
		}
		if roleARN != "" && !ValidBedrockRoleARN(roleARN) {
			return "bedrock_role_arn is invalid"
		}
	default:
		return "bedrock_auth must be api_key or iam"
	}
	return ""
}

// ValidBedrockRegion reports whether r is a syntactically valid AWS region
// name. The check is strict because the region is interpolated into a host
// name: values such as "eu-central-1.evil.com" are rejected.
func ValidBedrockRegion(r string) bool {
	return bedrockRegionRe.MatchString(r)
}

// ValidBedrockModelID reports whether id is an acceptable Bedrock model
// identifier: a base model ID, a cross-region inference profile ID
// ("eu.", "us.", "global." prefixes) or an ARN.
func ValidBedrockModelID(id string) bool {
	return len(id) >= 1 && len(id) <= 2048 && bedrockModelIDRe.MatchString(id)
}

// BedrockRuntimeBaseURL returns the regional Bedrock runtime endpoint for
// region. The region must already be validated with ValidBedrockRegion.
func BedrockRuntimeBaseURL(region string) string {
	return "https://bedrock-runtime." + region + ".amazonaws.com"
}

// EffectiveBaseURL returns baseURL when it is non-empty. For the bedrock
// provider with an empty baseURL and a valid region it returns the derived
// regional endpoint. In every other case it returns baseURL unchanged.
func EffectiveBaseURL(providerName, baseURL, region string) string {
	if baseURL != "" {
		return baseURL
	}
	if providerName == Bedrock && ValidBedrockRegion(region) {
		return BedrockRuntimeBaseURL(region)
	}
	return baseURL
}

// EscapeBedrockModelID percent-encodes id for use as a single URL path
// segment. Every byte outside A-Z a-z 0-9 - . _ ~ is encoded, so ":" becomes
// "%3A" and "/" becomes "%2F". This matches the canonical URI escaping that
// AWS signing expects.
func EscapeBedrockModelID(id string) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	b.Grow(len(id))
	for i := 0; i < len(id); i++ {
		c := id[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9',
			c == '-', c == '.', c == '_', c == '~':
			b.WriteByte(c)
		default:
			b.WriteByte('%')
			b.WriteByte(hex[c>>4])
			b.WriteByte(hex[c&0x0f])
		}
	}
	return b.String()
}
