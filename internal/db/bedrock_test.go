package db

import (
	"context"
	"os"
	"testing"

	"github.com/google/uuid"

	"github.com/voidmind-io/voidllm/internal/config"
)

const (
	testBedrockModelID  = "eu.anthropic.claude-sonnet-4-5-20250929-v1:0"
	testBedrockModelID2 = "us.anthropic.claude-sonnet-4-5-20250929-v1:0"
)

func TestBedrockFields_ModelRoundTrip(t *testing.T) {
	t.Parallel()
	forEachDialect(t, func(t *testing.T, d *DB) {
		ctx := context.Background()

		created, err := d.CreateModel(ctx, CreateModelParams{
			Name: "claude", Provider: "bedrock", BaseURL: "", Region: "eu-central-1", BedrockModelID: testBedrockModelID, Source: "api",
		})
		if err != nil {
			t.Fatalf("CreateModel: %v", err)
		}
		if created.Region != "eu-central-1" || created.BedrockModelID != testBedrockModelID || created.BaseURL != "" {
			t.Errorf("created = region %q model %q base %q", created.Region, created.BedrockModelID, created.BaseURL)
		}

		byID, err := d.GetModel(ctx, created.ID)
		if err != nil {
			t.Fatalf("GetModel: %v", err)
		}
		byName, err := d.GetModelByName(ctx, "claude")
		if err != nil {
			t.Fatalf("GetModelByName: %v", err)
		}
		for label, m := range map[string]*Model{"GetModel": byID, "GetModelByName": byName} {
			if m.Region != "eu-central-1" || m.BedrockModelID != testBedrockModelID {
				t.Errorf("%s = region %q model %q", label, m.Region, m.BedrockModelID)
			}
		}

		list, err := d.ListModels(ctx, "", 50, true)
		if err != nil {
			t.Fatalf("ListModels: %v", err)
		}
		var found bool
		for _, m := range list {
			if m.ID == created.ID {
				found = true
				if m.Region != "eu-central-1" || m.BedrockModelID != testBedrockModelID {
					t.Errorf("ListModels entry = region %q model %q", m.Region, m.BedrockModelID)
				}
			}
		}
		if !found {
			t.Error("model missing from ListModels")
		}

		plain := mustCreateModel(t, d, "plain-openai")
		if plain.Region != "" || plain.BedrockModelID != "" {
			t.Errorf("non-bedrock model has region %q model %q, want empty", plain.Region, plain.BedrockModelID)
		}
	})
}

func TestBedrockFields_UpdateModel(t *testing.T) {
	t.Parallel()
	forEachDialect(t, func(t *testing.T, d *DB) {
		ctx := context.Background()
		m, err := d.CreateModel(ctx, CreateModelParams{
			Name: "claude", Provider: "bedrock", Region: "eu-central-1", BedrockModelID: testBedrockModelID, Source: "api",
		})
		if err != nil {
			t.Fatal(err)
		}

		// nil leaves both untouched while another field changes.
		timeout := "30s"
		got, err := d.UpdateModel(ctx, m.ID, UpdateModelParams{Timeout: &timeout})
		if err != nil {
			t.Fatalf("UpdateModel: %v", err)
		}
		if got.Region != "eu-central-1" || got.BedrockModelID != testBedrockModelID {
			t.Errorf("nil update changed fields: %q %q", got.Region, got.BedrockModelID)
		}

		// Only region.
		r := "us-east-1"
		got, err = d.UpdateModel(ctx, m.ID, UpdateModelParams{Region: &r})
		if err != nil {
			t.Fatal(err)
		}
		if got.Region != "us-east-1" || got.BedrockModelID != testBedrockModelID {
			t.Errorf("region update = %q %q", got.Region, got.BedrockModelID)
		}

		// Only model ID.
		id := testBedrockModelID2
		got, err = d.UpdateModel(ctx, m.ID, UpdateModelParams{BedrockModelID: &id})
		if err != nil {
			t.Fatal(err)
		}
		if got.Region != "us-east-1" || got.BedrockModelID != testBedrockModelID2 {
			t.Errorf("model id update = %q %q", got.Region, got.BedrockModelID)
		}

		// Empty string clears.
		empty := ""
		got, err = d.UpdateModel(ctx, m.ID, UpdateModelParams{Region: &empty, BedrockModelID: &empty})
		if err != nil {
			t.Fatal(err)
		}
		if got.Region != "" || got.BedrockModelID != "" {
			t.Errorf("clear = %q %q", got.Region, got.BedrockModelID)
		}

		// Persisted, not just returned.
		fetched, err := d.GetModel(ctx, m.ID)
		if err != nil {
			t.Fatal(err)
		}
		if fetched.Region != "" || fetched.BedrockModelID != "" {
			t.Errorf("persisted = %q %q", fetched.Region, fetched.BedrockModelID)
		}
	})
}

func TestBedrockFields_DeploymentRoundTripAndUpdate(t *testing.T) {
	t.Parallel()
	forEachDialect(t, func(t *testing.T, d *DB) {
		ctx := context.Background()
		model := mustCreateModel(t, d, "multi")

		dep := mustCreateDeployment(t, d, CreateDeploymentParams{
			ModelID: model.ID, Name: "bedrock-eu", Provider: "bedrock", BaseURL: "",
			Region: "eu-central-1", BedrockModelID: testBedrockModelID, Weight: 1, Priority: 1,
		})
		other := mustCreateDeployment(t, d, CreateDeploymentParams{
			ModelID: model.ID, Name: "openai", Provider: "openai", BaseURL: "https://api.openai.com/v1", Weight: 1, Priority: 2,
		})
		if dep.Region != "eu-central-1" || dep.BedrockModelID != testBedrockModelID || dep.BaseURL != "" {
			t.Errorf("created = %+v", dep)
		}
		if other.Region != "" || other.BedrockModelID != "" {
			t.Errorf("non-bedrock deployment has bedrock fields: %+v", other)
		}

		got, err := d.GetDeployment(ctx, dep.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Region != "eu-central-1" || got.BedrockModelID != testBedrockModelID {
			t.Errorf("GetDeployment = %q %q", got.Region, got.BedrockModelID)
		}

		list, err := d.ListDeployments(ctx, model.ID)
		if err != nil {
			t.Fatal(err)
		}
		byName := map[string]Deployment{}
		for _, x := range list {
			byName[x.Name] = x
		}
		if byName["bedrock-eu"].BedrockModelID != testBedrockModelID || byName["openai"].BedrockModelID != "" {
			t.Errorf("ListDeployments = %+v", list)
		}

		grouped, err := d.ListDeploymentsByModelIDs(ctx, []string{model.ID})
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, x := range grouped[model.ID] {
			if x.Name == "bedrock-eu" {
				found = true
				if x.Region != "eu-central-1" || x.BedrockModelID != testBedrockModelID {
					t.Errorf("ListDeploymentsByModelIDs = %q %q", x.Region, x.BedrockModelID)
				}
			}
		}
		if !found {
			t.Error("deployment missing from ListDeploymentsByModelIDs")
		}

		// Update only region, then only model ID, then clear, then nil-untouched.
		r := "us-east-1"
		upd, err := d.UpdateDeployment(ctx, dep.ID, UpdateDeploymentParams{Region: &r})
		if err != nil {
			t.Fatal(err)
		}
		if upd.Region != "us-east-1" || upd.BedrockModelID != testBedrockModelID {
			t.Errorf("region update = %q %q", upd.Region, upd.BedrockModelID)
		}
		id := testBedrockModelID2
		upd, err = d.UpdateDeployment(ctx, dep.ID, UpdateDeploymentParams{BedrockModelID: &id})
		if err != nil {
			t.Fatal(err)
		}
		if upd.Region != "us-east-1" || upd.BedrockModelID != testBedrockModelID2 {
			t.Errorf("model id update = %q %q", upd.Region, upd.BedrockModelID)
		}
		prio := 9
		upd, err = d.UpdateDeployment(ctx, dep.ID, UpdateDeploymentParams{Priority: &prio})
		if err != nil {
			t.Fatal(err)
		}
		if upd.Region != "us-east-1" || upd.BedrockModelID != testBedrockModelID2 {
			t.Errorf("unrelated update changed bedrock fields: %q %q", upd.Region, upd.BedrockModelID)
		}
		empty := ""
		upd, err = d.UpdateDeployment(ctx, dep.ID, UpdateDeploymentParams{Region: &empty, BedrockModelID: &empty})
		if err != nil {
			t.Fatal(err)
		}
		if upd.Region != "" || upd.BedrockModelID != "" {
			t.Errorf("clear = %q %q", upd.Region, upd.BedrockModelID)
		}
	})
}

func TestBedrockFields_SyncYAMLModels(t *testing.T) {
	t.Parallel()
	forEachDialect(t, func(t *testing.T, d *DB) {
		ctx := context.Background()
		key := make([]byte, 32)

		cfgModels := []config.ModelConfig{
			{Name: "claude", Provider: "bedrock", Region: "eu-central-1", BedrockModelID: testBedrockModelID},
			{
				Name: "multi", Provider: "bedrock", Strategy: "priority",
				Deployments: []config.DeploymentConfig{
					{Name: "a", Provider: "bedrock", Region: "eu-central-1", BedrockModelID: testBedrockModelID, Priority: 1},
					{Name: "b", Provider: "anthropic", BaseURL: "https://api.anthropic.com", Priority: 2},
				},
			},
		}
		if err := d.SyncYAMLModels(ctx, cfgModels, key); err != nil {
			t.Fatalf("SyncYAMLModels create: %v", err)
		}

		m, err := d.GetModelByName(ctx, "claude")
		if err != nil {
			t.Fatal(err)
		}
		if m.Region != "eu-central-1" || m.BedrockModelID != testBedrockModelID || m.BaseURL != "" || m.Source != "yaml" {
			t.Errorf("created yaml model = %+v", m)
		}
		multi, err := d.GetModelByName(ctx, "multi")
		if err != nil {
			t.Fatal(err)
		}
		deps, err := d.ListDeployments(ctx, multi.ID)
		if err != nil {
			t.Fatal(err)
		}
		byName := map[string]Deployment{}
		for _, x := range deps {
			byName[x.Name] = x
		}
		if byName["a"].Region != "eu-central-1" || byName["a"].BedrockModelID != testBedrockModelID || byName["b"].Region != "" {
			t.Errorf("created yaml deployments = %+v", deps)
		}

		// Re-sync with changed values: yaml wins and updates both models and deployments.
		cfgModels[0].Region = "us-east-1"
		cfgModels[0].BedrockModelID = testBedrockModelID2
		cfgModels[1].Deployments[0].Region = "us-east-1"
		cfgModels[1].Deployments[0].BedrockModelID = testBedrockModelID2
		if err := d.SyncYAMLModels(ctx, cfgModels, key); err != nil {
			t.Fatalf("SyncYAMLModels update: %v", err)
		}
		m, err = d.GetModelByName(ctx, "claude")
		if err != nil {
			t.Fatal(err)
		}
		if m.Region != "us-east-1" || m.BedrockModelID != testBedrockModelID2 {
			t.Errorf("updated yaml model = %q %q", m.Region, m.BedrockModelID)
		}
		deps, err = d.ListDeployments(ctx, multi.ID)
		if err != nil {
			t.Fatal(err)
		}
		for _, x := range deps {
			if x.Name == "a" && (x.Region != "us-east-1" || x.BedrockModelID != testBedrockModelID2) {
				t.Errorf("updated yaml deployment = %q %q", x.Region, x.BedrockModelID)
			}
		}

		// Removing the bedrock fields in yaml (provider switched) clears them.
		cfgModels[0] = config.ModelConfig{Name: "claude", Provider: "openai", BaseURL: "https://api.openai.com/v1"}
		if err := d.SyncYAMLModels(ctx, cfgModels, key); err != nil {
			t.Fatalf("SyncYAMLModels switch: %v", err)
		}
		m, err = d.GetModelByName(ctx, "claude")
		if err != nil {
			t.Fatal(err)
		}
		if m.Region != "" || m.BedrockModelID != "" || m.Provider != "openai" {
			t.Errorf("switched model = %+v", m)
		}
	})
}

func TestBedrockFields_SyncYAMLDoesNotTouchAPIModels(t *testing.T) {
	t.Parallel()
	forEachDialect(t, func(t *testing.T, d *DB) {
		ctx := context.Background()
		api, err := d.CreateModel(ctx, CreateModelParams{
			Name: "claude", Provider: "bedrock", Region: "eu-central-1", BedrockModelID: testBedrockModelID, Source: "api",
		})
		if err != nil {
			t.Fatal(err)
		}
		err = d.SyncYAMLModels(ctx, []config.ModelConfig{
			{Name: "claude", Provider: "bedrock", Region: "us-east-1", BedrockModelID: testBedrockModelID2},
		}, make([]byte, 32))
		if err != nil {
			t.Fatal(err)
		}
		got, err := d.GetModel(ctx, api.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Region != "eu-central-1" || got.BedrockModelID != testBedrockModelID {
			t.Errorf("api model overwritten by yaml sync: %q %q", got.Region, got.BedrockModelID)
		}
	})
}

// ---- migration 0020 ---------------------------------------------------------

func readMigration0020(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("migrations/" + name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}

func columnExists(t *testing.T, d *DB, table, column string) bool {
	t.Helper()
	_, err := d.sql.ExecContext(context.Background(), "SELECT "+column+" FROM "+table+" WHERE 1 = 0")
	return err == nil
}

func execInTx(t *testing.T, d *DB, stmts string) {
	t.Helper()
	ctx := context.Background()
	if err := d.WithTx(ctx, func(q Querier) error {
		_, err := q.ExecContext(ctx, stmts)
		return err
	}); err != nil {
		t.Fatalf("exec migration SQL: %v", err)
	}
}

func TestMigration0020_ColumnsExistWithEmptyDefaults(t *testing.T) {
	t.Parallel()
	forEachDialect(t, func(t *testing.T, d *DB) {
		for _, c := range []struct{ table, col string }{
			{"models", "region"}, {"models", "bedrock_model_id"},
			{"model_deployments", "region"}, {"model_deployments", "bedrock_model_id"},
		} {
			if !columnExists(t, d, c.table, c.col) {
				t.Errorf("column %s.%s missing after migrations", c.table, c.col)
			}
		}

		// A row inserted without naming the new columns gets ''.
		ctx := context.Background()
		p := d.dialect.Placeholder
		mid := uuid.NewString()
		if _, err := d.sql.ExecContext(ctx,
			"INSERT INTO models (id, name, provider, base_url, is_active, source, created_at, updated_at) VALUES ("+
				p(1)+", "+p(2)+", 'openai', 'https://x', 1, 'api', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)", mid, "defaults-model"); err != nil {
			t.Fatalf("insert model: %v", err)
		}
		if got := rawColumnValue(t, d, "models", "region", mid); got != "" {
			t.Errorf("models.region default = %q, want empty", got)
		}
		if got := rawColumnValue(t, d, "models", "bedrock_model_id", mid); got != "" {
			t.Errorf("models.bedrock_model_id default = %q, want empty", got)
		}
		did := uuid.NewString()
		if _, err := d.sql.ExecContext(ctx,
			"INSERT INTO model_deployments (id, model_id, name, provider, base_url, weight, priority, is_active, created_at, updated_at) VALUES ("+
				p(1)+", "+p(2)+", 'd', 'openai', 'https://x', 1, 0, 1, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)", did, mid); err != nil {
			t.Fatalf("insert deployment: %v", err)
		}
		if got := rawColumnValue(t, d, "model_deployments", "region", did); got != "" {
			t.Errorf("model_deployments.region default = %q, want empty", got)
		}
		if got := rawColumnValue(t, d, "model_deployments", "bedrock_model_id", did); got != "" {
			t.Errorf("model_deployments.bedrock_model_id default = %q, want empty", got)
		}
	})
}

// TestMigration0020_BackfillsPreExistingRows creates fully populated rows,
// removes the 0020 columns with the real down migration (which leaves the rows
// in the exact shape of a pre-0020 installation), applies the real up
// migration and checks the old rows read back with ” in the new columns.
func TestMigration0020_BackfillsPreExistingRows(t *testing.T) {
	t.Parallel()
	forEachDialect(t, func(t *testing.T, d *DB) {
		ctx := context.Background()

		var modelIDs []string
		for _, name := range []string{"legacy-a", "legacy-b"} {
			m, err := d.CreateModel(ctx, CreateModelParams{
				Name: name, Provider: "openai", BaseURL: "https://api.openai.com/v1", Source: "api", Aliases: name + "-alias",
			})
			if err != nil {
				t.Fatalf("CreateModel: %v", err)
			}
			modelIDs = append(modelIDs, m.ID)
		}
		dep, err := d.CreateDeployment(ctx, CreateDeploymentParams{
			ModelID: modelIDs[0], Name: "legacy-dep", Provider: "openai", BaseURL: "https://api.openai.com/v1", Weight: 3, Priority: 2,
		})
		if err != nil {
			t.Fatalf("CreateDeployment: %v", err)
		}

		execInTx(t, d, readMigration0020(t, "0020_bedrock_fields.down.sql"))
		for _, c := range []struct{ table, col string }{
			{"models", "region"}, {"models", "bedrock_model_id"},
			{"model_deployments", "region"}, {"model_deployments", "bedrock_model_id"},
		} {
			if columnExists(t, d, c.table, c.col) {
				t.Fatalf("column %s.%s still exists after the down migration", c.table, c.col)
			}
		}

		execInTx(t, d, readMigration0020(t, "0020_bedrock_fields.up.sql"))

		for _, id := range modelIDs {
			if got := rawColumnValue(t, d, "models", "region", id); got != "" {
				t.Errorf("legacy model region = %q, want empty", got)
			}
			if got := rawColumnValue(t, d, "models", "bedrock_model_id", id); got != "" {
				t.Errorf("legacy model bedrock_model_id = %q, want empty", got)
			}
		}
		if got := rawColumnValue(t, d, "model_deployments", "region", dep.ID); got != "" {
			t.Errorf("legacy deployment region = %q, want empty", got)
		}
		if got := rawColumnValue(t, d, "model_deployments", "bedrock_model_id", dep.ID); got != "" {
			t.Errorf("legacy deployment bedrock_model_id = %q, want empty", got)
		}

		// The store reads and writes the backfilled rows normally.
		m, err := d.GetModel(ctx, modelIDs[0])
		if err != nil {
			t.Fatalf("GetModel on legacy row: %v", err)
		}
		if m.Region != "" || m.BedrockModelID != "" || m.Aliases != "legacy-a-alias" {
			t.Errorf("legacy model = %+v", m)
		}
		gotDep, err := d.GetDeployment(ctx, dep.ID)
		if err != nil {
			t.Fatalf("GetDeployment on legacy row: %v", err)
		}
		if gotDep.Region != "" || gotDep.BedrockModelID != "" || gotDep.Weight != 3 || gotDep.Priority != 2 {
			t.Errorf("legacy deployment = %+v", gotDep)
		}
		r := "eu-central-1"
		if upd, err := d.UpdateModel(ctx, modelIDs[0], UpdateModelParams{Region: &r}); err != nil || upd.Region != r {
			t.Fatalf("UpdateModel on backfilled row: %v %+v", err, upd)
		}
	})
}

func TestMigration0020_DownThenUpPreservesOtherData(t *testing.T) {
	t.Parallel()
	forEachDialect(t, func(t *testing.T, d *DB) {
		ctx := context.Background()
		m, err := d.CreateModel(ctx, CreateModelParams{
			Name: "keep", Provider: "bedrock", Region: "eu-central-1", BedrockModelID: testBedrockModelID,
			Source: "api", Aliases: "k1,k2", GCPProject: "proj", GCPLocation: "us-central1",
		})
		if err != nil {
			t.Fatal(err)
		}
		execInTx(t, d, readMigration0020(t, "0020_bedrock_fields.down.sql"))
		execInTx(t, d, readMigration0020(t, "0020_bedrock_fields.up.sql"))

		got, err := d.GetModel(ctx, m.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Name != "keep" || got.Aliases != "k1,k2" || got.GCPProject != "proj" || got.GCPLocation != "us-central1" {
			t.Errorf("other columns damaged: %+v", got)
		}
		if got.Region != "" || got.BedrockModelID != "" {
			t.Errorf("bedrock columns must be reset by down+up, got %q %q", got.Region, got.BedrockModelID)
		}
	})
}
