package provider_test

import (
	"errors"
	"testing"

	"github.com/voidmind-io/voidllm/internal/provider"
)

func TestValidBedrockIAMEndpoint(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		url  string
		ok   bool
	}{
		{"regional", "https://bedrock-runtime.eu-central-1.amazonaws.com", true},
		{"fips", "https://bedrock-runtime-fips.us-east-1.amazonaws.com/", true},
		{"vpce", "https://vpce-0abc.bedrock-runtime.eu-central-1.vpce.amazonaws.com", true},
		{"china", "https://bedrock-runtime.cn-north-1.amazonaws.com.cn", true},
		{"api.aws", "https://bedrock-runtime.us-east-1.api.aws", true},
		{"uppercase host", "https://Bedrock-Runtime.EU-CENTRAL-1.AmazonAWS.com", true},
		{"explicit port", "https://bedrock-runtime.eu-central-1.amazonaws.com:443", true},
		{"http scheme", "http://bedrock-runtime.eu-central-1.amazonaws.com", false},
		{"userinfo", "https://user:pw@bedrock-runtime.eu-central-1.amazonaws.com", false},
		{"lookalike suffix", "https://evilamazonaws.com", false},
		{"lookalike subdomain", "https://amazonaws.com.evil.com", false},
		{"bare apex", "https://amazonaws.com", false},
		{"localhost", "https://localhost", false},
		{"ipv4", "https://127.0.0.1", false},
		{"ipv6", "https://[::1]", false},
		{"other host", "https://example.com", false},
		{"empty", "", false},
		{"garbage", "://bad", false},
		{"no scheme", "bedrock-runtime.eu-central-1.amazonaws.com", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := provider.ValidBedrockIAMEndpoint(tc.url)
			if tc.ok && err != nil {
				t.Errorf("ValidBedrockIAMEndpoint(%q) = %v, want nil", tc.url, err)
			}
			if !tc.ok && !errors.Is(err, provider.ErrBedrockIAMEndpoint) {
				t.Errorf("ValidBedrockIAMEndpoint(%q) = %v, want ErrBedrockIAMEndpoint", tc.url, err)
			}
		})
	}
}
