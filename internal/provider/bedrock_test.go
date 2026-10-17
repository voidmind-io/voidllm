package provider_test

import (
	"strings"
	"testing"

	"github.com/voidmind-io/voidllm/internal/provider"
)

func TestBedrockProviderRegistered(t *testing.T) {
	t.Parallel()
	if provider.Bedrock != "bedrock" {
		t.Errorf("Bedrock = %q", provider.Bedrock)
	}
	if !provider.ValidProviders["bedrock"] {
		t.Error("bedrock missing from ValidProviders")
	}
	found := false
	for _, n := range provider.Names() {
		if n == "bedrock" {
			found = true
		}
	}
	if !found {
		t.Error("bedrock missing from Names()")
	}
}

func TestValidBedrockRegion(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in   string
		want bool
	}{
		{"us-east-1", true},
		{"us-east-2", true},
		{"us-west-2", true},
		{"eu-central-1", true},
		{"eu-west-3", true},
		{"eu-north-1", true},
		{"eu-south-2", true},
		{"ap-southeast-2", true},
		{"ap-northeast-1", true},
		{"ap-southeast-3", true},
		{"ca-central-1", true},
		{"sa-east-1", true},
		{"me-central-1", true},
		{"af-south-1", true},
		{"il-central-1", true},
		{"us-gov-west-1", true},
		{"us-gov-east-1", true},
		{"cn-north-1", true},
		{"cn-northwest-1", true},
		{"us-iso-east-1", true},
		{"us-isob-east-1", true},
		{"eu-central-12", true},
		{"", false},
		{"eu-central-1.evil.com", false},
		{"eu-central-1.", false},
		{".eu-central-1", false},
		{"eu-central-1/x", false},
		{"eu-central-1:443", false},
		{"eu-central-1@evil.com", false},
		{"eu-central-1#x", false},
		{"eu-central-1?x=1", false},
		{"eu-central-1\\x", false},
		{"eu-central-1 ", false},
		{" eu-central-1", false},
		{"eu-central-1\n", false},
		{"eu-central-1\nHost: evil", false},
		{"eu-central-1\x00", false},
		{"EU-CENTRAL-1", false},
		{"eu-Central-1", false},
		{"eu_central_1", false},
		{"eu-central", false},
		{"eu-central-", false},
		{"eu-central-123", false},
		{"central-1", false},
		{"e-central-1", false},
		{"eur-central-1", false},
		{"eu--1", false},
		{"eu-1", false},
		{"1-central-1", false},
		{"eu-central-1-", false},
		{"eu-central-1-1", false},
		{"..", false},
		{"eu-central-１", false},
		{"éu-central-1", false},
		{strings.Repeat("a", 300), false},
	}
	for _, tc := range tests {
		if got := provider.ValidBedrockRegion(tc.in); got != tc.want {
			t.Errorf("ValidBedrockRegion(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestValidBedrockModelID(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   string
		want bool
	}{
		{"base model", "anthropic.claude-3-haiku-20240307-v1:0", true},
		{"eu profile", "eu.anthropic.claude-sonnet-4-5-20250929-v1:0", true},
		{"us profile", "us.anthropic.claude-sonnet-4-5-20250929-v1:0", true},
		{"global profile", "global.anthropic.claude-sonnet-4-5-20250929-v1:0", true},
		{"amazon nova", "amazon.nova-pro-v1:0", true},
		{"meta llama", "meta.llama3-1-70b-instruct-v1:0", true},
		{"context suffix", "anthropic.claude-3-sonnet-20240229-v1:0:28k", true},
		{"inference profile arn", "arn:aws:bedrock:eu-central-1:123456789012:inference-profile/eu.anthropic.claude-sonnet-4-5-20250929-v1:0", true},
		{"application profile arn", "arn:aws:bedrock:us-east-1:123456789012:application-inference-profile/abc123def", true},
		{"foundation model arn", "arn:aws:bedrock:us-east-1::foundation-model/anthropic.claude-v2", true},
		{"provisioned model arn", "arn:aws:bedrock:us-east-1:123456789012:provisioned-model/xyz", true},
		{"single char", "a", true},
		{"digit start", "1abc", true},
		{"max length", strings.Repeat("a", 2048), true},
		{"over max length", strings.Repeat("a", 2049), false},
		{"empty", "", false},
		{"leading dot", ".model", false},
		{"leading hyphen", "-model", false},
		{"leading colon", ":model", false},
		{"leading slash", "/model", false},
		{"space", "a b", false},
		{"trailing space", "a ", false},
		{"tab", "a\tb", false},
		{"newline", "a\nb", false},
		{"nul", "a\x00b", false},
		{"question mark", "a?b=1", false},
		{"hash", "a#b", false},
		{"percent", "a%2Fb", false},
		{"backslash", "a\\b", false},
		{"at sign", "a@b", false},
		{"semicolon", "a;b", false},
		{"quote", `a"b`, false},
		{"angle", "a<b", false},
		{"non ascii", "modèl", false},
		{"emoji", "a😀", false},
		{"unicode digit", "a１", false},
		{"crlf injection", "a\r\nHost: evil", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := provider.ValidBedrockModelID(tc.in); got != tc.want {
				t.Errorf("ValidBedrockModelID(%q) = %v, want %v", truncate(tc.in), got, tc.want)
			}
		})
	}
}

func truncate(s string) string {
	if len(s) > 60 {
		return s[:60] + "..."
	}
	return s
}

func TestBedrockRuntimeBaseURL(t *testing.T) {
	t.Parallel()
	tests := map[string]string{
		"eu-central-1":   "https://bedrock-runtime.eu-central-1.amazonaws.com",
		"us-gov-west-1":  "https://bedrock-runtime.us-gov-west-1.amazonaws.com",
		"ap-southeast-2": "https://bedrock-runtime.ap-southeast-2.amazonaws.com",
	}
	for in, want := range tests {
		if got := provider.BedrockRuntimeBaseURL(in); got != want {
			t.Errorf("BedrockRuntimeBaseURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestEffectiveBaseURL(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		provider string
		baseURL  string
		region   string
		want     string
	}{
		{"bedrock empty base derives", "bedrock", "", "eu-central-1", "https://bedrock-runtime.eu-central-1.amazonaws.com"},
		{"bedrock gov derives", "bedrock", "", "us-gov-west-1", "https://bedrock-runtime.us-gov-west-1.amazonaws.com"},
		{"bedrock override wins", "bedrock", "https://vpce.example.com", "eu-central-1", "https://vpce.example.com"},
		{"bedrock fips override wins", "bedrock", "https://bedrock-runtime-fips.us-east-1.amazonaws.com", "us-east-1", "https://bedrock-runtime-fips.us-east-1.amazonaws.com"},
		{"bedrock override with trailing slash kept", "bedrock", "https://gw.example.com/", "eu-central-1", "https://gw.example.com/"},
		{"bedrock empty region stays empty", "bedrock", "", "", ""},
		{"bedrock injection region stays empty", "bedrock", "", "eu-central-1.evil.com", ""},
		{"bedrock invalid region stays empty", "bedrock", "", "nope", ""},
		{"bedrock uppercase region stays empty", "bedrock", "", "EU-CENTRAL-1", ""},
		{"openai empty stays empty even with region", "openai", "", "eu-central-1", ""},
		{"openai base kept", "openai", "https://api.openai.com/v1", "eu-central-1", "https://api.openai.com/v1"},
		{"anthropic empty stays empty", "anthropic", "", "us-east-1", ""},
		{"custom empty stays empty", "custom", "", "us-east-1", ""},
		{"empty provider stays empty", "", "", "us-east-1", ""},
		{"provider name is case sensitive", "Bedrock", "", "us-east-1", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := provider.EffectiveBaseURL(tc.provider, tc.baseURL, tc.region); got != tc.want {
				t.Errorf("EffectiveBaseURL(%q, %q, %q) = %q, want %q", tc.provider, tc.baseURL, tc.region, got, tc.want)
			}
		})
	}
}

func TestEscapeBedrockModelID(t *testing.T) {
	t.Parallel()
	tests := []struct{ in, want string }{
		{"", ""},
		{"anthropic.claude-v2", "anthropic.claude-v2"},
		{"anthropic.claude-3-haiku-20240307-v1:0", "anthropic.claude-3-haiku-20240307-v1%3A0"},
		{"A-Za-z0-9._~", "A-Za-z0-9._~"},
		{"a/b", "a%2Fb"},
		{"a b", "a%20b"},
		{"a%b", "a%25b"},
		{"a+b", "a%2Bb"},
		{"a?b#c", "a%3Fb%23c"},
		{"ä", "%C3%A4"},
		{"arn:aws:bedrock:us-east-1::foundation-model/x", "arn%3Aaws%3Abedrock%3Aus-east-1%3A%3Afoundation-model%2Fx"},
		{"\x00\xff", "%00%FF"},
	}
	for _, tc := range tests {
		if got := provider.EscapeBedrockModelID(tc.in); got != tc.want {
			t.Errorf("EscapeBedrockModelID(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	// Every valid model ID must escape to something with no raw ':' or '/'.
	for _, id := range []string{"a:b/c", "eu.x-v1:0:28k", "arn:aws:bedrock:eu-central-1:1:inference-profile/eu.x-v1:0"} {
		got := provider.EscapeBedrockModelID(id)
		if strings.ContainsAny(got, ":/") {
			t.Errorf("escaped %q still contains ':' or '/': %q", id, got)
		}
	}
}
