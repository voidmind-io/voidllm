package router_test

import (
	"testing"

	"github.com/voidmind-io/voidllm/internal/proxy"
	"github.com/voidmind-io/voidllm/internal/router"
)

func TestPick_SingleDeployment_BedrockFields(t *testing.T) {
	t.Parallel()

	r := router.NewRouter(nil, nil, nil)
	got := r.Pick(proxy.Model{
		Name:           "claude",
		Provider:       "bedrock",
		BaseURL:        "https://bedrock-runtime.eu-central-1.amazonaws.com",
		APIKey:         "k",
		Region:         "eu-central-1",
		BedrockModelID: "eu.anthropic.claude-sonnet-4-5-20250929-v1:0",
	})
	if len(got) != 1 {
		t.Fatalf("Pick returned %d candidates, want 1", len(got))
	}
	d := got[0]
	if d.Region != "eu-central-1" || d.BedrockModelID != "eu.anthropic.claude-sonnet-4-5-20250929-v1:0" {
		t.Errorf("synthesized deployment lost bedrock fields: %+v", d)
	}
	if d.Provider != "bedrock" || d.BaseURL == "" {
		t.Errorf("deployment = %+v", d)
	}
}

func TestPick_MultiDeployment_BedrockFieldsPreserved(t *testing.T) {
	t.Parallel()

	r := router.NewRouter(nil, nil, nil)
	got := r.Pick(proxy.Model{
		Name:     "claude",
		Strategy: "priority",
		Deployments: []proxy.Deployment{
			{Name: "a", Provider: "bedrock", BaseURL: "https://a", Region: "eu-central-1", BedrockModelID: "eu.m-v1:0", Priority: 1},
			{Name: "b", Provider: "bedrock", BaseURL: "https://b", Region: "us-east-1", BedrockModelID: "us.m-v1:0", Priority: 2},
		},
	})
	if len(got) != 2 {
		t.Fatalf("Pick returned %d, want 2", len(got))
	}
	if got[0].BedrockModelID != "eu.m-v1:0" || got[0].Region != "eu-central-1" ||
		got[1].BedrockModelID != "us.m-v1:0" || got[1].Region != "us-east-1" {
		t.Errorf("deployments = %+v", got)
	}
}
