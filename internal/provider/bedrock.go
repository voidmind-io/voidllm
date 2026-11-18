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
// is not a Bedrock Runtime host that SigV4 credentials may be sent to.
var ErrBedrockIAMEndpoint = errors.New("bedrock iam requires an https Bedrock Runtime endpoint for the configured region")

// bedrockVPCEndpointRe matches the middle part of a Bedrock Runtime VPC
// interface endpoint DNS name (regional or AZ-specific), without the region.
var bedrockVPCEndpointRe = regexp.MustCompile(`^vpce-[a-z0-9]+(-[a-z0-9]+)*$`)

// ValidBedrockIAMEndpoint reports whether baseURL is an endpoint to which
// IAM-signed requests for region may be sent. The URL must use https, have no
// userinfo, query or fragment, port empty or 443, and a DNS host name (not an
// IP literal) that is exactly one of the documented Bedrock Runtime hosts for
// region (compared case-insensitively, one trailing dot ignored):
//
//   - bedrock-runtime.R.amazonaws.com and bedrock-runtime-fips.R.amazonaws.com
//   - bedrock-runtime.R.amazonaws.com.cn
//   - bedrock-runtime.R.api.aws and bedrock-runtime-fips.R.api.aws
//   - VPC interface endpoints: vpce-<id>[-<az>].bedrock-runtime[-fips].R.vpce.amazonaws.com[.cn]
//
// Any other host, including other AWS services, is rejected with
// ErrBedrockIAMEndpoint. This stops a misconfigured or malicious base_url from
// receiving signed requests.
func ValidBedrockIAMEndpoint(baseURL, region string) error {
	if !ValidBedrockRegion(region) || strings.ContainsAny(baseURL, "?#") {
		return ErrBedrockIAMEndpoint
	}
	u, err := url.Parse(baseURL)
	if err != nil || u.Scheme != "https" || u.User != nil {
		return ErrBedrockIAMEndpoint
	}
	if port := u.Port(); port != "" && port != "443" {
		return ErrBedrockIAMEndpoint
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if host == "" {
		return ErrBedrockIAMEndpoint
	}
	if _, perr := netip.ParseAddr(host); perr == nil {
		return ErrBedrockIAMEndpoint
	}
	switch host {
	case "bedrock-runtime." + region + ".amazonaws.com",
		"bedrock-runtime-fips." + region + ".amazonaws.com",
		"bedrock-runtime." + region + ".amazonaws.com.cn",
		"bedrock-runtime." + region + ".api.aws",
		"bedrock-runtime-fips." + region + ".api.aws":
		return nil
	}
	for _, suffix := range []string{
		".bedrock-runtime." + region + ".vpce.amazonaws.com",
		".bedrock-runtime-fips." + region + ".vpce.amazonaws.com",
		".bedrock-runtime." + region + ".vpce.amazonaws.com.cn",
		".bedrock-runtime-fips." + region + ".vpce.amazonaws.com.cn",
	} {
		if id, ok := strings.CutSuffix(host, suffix); ok && bedrockVPCEndpointRe.MatchString(id) {
			return nil
		}
	}
	return ErrBedrockIAMEndpoint
}

// ValidBedrockBaseURLShape reports whether baseURL, when non-empty, has no
// query string or fragment. The proxy appends "/model/..." to base_url by
// string concatenation, so a query or fragment would swallow the path.
func ValidBedrockBaseURLShape(baseURL string) bool {
	return !strings.ContainsAny(baseURL, "?#")
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
