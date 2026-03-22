package proxy

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ──────────────────────────────────────────────────────────────────────────────
// GET /v1/models
// ──────────────────────────────────────────────────────────────────────────────

func TestModelsHandler_Status200(t *testing.T) {
	t.Parallel()

	dummy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	t.Cleanup(dummy.Close)

	handler := testProxyHandler(t, dummy.URL)
	app := testApp(t, handler)

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	resp, err := app.Test(req, testTimeout)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
}

func TestModelsHandler_ResponseShape(t *testing.T) {
	t.Parallel()

	dummy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	t.Cleanup(dummy.Close)

	handler := testProxyHandler(t, dummy.URL)
	app := testApp(t, handler)

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	resp, err := app.Test(req, testTimeout)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	var result modelsResponse
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatalf("unmarshal response: %v (body: %s)", err, string(body))
	}

	if result.Object != "list" {
		t.Errorf(`object = %q, want "list"`, result.Object)
	}
	if result.Data == nil {
		t.Error("data is nil, want a non-nil array")
	}
}

func TestModelsHandler_ModelCount(t *testing.T) {
	t.Parallel()

	dummy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	t.Cleanup(dummy.Close)

	handler := testProxyHandler(t, dummy.URL)
	app := testApp(t, handler)

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	resp, err := app.Test(req, testTimeout)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	var result modelsResponse
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}

	// testRegistry registers test-model, azure-model, and no-key-model.
	wantCount := 3
	if len(result.Data) != wantCount {
		t.Errorf("data length = %d, want %d", len(result.Data), wantCount)
	}
}

func TestModelsHandler_EachEntryShape(t *testing.T) {
	t.Parallel()

	dummy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	t.Cleanup(dummy.Close)

	handler := testProxyHandler(t, dummy.URL)
	app := testApp(t, handler)

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	resp, err := app.Test(req, testTimeout)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	var result modelsResponse
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}

	for i, entry := range result.Data {
		if entry.ID == "" {
			t.Errorf("data[%d].id is empty", i)
		}
		if entry.Object != "model" {
			t.Errorf("data[%d].object = %q, want %q", i, entry.Object, "model")
		}
		if entry.OwnedBy == "" {
			t.Errorf("data[%d].owned_by is empty", i)
		}
	}
}

func TestModelsHandler_ModelNamesMatchRegistry(t *testing.T) {
	t.Parallel()

	dummy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	t.Cleanup(dummy.Close)

	handler := testProxyHandler(t, dummy.URL)
	app := testApp(t, handler)

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	resp, err := app.Test(req, testTimeout)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	var result modelsResponse
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}

	gotIDs := make(map[string]bool, len(result.Data))
	for _, entry := range result.Data {
		gotIDs[entry.ID] = true
	}

	// All models registered in testRegistry must appear.
	wantIDs := []string{"test-model", "azure-model", "no-key-model"}
	for _, id := range wantIDs {
		if !gotIDs[id] {
			t.Errorf("model %q not found in /v1/models response", id)
		}
	}
}

func TestModelsHandler_AliasesPresent(t *testing.T) {
	t.Parallel()

	dummy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	t.Cleanup(dummy.Close)

	handler := testProxyHandler(t, dummy.URL)
	app := testApp(t, handler)

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	resp, err := app.Test(req, testTimeout)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	var result modelsResponse
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}

	var found *modelEntry
	for i := range result.Data {
		if result.Data[i].ID == "test-model" {
			found = &result.Data[i]
			break
		}
	}
	if found == nil {
		t.Fatal("test-model not found in response")
	}

	aliasSet := make(map[string]bool, len(found.Aliases))
	for _, a := range found.Aliases {
		aliasSet[a] = true
	}
	for _, wantAlias := range []string{"default", "fast"} {
		if !aliasSet[wantAlias] {
			t.Errorf("alias %q not present in test-model aliases %v", wantAlias, found.Aliases)
		}
	}
}

func TestModelsHandler_SortedByName(t *testing.T) {
	t.Parallel()

	dummy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	t.Cleanup(dummy.Close)

	handler := testProxyHandler(t, dummy.URL)
	app := testApp(t, handler)

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	resp, err := app.Test(req, testTimeout)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	var result modelsResponse
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}

	for i := 1; i < len(result.Data); i++ {
		if result.Data[i].ID < result.Data[i-1].ID {
			t.Errorf("models not sorted: data[%d].id = %q < data[%d].id = %q",
				i, result.Data[i].ID, i-1, result.Data[i-1].ID)
		}
	}
}

func TestModelsHandler_ContentTypeJSON(t *testing.T) {
	t.Parallel()

	dummy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	t.Cleanup(dummy.Close)

	handler := testProxyHandler(t, dummy.URL)
	app := testApp(t, handler)

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	resp, err := app.Test(req, testTimeout)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	ct := resp.Header.Get("Content-Type")
	if !strings.Contains(ct, "application/json") {
		t.Errorf("Content-Type = %q, want it to contain %q", ct, "application/json")
	}
}

func TestModelsHandler_EmptyRegistry(t *testing.T) {
	t.Parallel()

	r, err := NewRegistry(nil)
	if err != nil {
		t.Fatal(err)
	}

	handler := NewProxyHandler(r, slog.New(slog.NewTextHandler(io.Discard, nil)))
	app := testApp(t, handler)

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	resp, err := app.Test(req, testTimeout)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	body, _ := io.ReadAll(resp.Body)
	var result modelsResponse
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if result.Object != "list" {
		t.Errorf(`object = %q, want "list"`, result.Object)
	}
	if len(result.Data) != 0 {
		t.Errorf("data length = %d, want 0 for empty registry", len(result.Data))
	}
}
