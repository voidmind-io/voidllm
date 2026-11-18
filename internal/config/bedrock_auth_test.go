package config_test

import (
	"strings"
	"testing"

	"github.com/voidmind-io/voidllm/internal/config"
)

func TestValidate_BedrockAuth(t *testing.T) {
	t.Parallel()

	const id = "eu.anthropic.claude-sonnet-4-5-20250929-v1:0"
	const role = "arn:aws:iam::123456789012:role/voidllm"

	// model builds a single-model bedrock entry from extra field lines.
	model := func(extra string) string {
		return "\n  - name: claude\n    provider: bedrock\n    region: eu-central-1\n    bedrock_model_id: " + id + "\n" + extra
	}
	// dep builds a bedrock deployment from extra field lines.
	dep := func(extra string) string {
		return "\n  - name: claude\n    provider: bedrock\n    strategy: priority\n    deployments:\n" +
			"      - name: d\n        provider: bedrock\n        region: eu-central-1\n        bedrock_model_id: " + id + "\n        priority: 1\n" + extra
	}

	tests := []struct {
		name    string
		models  string
		wantErr []string
	}{
		// models
		{name: "model api_key mode with key", models: model("    bedrock_auth: api_key\n    api_key: k\n")},
		{name: "model empty auth with key", models: model("    api_key: k\n")},
		{name: "model api_key mode without key", models: model("    bedrock_auth: api_key\n"),
			wantErr: []string{"models[0]", "api_key is required for bedrock when bedrock_auth is api_key"}},
		{name: "model empty auth without key", models: model(""),
			wantErr: []string{"api_key is required for bedrock"}},
		{name: "model iam without key", models: model("    bedrock_auth: iam\n")},
		{name: "model iam with role", models: model("    bedrock_auth: iam\n    bedrock_role_arn: " + role + "\n")},
		{name: "model iam govcloud role", models: model("    bedrock_auth: iam\n    bedrock_role_arn: arn:aws-us-gov:iam::123456789012:role/path/x\n")},
		{name: "model iam with key", models: model("    bedrock_auth: iam\n    api_key: k\n"),
			wantErr: []string{"models[0]", "api_key must be empty when bedrock_auth is iam"}},
		{name: "model iam bad role arn", models: model("    bedrock_auth: iam\n    bedrock_role_arn: not-an-arn\n"),
			wantErr: []string{"bedrock_role_arn is invalid"}},
		{name: "model iam user arn instead of role", models: model("    bedrock_auth: iam\n    bedrock_role_arn: arn:aws:iam::123456789012:user/bob\n"),
			wantErr: []string{"bedrock_role_arn is invalid"}},
		{name: "model role arn in api_key mode", models: model("    api_key: k\n    bedrock_role_arn: " + role + "\n"),
			wantErr: []string{"bedrock_role_arn requires bedrock_auth iam"}},
		{name: "model unknown auth", models: model("    bedrock_auth: sso\n    api_key: k\n"),
			wantErr: []string{"bedrock_auth must be api_key or iam"}},
		{name: "model auth case sensitive", models: model("    bedrock_auth: IAM\n"),
			wantErr: []string{"bedrock_auth must be api_key or iam"}},
		{name: "non-bedrock provider with bedrock_auth", models: "\n  - name: x\n    provider: openai\n    base_url: https://api.openai.com\n    bedrock_auth: iam\n",
			wantErr: []string{"models[0].bedrock_auth/bedrock_role_arn: only valid for bedrock provider"}},
		{name: "non-bedrock provider with role arn", models: "\n  - name: x\n    provider: openai\n    base_url: https://api.openai.com\n    bedrock_role_arn: " + role + "\n",
			wantErr: []string{"only valid for bedrock provider"}},

		// deployments
		{name: "deployment api_key mode with key", models: dep("        api_key: k\n")},
		{name: "deployment api_key mode without key", models: dep("        bedrock_auth: api_key\n"),
			wantErr: []string{"models[0].deployments[0]", "api_key is required for bedrock"}},
		{name: "deployment iam without key", models: dep("        bedrock_auth: iam\n")},
		{name: "deployment iam with role", models: dep("        bedrock_auth: iam\n        bedrock_role_arn: " + role + "\n")},
		{name: "deployment iam with key", models: dep("        bedrock_auth: iam\n        api_key: k\n"),
			wantErr: []string{"models[0].deployments[0]", "api_key must be empty when bedrock_auth is iam"}},
		{name: "deployment bad role arn", models: dep("        bedrock_auth: iam\n        bedrock_role_arn: arn:aws:iam::12:role/x\n"),
			wantErr: []string{"bedrock_role_arn is invalid"}},
		{name: "deployment role arn in api_key mode", models: dep("        api_key: k\n        bedrock_role_arn: " + role + "\n"),
			wantErr: []string{"bedrock_role_arn requires bedrock_auth iam"}},
		{name: "deployment unknown auth", models: dep("        bedrock_auth: kerberos\n"),
			wantErr: []string{"bedrock_auth must be api_key or iam"}},
		{name: "non-bedrock deployment with bedrock_auth",
			models:  "\n  - name: x\n    provider: openai\n    strategy: priority\n    deployments:\n      - name: d\n        provider: openai\n        base_url: https://api.openai.com\n        bedrock_auth: iam\n        priority: 1\n",
			wantErr: []string{"models[0].deployments[0].bedrock_auth/bedrock_role_arn: only valid for bedrock provider"}},
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

func TestLoad_BedrockAuthDefaultsAndParsing(t *testing.T) {
	t.Parallel()
	path := writeTemp(t, "voidllm.yaml", bedrockYAML(`
  - name: legacy
    provider: bedrock
    region: eu-central-1
    api_key: k
    bedrock_model_id: m-v1
  - name: iam
    provider: bedrock
    region: eu-central-1
    bedrock_auth: iam
    bedrock_role_arn: arn:aws:iam::123456789012:role/r
    bedrock_model_id: m-v1
  - name: multi
    provider: bedrock
    strategy: priority
    deployments:
      - name: a
        provider: bedrock
        region: us-east-1
        api_key: k
        bedrock_model_id: m-v1
        priority: 1
      - name: b
        provider: bedrock
        region: us-east-1
        bedrock_auth: iam
        bedrock_model_id: m-v1
        priority: 2
  - name: other
    provider: openai
    base_url: https://api.openai.com
`))
	cfg, _, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	m := cfg.Models
	if m[0].BedrockAuth != "api_key" {
		t.Errorf("empty auth normalized to %q, want api_key", m[0].BedrockAuth)
	}
	if m[1].BedrockAuth != "iam" || m[1].BedrockRoleARN != "arn:aws:iam::123456789012:role/r" {
		t.Errorf("iam model = %q / %q", m[1].BedrockAuth, m[1].BedrockRoleARN)
	}
	if m[2].Deployments[0].BedrockAuth != "api_key" || m[2].Deployments[1].BedrockAuth != "iam" {
		t.Errorf("deployment auth = %q / %q", m[2].Deployments[0].BedrockAuth, m[2].Deployments[1].BedrockAuth)
	}
	if m[3].BedrockAuth != "" {
		t.Errorf("non-bedrock auth = %q, want empty", m[3].BedrockAuth)
	}
}
