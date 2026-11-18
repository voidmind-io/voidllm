package db

import (
	"context"
	"testing"

	"github.com/voidmind-io/voidllm/internal/config"
)

const testRoleARN = "arn:aws:iam::123456789012:role/voidllm"

func strp(s string) *string { return &s }

func TestMigration0021_ColumnsAndDefaults(t *testing.T) {
	t.Parallel()
	forEachDialect(t, func(t *testing.T, d *DB) {
		ctx := context.Background()
		for _, c := range []struct{ table, col string }{
			{"models", "bedrock_auth"}, {"models", "bedrock_role_arn"},
			{"model_deployments", "bedrock_auth"}, {"model_deployments", "bedrock_role_arn"},
		} {
			if !columnExists(t, d, c.table, c.col) {
				t.Errorf("column %s.%s missing after migrations", c.table, c.col)
			}
		}
		// Legacy-style create (no auth fields) defaults to api_key / empty role.
		m, err := d.CreateModel(ctx, CreateModelParams{Name: "legacy", Provider: "bedrock", Region: "eu-central-1", BedrockModelID: testBedrockModelID, Source: "api"})
		if err != nil {
			t.Fatal(err)
		}
		if m.BedrockAuth != "api_key" || m.BedrockRoleARN != "" {
			t.Errorf("default model auth = %q role %q", m.BedrockAuth, m.BedrockRoleARN)
		}
		dep, err := d.CreateDeployment(ctx, CreateDeploymentParams{ModelID: m.ID, Name: "d", Provider: "bedrock", Region: "eu-central-1", BedrockModelID: testBedrockModelID, Weight: 1})
		if err != nil {
			t.Fatal(err)
		}
		if dep.BedrockAuth != "api_key" || dep.BedrockRoleARN != "" {
			t.Errorf("default deployment auth = %q role %q", dep.BedrockAuth, dep.BedrockRoleARN)
		}
	})
}

// TestMigration0021_DownUpBackfill removes the 0021 columns with the real down
// migration and re-applies the up migration; pre-existing rows must backfill
// to api_key / empty role and keep their other data. Store queries select the
// new columns, so only raw SQL is used while they are absent.
func TestMigration0021_DownUpBackfill(t *testing.T) {
	t.Parallel()
	forEachDialect(t, func(t *testing.T, d *DB) {
		ctx := context.Background()
		enc := "ciphertext"
		m, err := d.CreateModel(ctx, CreateModelParams{
			Name: "keep", Provider: "bedrock", Region: "eu-central-1", BedrockModelID: testBedrockModelID,
			Source: "api", Aliases: "k1", APIKeyEncrypted: &enc, BedrockAuth: "iam", BedrockRoleARN: testRoleARN,
		})
		if err != nil {
			t.Fatal(err)
		}
		dep, err := d.CreateDeployment(ctx, CreateDeploymentParams{
			ModelID: m.ID, Name: "d", Provider: "bedrock", Region: "eu-central-1", BedrockModelID: testBedrockModelID,
			Weight: 2, BedrockAuth: "iam", BedrockRoleARN: testRoleARN,
		})
		if err != nil {
			t.Fatal(err)
		}

		execInTx(t, d, readMigration0020(t, "0021_bedrock_auth.down.sql"))
		for _, c := range []struct{ table, col string }{
			{"models", "bedrock_auth"}, {"models", "bedrock_role_arn"},
			{"model_deployments", "bedrock_auth"}, {"model_deployments", "bedrock_role_arn"},
		} {
			if columnExists(t, d, c.table, c.col) {
				t.Fatalf("column %s.%s still exists after down", c.table, c.col)
			}
		}
		if !columnExists(t, d, "models", "bedrock_model_id") || !columnExists(t, d, "model_deployments", "region") {
			t.Fatal("0021 down must not drop 0020 columns")
		}

		execInTx(t, d, readMigration0020(t, "0021_bedrock_auth.up.sql"))
		if got := rawColumnValue(t, d, "models", "bedrock_auth", m.ID); got != "api_key" {
			t.Errorf("backfilled models.bedrock_auth = %q", got)
		}
		if got := rawColumnValue(t, d, "models", "bedrock_role_arn", m.ID); got != "" {
			t.Errorf("backfilled models.bedrock_role_arn = %q", got)
		}
		if got := rawColumnValue(t, d, "model_deployments", "bedrock_auth", dep.ID); got != "api_key" {
			t.Errorf("backfilled deployments.bedrock_auth = %q", got)
		}
		if got := rawColumnValue(t, d, "model_deployments", "bedrock_role_arn", dep.ID); got != "" {
			t.Errorf("backfilled deployments.bedrock_role_arn = %q", got)
		}

		got, err := d.GetModel(ctx, m.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Aliases != "k1" || got.APIKeyEncrypted == nil || *got.APIKeyEncrypted != enc || got.Region != "eu-central-1" {
			t.Errorf("other data damaged: %+v", got)
		}
		gd, err := d.GetDeployment(ctx, dep.ID)
		if err != nil {
			t.Fatal(err)
		}
		if gd.Weight != 2 || gd.BedrockAuth != "api_key" {
			t.Errorf("deployment after down/up = %+v", gd)
		}
	})
}

func TestBedrockAuth_ModelRoundTripAndUpdate(t *testing.T) {
	t.Parallel()
	forEachDialect(t, func(t *testing.T, d *DB) {
		ctx := context.Background()
		enc := "ciphertext"
		m, err := d.CreateModel(ctx, CreateModelParams{
			Name: "claude", Provider: "bedrock", Region: "eu-central-1", BedrockModelID: testBedrockModelID,
			Source: "api", BedrockAuth: "iam", BedrockRoleARN: testRoleARN,
		})
		if err != nil {
			t.Fatal(err)
		}
		if m.BedrockAuth != "iam" || m.BedrockRoleARN != testRoleARN || m.APIKeyEncrypted != nil {
			t.Errorf("created = %q %q key=%v", m.BedrockAuth, m.BedrockRoleARN, m.APIKeyEncrypted)
		}
		byName, err := d.GetModelByName(ctx, "claude")
		if err != nil {
			t.Fatal(err)
		}
		if byName.BedrockAuth != "iam" || byName.BedrockRoleARN != testRoleARN {
			t.Errorf("GetModelByName = %q %q", byName.BedrockAuth, byName.BedrockRoleARN)
		}
		list, err := d.ListModels(ctx, "", 50, true)
		if err != nil {
			t.Fatal(err)
		}
		if len(list) != 1 || list[0].BedrockAuth != "iam" || list[0].BedrockRoleARN != testRoleARN {
			t.Errorf("ListModels = %+v", list)
		}

		// nil pointers leave both untouched.
		to := "30s"
		got, err := d.UpdateModel(ctx, m.ID, UpdateModelParams{Timeout: &to})
		if err != nil {
			t.Fatal(err)
		}
		if got.BedrockAuth != "iam" || got.BedrockRoleARN != testRoleARN {
			t.Errorf("nil update changed auth: %q %q", got.BedrockAuth, got.BedrockRoleARN)
		}

		// iam -> api_key with key, role cleared.
		got, err = d.UpdateModel(ctx, m.ID, UpdateModelParams{BedrockAuth: strp("api_key"), BedrockRoleARN: strp(""), APIKeyEncrypted: &enc})
		if err != nil {
			t.Fatal(err)
		}
		if got.BedrockAuth != "api_key" || got.BedrockRoleARN != "" || got.APIKeyEncrypted == nil || *got.APIKeyEncrypted != enc {
			t.Errorf("after switch to api_key = %q %q key=%v", got.BedrockAuth, got.BedrockRoleARN, got.APIKeyEncrypted)
		}

		// ClearAPIKey sets NULL, and wins over a supplied APIKeyEncrypted.
		other := "other"
		got, err = d.UpdateModel(ctx, m.ID, UpdateModelParams{BedrockAuth: strp("iam"), ClearAPIKey: true, APIKeyEncrypted: &other})
		if err != nil {
			t.Fatal(err)
		}
		if got.APIKeyEncrypted != nil || got.BedrockAuth != "iam" {
			t.Errorf("after ClearAPIKey = auth %q key=%v", got.BedrockAuth, got.APIKeyEncrypted)
		}
		var isNull bool
		if err := d.sql.QueryRowContext(ctx, "SELECT api_key_encrypted IS NULL FROM models WHERE id = "+d.dialect.Placeholder(1), m.ID).Scan(&isNull); err != nil {
			t.Fatal(err)
		}
		if !isNull {
			t.Error("api_key_encrypted is not NULL in the table")
		}

		// ClearAPIKey alone on a row with no changes elsewhere still works (set clause present).
		if _, err := d.UpdateModel(ctx, m.ID, UpdateModelParams{APIKeyEncrypted: &enc}); err != nil {
			t.Fatal(err)
		}
		got, err = d.UpdateModel(ctx, m.ID, UpdateModelParams{ClearAPIKey: true})
		if err != nil {
			t.Fatalf("ClearAPIKey only: %v", err)
		}
		if got.APIKeyEncrypted != nil {
			t.Errorf("ClearAPIKey only left key = %v", got.APIKeyEncrypted)
		}

		// Non-bedrock rows default.
		plain := mustCreateModel(t, d, "plain")
		if plain.BedrockAuth != "api_key" || plain.BedrockRoleARN != "" {
			t.Errorf("plain model auth = %q role %q", plain.BedrockAuth, plain.BedrockRoleARN)
		}
	})
}

func TestBedrockAuth_DeploymentRoundTripAndUpdate(t *testing.T) {
	t.Parallel()
	forEachDialect(t, func(t *testing.T, d *DB) {
		ctx := context.Background()
		m := mustCreateModel(t, d, "multi")
		enc := "ciphertext"
		dep, err := d.CreateDeployment(ctx, CreateDeploymentParams{
			ModelID: m.ID, Name: "b", Provider: "bedrock", Region: "eu-central-1", BedrockModelID: testBedrockModelID,
			Weight: 1, BedrockAuth: "iam", BedrockRoleARN: testRoleARN,
		})
		if err != nil {
			t.Fatal(err)
		}
		if dep.BedrockAuth != "iam" || dep.BedrockRoleARN != testRoleARN || dep.APIKeyEncrypted != nil {
			t.Errorf("created = %+v", dep)
		}
		for label, get := range map[string]func() ([]Deployment, error){
			"ListDeployments":       func() ([]Deployment, error) { return d.ListDeployments(ctx, m.ID) },
			"ListActiveDeployments": func() ([]Deployment, error) { return d.ListActiveDeployments(ctx, m.ID) },
		} {
			deps, err := get()
			if err != nil {
				t.Fatal(err)
			}
			if len(deps) != 1 || deps[0].BedrockAuth != "iam" || deps[0].BedrockRoleARN != testRoleARN {
				t.Errorf("%s = %+v", label, deps)
			}
		}
		byModel, err := d.ListDeploymentsByModelIDs(ctx, []string{m.ID})
		if err != nil {
			t.Fatal(err)
		}
		if got := byModel[m.ID]; len(got) != 1 || got[0].BedrockAuth != "iam" {
			t.Errorf("ListDeploymentsByModelIDs = %+v", got)
		}

		w := 5
		got, err := d.UpdateDeployment(ctx, dep.ID, UpdateDeploymentParams{Weight: &w})
		if err != nil {
			t.Fatal(err)
		}
		if got.BedrockAuth != "iam" || got.BedrockRoleARN != testRoleARN {
			t.Errorf("nil update changed auth: %+v", got)
		}

		got, err = d.UpdateDeployment(ctx, dep.ID, UpdateDeploymentParams{BedrockAuth: strp("api_key"), BedrockRoleARN: strp(""), APIKeyEncrypted: &enc})
		if err != nil {
			t.Fatal(err)
		}
		if got.BedrockAuth != "api_key" || got.BedrockRoleARN != "" || got.APIKeyEncrypted == nil {
			t.Errorf("after switch = %+v", got)
		}

		got, err = d.UpdateDeployment(ctx, dep.ID, UpdateDeploymentParams{BedrockAuth: strp("iam"), ClearAPIKey: true})
		if err != nil {
			t.Fatal(err)
		}
		if got.APIKeyEncrypted != nil || got.BedrockAuth != "iam" {
			t.Errorf("after ClearAPIKey = %+v", got)
		}
		if _, err := d.UpdateDeployment(ctx, dep.ID, UpdateDeploymentParams{APIKeyEncrypted: &enc}); err != nil {
			t.Fatal(err)
		}
		got, err = d.UpdateDeployment(ctx, dep.ID, UpdateDeploymentParams{ClearAPIKey: true})
		if err != nil {
			t.Fatalf("ClearAPIKey only: %v", err)
		}
		if got.APIKeyEncrypted != nil {
			t.Errorf("ClearAPIKey only left key")
		}
	})
}

func TestBedrockAuth_SyncYAMLModels(t *testing.T) {
	t.Parallel()
	forEachDialect(t, func(t *testing.T, d *DB) {
		ctx := context.Background()
		key := make([]byte, 32)
		cfgModels := []config.ModelConfig{
			{Name: "claude", Provider: "bedrock", Region: "eu-central-1", BedrockModelID: testBedrockModelID, BedrockAuth: "iam", BedrockRoleARN: testRoleARN},
			{
				Name: "multi", Provider: "bedrock", Strategy: "priority",
				Deployments: []config.DeploymentConfig{
					{Name: "a", Provider: "bedrock", Region: "eu-central-1", BedrockModelID: testBedrockModelID, BedrockAuth: "iam", BedrockRoleARN: testRoleARN, Priority: 1},
					{Name: "b", Provider: "bedrock", Region: "eu-central-1", BedrockModelID: testBedrockModelID, APIKey: "k", Priority: 2},
				},
			},
		}
		if err := d.SyncYAMLModels(ctx, cfgModels, key); err != nil {
			t.Fatalf("create: %v", err)
		}
		m, err := d.GetModelByName(ctx, "claude")
		if err != nil {
			t.Fatal(err)
		}
		if m.BedrockAuth != "iam" || m.BedrockRoleARN != testRoleARN || m.APIKeyEncrypted != nil {
			t.Errorf("created yaml model = %q %q key=%v", m.BedrockAuth, m.BedrockRoleARN, m.APIKeyEncrypted)
		}
		multi, _ := d.GetModelByName(ctx, "multi")
		deps, err := d.ListDeployments(ctx, multi.ID)
		if err != nil {
			t.Fatal(err)
		}
		by := map[string]Deployment{}
		for _, x := range deps {
			by[x.Name] = x
		}
		if by["a"].BedrockAuth != "iam" || by["a"].BedrockRoleARN != testRoleARN || by["b"].BedrockAuth != "api_key" {
			t.Errorf("yaml deployments = %+v", deps)
		}

		// Re-sync: role change and mode switch are applied.
		cfgModels[0].BedrockRoleARN = ""
		cfgModels[0].BedrockAuth = ""
		cfgModels[0].APIKey = "now-key"
		cfgModels[1].Deployments[0].BedrockRoleARN = "arn:aws:iam::123456789012:role/other"
		if err := d.SyncYAMLModels(ctx, cfgModels, key); err != nil {
			t.Fatalf("update: %v", err)
		}
		m, _ = d.GetModelByName(ctx, "claude")
		if m.BedrockAuth != "api_key" || m.BedrockRoleARN != "" || m.APIKeyEncrypted == nil {
			t.Errorf("updated yaml model = %q %q", m.BedrockAuth, m.BedrockRoleARN)
		}
		deps, _ = d.ListDeployments(ctx, multi.ID)
		for _, x := range deps {
			if x.Name == "a" && x.BedrockRoleARN != "arn:aws:iam::123456789012:role/other" {
				t.Errorf("deployment a role = %q", x.BedrockRoleARN)
			}
		}
	})
}
