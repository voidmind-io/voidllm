package db

import (
	"context"
	"testing"

	"github.com/voidmind-io/voidllm/internal/config"
)

// TestSyncYAMLModels_SwitchToIAMClearsStoredKey verifies that re-syncing a YAML
// bedrock model and deployment from api_key to iam removes the stored key.
func TestSyncYAMLModels_SwitchToIAMClearsStoredKey(t *testing.T) {
	t.Parallel()
	forEachDialect(t, func(t *testing.T, d *DB) {
		ctx := context.Background()
		key := make([]byte, 32)
		cfgModels := []config.ModelConfig{
			{Name: "single", Provider: "bedrock", Region: "eu-central-1", BedrockModelID: testBedrockModelID, APIKey: "k1"},
			{
				Name: "multi", Provider: "bedrock", Strategy: "priority",
				Deployments: []config.DeploymentConfig{
					{Name: "a", Provider: "bedrock", Region: "eu-central-1", BedrockModelID: testBedrockModelID, APIKey: "k2", Priority: 1},
				},
			},
		}
		if err := d.SyncYAMLModels(ctx, cfgModels, key); err != nil {
			t.Fatalf("initial sync: %v", err)
		}
		single, err := d.GetModelByName(ctx, "single")
		if err != nil {
			t.Fatal(err)
		}
		if single.APIKeyEncrypted == nil {
			t.Fatal("model key not stored after initial sync")
		}

		cfgModels[0].APIKey = ""
		cfgModels[0].BedrockAuth = "iam"
		cfgModels[1].Deployments[0].APIKey = ""
		cfgModels[1].Deployments[0].BedrockAuth = "iam"
		if err := d.SyncYAMLModels(ctx, cfgModels, key); err != nil {
			t.Fatalf("iam sync: %v", err)
		}

		single, _ = d.GetModelByName(ctx, "single")
		if single.BedrockAuth != "iam" || single.APIKeyEncrypted != nil {
			t.Errorf("model auth=%q key=%v, want iam and nil key", single.BedrockAuth, single.APIKeyEncrypted)
		}
		multi, _ := d.GetModelByName(ctx, "multi")
		deps, err := d.ListDeployments(ctx, multi.ID)
		if err != nil || len(deps) != 1 {
			t.Fatalf("ListDeployments: %v %d", err, len(deps))
		}
		if deps[0].BedrockAuth != "iam" || deps[0].APIKeyEncrypted != nil {
			t.Errorf("deployment auth=%q key=%v, want iam and nil key", deps[0].BedrockAuth, deps[0].APIKeyEncrypted)
		}
	})
}
