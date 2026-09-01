package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"cpa-usage-keeper/internal/cpa"
	"cpa-usage-keeper/internal/models"
	"cpa-usage-keeper/internal/repository"
)

type pricingSyncModelsFetcher struct {
	result *cpa.ModelsResult
	err    error
}

func (s pricingSyncModelsFetcher) FetchModels(context.Context) (*cpa.ModelsResult, error) {
	return s.result, s.err
}

func TestExternalPricingSyncerUpdatesCurrentModelsAndPreservesUnmatchedPrices(t *testing.T) {
	db := openPricingServiceTestDatabase(t)
	if _, err := repository.UpsertModelPriceSetting(db, repository.ModelPriceSettingInput{
		Model: "manual-only", PromptPricePer1M: 9, CompletionPricePer1M: 10, CachePricePer1M: 1,
	}); err != nil {
		t.Fatalf("seed manual pricing: %v", err)
	}

	catalog := `{
		"xai":{"models":{"grok-4.5":{"id":"grok-4.5","cost":{"input":2,"output":6,"cache_read":0.3}}}},
		"opencode":{"models":{"grok-4.5":{"id":"grok-4.5","cost":{"input":2,"output":6,"cache_read":0.5}}}},
		"openai":{"models":{"gpt-5.6-sol":{"id":"gpt-5.6-sol","cost":{"input":5,"output":30,"cache_read":0.5}}}},
		"anthropic":{"models":{"claude-opus-4-7":{"id":"claude-opus-4-7","cost":{"input":5,"output":25,"cache_read":0.5}}}},
		"google":{"models":{"gemini-3.5-flash":{"id":"gemini-3.5-flash","cost":{"input":1.5,"output":9,"cache_read":0.15}},"gemini-3.7-flash":{"id":"gemini-3.7-flash","cost":{"input":0.75,"output":3.75,"cache_read":0.075}}}}
	}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") != "cpa-usage-keeper/pricing-sync" {
			t.Errorf("unexpected user agent %q", r.Header.Get("User-Agent"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(catalog))
	}))
	defer server.Close()

	syncer := NewExternalPricingSyncer(db, pricingSyncModelsFetcher{result: &cpa.ModelsResult{
		Payload: cpa.ModelsResponse{Data: []cpa.ModelInfo{
			{ID: "grok-4.5", OwnedBy: "opencode"},
			{ID: "gpt-5.6-sol", OwnedBy: "openai"},
			{ID: "openai/gpt-5.6-sol"},
			{ID: "claude-opus-4-7-thinking", OwnedBy: "anthropic"},
			{ID: "gemini-3.5-flash-extra-low", OwnedBy: "google"},
			{ID: "gemini-3.7-flash-high", OwnedBy: "google"},
			{ID: "unpriced-model", OwnedBy: "unknown"},
		}},
	}}, &http.Client{Timeout: time.Second}, server.URL)
	if err := syncer.SyncPricing(context.Background()); err != nil {
		t.Fatalf("SyncPricing returned error: %v", err)
	}

	settings, err := repository.ListModelPriceSettings(db)
	if err != nil {
		t.Fatalf("list synced pricing: %v", err)
	}
	byModel := make(map[string]models.ModelPriceSetting, len(settings))
	for _, setting := range settings {
		byModel[setting.Model] = setting
	}
	assertSyncedPrice(t, byModel["grok-4.5"], 2, 6, 0.3)
	assertSyncedPrice(t, byModel["gpt-5.6-sol"], 5, 30, 0.5)
	assertSyncedPrice(t, byModel["openai/gpt-5.6-sol"], 5, 30, 0.5)
	assertSyncedPrice(t, byModel["claude-opus-4-7-thinking"], 5, 25, 0.5)
	assertSyncedPrice(t, byModel["gemini-3.5-flash-extra-low"], 1.5, 9, 0.15)
	assertSyncedPrice(t, byModel["gemini-3.7-flash-high"], 0.75, 3.75, 0.075)
	assertSyncedPrice(t, byModel["manual-only"], 9, 10, 1)
	if _, ok := byModel["unpriced-model"]; ok {
		t.Fatal("expected catalog-missing model to remain unpriced")
	}
}

func TestExternalPricingSyncerKeepsExistingPricesWhenCatalogFails(t *testing.T) {
	db := openPricingServiceTestDatabase(t)
	if _, err := repository.UpsertModelPriceSetting(db, repository.ModelPriceSettingInput{
		Model: "grok-4.5", PromptPricePer1M: 2, CompletionPricePer1M: 6, CachePricePer1M: 0.3,
	}); err != nil {
		t.Fatalf("seed pricing: %v", err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unavailable", http.StatusBadGateway)
	}))
	defer server.Close()

	syncer := NewExternalPricingSyncer(db, pricingSyncModelsFetcher{result: &cpa.ModelsResult{
		Payload: cpa.ModelsResponse{Data: []cpa.ModelInfo{{ID: "grok-4.5", OwnedBy: "xai"}}},
	}}, server.Client(), server.URL)
	if err := syncer.SyncPricing(context.Background()); err == nil {
		t.Fatal("expected catalog failure")
	}

	settings, err := repository.ListModelPriceSettings(db)
	if err != nil {
		t.Fatalf("list pricing after failed sync: %v", err)
	}
	if len(settings) != 1 {
		t.Fatalf("expected existing pricing to be preserved, got %+v", settings)
	}
	assertSyncedPrice(t, settings[0], 2, 6, 0.3)
}

func TestExternalPricingSyncerRollsBackAllUpdatesWhenOneWriteFails(t *testing.T) {
	db := openPricingServiceTestDatabase(t)
	for _, input := range []repository.ModelPriceSettingInput{
		{Model: "grok-4.5", PromptPricePer1M: 1, CompletionPricePer1M: 1, CachePricePer1M: 1},
		{Model: "gpt-5.6-sol", PromptPricePer1M: 1, CompletionPricePer1M: 1, CachePricePer1M: 1},
	} {
		if _, err := repository.UpsertModelPriceSetting(db, input); err != nil {
			t.Fatalf("seed pricing for %s: %v", input.Model, err)
		}
	}
	if err := db.Exec(`CREATE TRIGGER fail_gpt_pricing BEFORE UPDATE ON model_price_settings
		WHEN NEW.model = 'gpt-5.6-sol' BEGIN SELECT RAISE(ABORT, 'forced failure'); END`).Error; err != nil {
		t.Fatalf("create failure trigger: %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{
			"xai":{"models":{"grok-4.5":{"id":"grok-4.5","cost":{"input":2,"output":6,"cache_read":0.3}}}},
			"openai":{"models":{"gpt-5.6-sol":{"id":"gpt-5.6-sol","cost":{"input":5,"output":30,"cache_read":0.5}}}}
		}`))
	}))
	defer server.Close()
	syncer := NewExternalPricingSyncer(db, pricingSyncModelsFetcher{result: &cpa.ModelsResult{
		Payload: cpa.ModelsResponse{Data: []cpa.ModelInfo{
			{ID: "grok-4.5", OwnedBy: "xai"},
			{ID: "gpt-5.6-sol", OwnedBy: "openai"},
		}},
	}}, server.Client(), server.URL)
	if err := syncer.SyncPricing(context.Background()); err == nil {
		t.Fatal("expected transactional pricing update failure")
	}

	settings, err := repository.ListModelPriceSettings(db)
	if err != nil {
		t.Fatalf("list pricing after rolled back sync: %v", err)
	}
	for _, setting := range settings {
		assertSyncedPrice(t, setting, 1, 1, 1)
	}
}

func assertSyncedPrice(t *testing.T, setting models.ModelPriceSetting, prompt, completion, cache float64) {
	t.Helper()
	if setting.PromptPricePer1M != prompt || setting.CompletionPricePer1M != completion || setting.CachePricePer1M != cache {
		t.Fatalf("unexpected pricing for %q: %+v", setting.Model, setting)
	}
}
