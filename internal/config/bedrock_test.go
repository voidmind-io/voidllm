package config_test

import (
	"strings"
	"testing"

	"github.com/voidmind-io/voidllm/internal/config"
)

// bedrockYAML wraps a models: block into an otherwise valid config.
func bedrockYAML(models string) string {
	return minimalValidYAML() + "models:\n" + models
}

func TestValidate_Bedrock(t *testing.T) {
	t.Parallel()

	const goodID = "eu.anthropic.claude-sonnet-4-5-20250929-v1:0"
	tests := []struct {
		name    string
		models  string
		wantErr []string // all substrings must appear; empty means no error
	}{
		{
			name: "valid single model without base_url",
			models: `
  - name: claude
    provider: bedrock
    region: eu-central-1
    bedrock_model_id: ` + goodID + `
`,
		},
		{
			name: "valid with api_key and explicit type chat",
			models: `
  - name: claude
    provider: bedrock
    type: chat
    api_key: abc
    region: us-east-1
    bedrock_model_id: anthropic.claude-3-haiku-20240307-v1:0
`,
		},
		{
			name: "valid with https base_url override",
			models: `
  - name: claude
    provider: bedrock
    base_url: https://vpce-123.bedrock-runtime.eu-central-1.vpce.amazonaws.com
    region: eu-central-1
    bedrock_model_id: ` + goodID + `
`,
		},
		{
			name: "valid with http base_url override",
			models: `
  - name: claude
    provider: bedrock
    base_url: http://localhost:9999
    region: eu-central-1
    bedrock_model_id: ` + goodID + `
`,
		},
		{
			name: "valid gov region and arn model id",
			models: `
  - name: claude
    provider: bedrock
    region: us-gov-west-1
    bedrock_model_id: arn:aws:bedrock:us-gov-west-1:123456789012:inference-profile/us-gov.anthropic.claude-v1:0
`,
		},
		{
			name: "region missing",
			models: `
  - name: claude
    provider: bedrock
    bedrock_model_id: ` + goodID + `
`,
			wantErr: []string{"models[0].region: must not be empty for bedrock provider"},
		},
		{
			name: "region missing even with base_url override",
			models: `
  - name: claude
    provider: bedrock
    base_url: https://gw.example.com
    bedrock_model_id: ` + goodID + `
`,
			wantErr: []string{"models[0].region: must not be empty"},
		},
		{
			name: "region injection rejected",
			models: `
  - name: claude
    provider: bedrock
    region: eu-central-1.evil.com
    bedrock_model_id: ` + goodID + `
`,
			wantErr: []string{"models[0].region: invalid AWS region"},
		},
		{
			name: "region uppercase rejected",
			models: `
  - name: claude
    provider: bedrock
    region: EU-CENTRAL-1
    bedrock_model_id: ` + goodID + `
`,
			wantErr: []string{"models[0].region: invalid AWS region"},
		},
		{
			name: "model id missing",
			models: `
  - name: claude
    provider: bedrock
    region: eu-central-1
`,
			wantErr: []string{"models[0].bedrock_model_id: must not be empty for bedrock provider"},
		},
		{
			name: "model id with space rejected",
			models: `
  - name: claude
    provider: bedrock
    region: eu-central-1
    bedrock_model_id: "bad id"
`,
			wantErr: []string{"models[0].bedrock_model_id: invalid Bedrock model ID"},
		},
		{
			name: "model id path traversal rejected",
			models: `
  - name: claude
    provider: bedrock
    region: eu-central-1
    bedrock_model_id: "../x?y=1"
`,
			wantErr: []string{"models[0].bedrock_model_id: invalid Bedrock model ID"},
		},
		{
			name: "base_url bad scheme",
			models: `
  - name: claude
    provider: bedrock
    base_url: ftp://x
    region: eu-central-1
    bedrock_model_id: ` + goodID + `
`,
			wantErr: []string{"models[0].base_url: must start with http:// or https://"},
		},
		{
			name: "embedding type rejected",
			models: `
  - name: claude
    provider: bedrock
    type: embedding
    region: eu-central-1
    bedrock_model_id: ` + goodID + `
`,
			wantErr: []string{"models[0].type: bedrock supports only chat models"},
		},
		{
			name: "all errors collected",
			models: `
  - name: claude
    provider: bedrock
    type: image
    base_url: nope
`,
			wantErr: []string{"region: must not be empty", "bedrock_model_id: must not be empty", "base_url: must start with", "bedrock supports only chat"},
		},
		{
			name: "non-bedrock still requires base_url",
			models: `
  - name: gpt
    provider: openai
`,
			wantErr: []string{"models[0].base_url: must not be empty"},
		},
		{
			name: "bedrock fields on other provider are ignored",
			models: `
  - name: gpt
    provider: openai
    base_url: https://api.openai.com
    region: not a region!
    bedrock_model_id: "bad id"
`,
		},
		{
			name: "valid deployments next to anthropic",
			models: `
  - name: claude
    provider: bedrock
    strategy: priority
    deployments:
      - name: anthropic
        provider: anthropic
        base_url: https://api.anthropic.com
        priority: 1
      - name: bedrock
        provider: bedrock
        region: eu-central-1
        bedrock_model_id: ` + goodID + `
        priority: 2
`,
		},
		{
			name: "deployment region missing",
			models: `
  - name: claude
    provider: bedrock
    strategy: priority
    deployments:
      - name: bedrock
        provider: bedrock
        bedrock_model_id: ` + goodID + `
`,
			wantErr: []string{"models[0].deployments[0].region: must not be empty for bedrock provider"},
		},
		{
			name: "deployment model id invalid",
			models: `
  - name: claude
    provider: bedrock
    strategy: priority
    deployments:
      - name: bedrock
        provider: bedrock
        region: eu-central-1
        bedrock_model_id: "a b"
`,
			wantErr: []string{"models[0].deployments[0].bedrock_model_id: invalid Bedrock model ID"},
		},
		{
			name: "deployment region injection",
			models: `
  - name: claude
    provider: bedrock
    strategy: priority
    deployments:
      - name: bedrock
        provider: bedrock
        region: "eu-central-1/../x"
        bedrock_model_id: ` + goodID + `
`,
			wantErr: []string{"models[0].deployments[0].region: invalid AWS region"},
		},
		{
			name: "deployment bad base_url",
			models: `
  - name: claude
    provider: bedrock
    strategy: priority
    deployments:
      - name: bedrock
        provider: bedrock
        base_url: gopher://x
        region: eu-central-1
        bedrock_model_id: ` + goodID + `
`,
			wantErr: []string{"models[0].deployments[0].base_url: must start with http:// or https://"},
		},
		{
			name: "deployment bedrock on non-chat model",
			models: `
  - name: emb
    provider: bedrock
    type: embedding
    strategy: priority
    deployments:
      - name: bedrock
        provider: bedrock
        region: eu-central-1
        bedrock_model_id: ` + goodID + `
`,
			wantErr: []string{"models[0].type: bedrock supports only chat models"},
		},
		{
			name: "deployment non-bedrock still requires base_url",
			models: `
  - name: claude
    provider: openai
    strategy: priority
    deployments:
      - name: d
        provider: openai
`,
			wantErr: []string{"models[0].deployments[0].base_url: must not be empty"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := writeTemp(t, "voidllm.yaml", bedrockYAML(tc.models))
			_, _, err := config.Load(path)
			if len(tc.wantErr) == 0 {
				if err != nil {
					t.Fatalf("Load() error = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Load() error = nil, want containing %v", tc.wantErr)
			}
			for _, want := range tc.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not contain %q", err.Error(), want)
				}
			}
		})
	}
}

func TestLoad_BedrockFieldsParsed(t *testing.T) {
	t.Parallel()
	path := writeTemp(t, "voidllm.yaml", bedrockYAML(`
  - name: claude
    provider: bedrock
    region: eu-central-1
    bedrock_model_id: eu.anthropic.claude-sonnet-4-5-20250929-v1:0
  - name: multi
    provider: bedrock
    strategy: priority
    deployments:
      - name: a
        provider: bedrock
        region: us-east-1
        bedrock_model_id: us.model-v1:0
        priority: 1
`))
	cfg, _, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	m := cfg.Models[0]
	if m.Region != "eu-central-1" || m.BedrockModelID != "eu.anthropic.claude-sonnet-4-5-20250929-v1:0" || m.BaseURL != "" {
		t.Errorf("model = %+v", m)
	}
	d := cfg.Models[1].Deployments[0]
	if d.Region != "us-east-1" || d.BedrockModelID != "us.model-v1:0" {
		t.Errorf("deployment = %+v", d)
	}
}

// TestLoad_BedrockEnvInterpolation cannot run in parallel: it sets env vars.
func TestLoad_BedrockEnvInterpolation(t *testing.T) {
	t.Setenv("BEDROCK_TEST_REGION", "ap-southeast-2")
	t.Setenv("BEDROCK_TEST_MODEL", "apac.anthropic.claude-3-haiku-20240307-v1:0")
	path := writeTemp(t, "voidllm.yaml", bedrockYAML(`
  - name: claude
    provider: bedrock
    region: ${BEDROCK_TEST_REGION}
    bedrock_model_id: ${BEDROCK_TEST_MODEL}
`))
	cfg, _, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Models[0].Region != "ap-southeast-2" || cfg.Models[0].BedrockModelID != "apac.anthropic.claude-3-haiku-20240307-v1:0" {
		t.Errorf("model = %+v", cfg.Models[0])
	}
}

func TestValidate_ProviderBedrockAccepted(t *testing.T) {
	t.Parallel()
	path := writeTemp(t, "voidllm.yaml", bedrockYAML(`
  - name: x
    provider: bedrock
    region: eu-west-1
    bedrock_model_id: m-v1
`))
	if _, _, err := config.Load(path); err != nil {
		t.Fatalf("bedrock must be a valid provider name: %v", err)
	}
}
