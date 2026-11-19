package provider_test

import (
	"errors"
	"testing"

	"github.com/voidmind-io/voidllm/internal/provider"
)

func TestValidBedrockIAMEndpoint(t *testing.T) {
	t.Parallel()
	const r = "eu-central-1"
	tests := []struct {
		name   string
		url    string
		region string
		ok     bool
	}{
		// allowed forms
		{"regional", "https://bedrock-runtime.eu-central-1.amazonaws.com", r, true},
		{"regional with path and slash", "https://bedrock-runtime.eu-central-1.amazonaws.com/", r, true},
		{"regional with model path", "https://bedrock-runtime.eu-central-1.amazonaws.com/model/x/converse", r, true},
		{"fips", "https://bedrock-runtime-fips.eu-central-1.amazonaws.com", r, true},
		{"china", "https://bedrock-runtime.cn-north-1.amazonaws.com.cn", "cn-north-1", true},
		{"dual-stack", "https://bedrock-runtime.eu-central-1.api.aws", r, true},
		{"dual-stack fips", "https://bedrock-runtime-fips.eu-central-1.api.aws", r, true},
		{"vpce regional", "https://vpce-0abc123.bedrock-runtime.eu-central-1.vpce.amazonaws.com", r, true},
		{"vpce fips", "https://vpce-0abc123.bedrock-runtime-fips.eu-central-1.vpce.amazonaws.com", r, true},
		{"vpce az specific", "https://vpce-0abc123-xyz9-eu-central-1a.bedrock-runtime.eu-central-1.vpce.amazonaws.com", r, true},
		{"vpce china", "https://vpce-0abc123.bedrock-runtime.cn-north-1.vpce.amazonaws.com.cn", "cn-north-1", true},
		{"uppercase", "https://Bedrock-Runtime.EU-CENTRAL-1.AmazonAWS.com", r, true},
		{"trailing dot", "https://bedrock-runtime.eu-central-1.amazonaws.com.", r, true},
		{"explicit 443", "https://bedrock-runtime.eu-central-1.amazonaws.com:443", r, true},
		{"no port", "https://bedrock-runtime.eu-central-1.amazonaws.com/model/x", r, true},
		// rejected
		{"bare port delimiter", "https://bedrock-runtime.eu-central-1.amazonaws.com:", r, false},
		{"bare port delimiter with path", "https://bedrock-runtime.eu-central-1.amazonaws.com:/model/x", r, false},
		{"region mismatch", "https://bedrock-runtime.us-east-1.amazonaws.com", r, false},
		{"vpce region mismatch", "https://vpce-0abc.bedrock-runtime.us-east-1.vpce.amazonaws.com", r, false},
		{"invalid region argument", "https://bedrock-runtime.eu-central-1.amazonaws.com", "", false},
		{"execute-api", "https://xyz.execute-api.eu-central-1.amazonaws.com", r, false},
		{"s3 bucket", "https://my-bedrock-bucket.s3.amazonaws.com", r, false},
		{"elb", "https://alb-1.eu-central-1.elb.amazonaws.com", r, false},
		{"ec2 compute", "https://ec2-1-2-3-4.compute-1.amazonaws.com", r, false},
		{"bedrock control plane", "https://bedrock.eu-central-1.amazonaws.com", r, false},
		{"bedrock-mantle", "https://bedrock-mantle.eu-central-1.api.aws", r, false},
		{"bedrock agent runtime", "https://bedrock-agent-runtime.eu-central-1.amazonaws.com", r, false},
		{"suffix attack", "https://bedrock-runtime.eu-central-1.amazonaws.com.evil.com", r, false},
		{"prefix attack", "https://evil.bedrock-runtime.eu-central-1.amazonaws.com", r, false},
		{"vpce bad id", "https://vpce-.bedrock-runtime.eu-central-1.vpce.amazonaws.com", r, false},
		{"vpce uppercase-free but underscore", "https://vpce-a_b.bedrock-runtime.eu-central-1.vpce.amazonaws.com", r, false},
		{"vpce without id", "https://bedrock-runtime.eu-central-1.vpce.amazonaws.com", r, false},
		{"other port", "https://bedrock-runtime.eu-central-1.amazonaws.com:8443", r, false},
		{"query", "https://bedrock-runtime.eu-central-1.amazonaws.com?x=1", r, false},
		{"query after path", "https://bedrock-runtime.eu-central-1.amazonaws.com/?x=1", r, false},
		{"fragment", "https://bedrock-runtime.eu-central-1.amazonaws.com#f", r, false},
		{"http", "http://bedrock-runtime.eu-central-1.amazonaws.com", r, false},
		{"userinfo", "https://user:pw@bedrock-runtime.eu-central-1.amazonaws.com", r, false},
		{"ipv4", "https://127.0.0.1", r, false},
		{"ipv6", "https://[::1]", r, false},
		{"localhost", "https://localhost", r, false},
		{"other host", "https://example.com", r, false},
		{"empty", "", r, false},
		{"garbage", "://bad", r, false},
		{"no scheme", "bedrock-runtime.eu-central-1.amazonaws.com", r, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := provider.ValidBedrockIAMEndpoint(tc.url, tc.region)
			if tc.ok && err != nil {
				t.Errorf("ValidBedrockIAMEndpoint(%q, %q) = %v, want nil", tc.url, tc.region, err)
			}
			if !tc.ok && !errors.Is(err, provider.ErrBedrockIAMEndpoint) {
				t.Errorf("ValidBedrockIAMEndpoint(%q, %q) = %v, want ErrBedrockIAMEndpoint", tc.url, tc.region, err)
			}
		})
	}
}

func TestValidBedrockBaseURLShape(t *testing.T) {
	t.Parallel()
	for url, want := range map[string]bool{
		"":                                 true,
		"https://h.example":                true,
		"https://h.example/prefix":         true,
		"https://h.example?x=1":            false,
		"https://h.example/p?x=1":          false,
		"https://h.example#frag":           false,
		"http://localhost:9999/p?x=1#frag": false,
	} {
		if got := provider.ValidBedrockBaseURLShape(url); got != want {
			t.Errorf("ValidBedrockBaseURLShape(%q) = %v, want %v", url, got, want)
		}
	}
}
