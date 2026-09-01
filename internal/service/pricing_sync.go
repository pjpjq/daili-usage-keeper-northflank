package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"

	"cpa-usage-keeper/internal/cpa"
	"cpa-usage-keeper/internal/repository"
	"github.com/sirupsen/logrus"
	"gorm.io/gorm"
)

type externalPricingSyncer struct {
	db            *gorm.DB
	modelsFetcher ModelsFetcher
	httpClient    *http.Client
	sourceURL     string
}

type modelsDevCatalog map[string]modelsDevProvider

type modelsDevProvider struct {
	Models map[string]modelsDevModel `json:"models"`
}

type modelsDevModel struct {
	ID   string        `json:"id"`
	Cost modelsDevCost `json:"cost"`
}

type modelsDevCost struct {
	Input     *float64 `json:"input"`
	Output    *float64 `json:"output"`
	CacheRead *float64 `json:"cache_read"`
}

type catalogPrice struct {
	promptPer1M     float64
	completionPer1M float64
	cachePer1M      float64
}

type modelPriceUpdate struct {
	model string
	price catalogPrice
}

func NewExternalPricingSyncer(db *gorm.DB, modelsFetcher ModelsFetcher, httpClient *http.Client, sourceURL string) *externalPricingSyncer {
	return &externalPricingSyncer{
		db:            db,
		modelsFetcher: modelsFetcher,
		httpClient:    httpClient,
		sourceURL:     strings.TrimSpace(sourceURL),
	}
}

// SyncPricing refreshes prices only for models currently exposed by CPA. Existing
// prices for unmatched models are intentionally preserved when the catalog changes.
func (s *externalPricingSyncer) SyncPricing(ctx context.Context) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("pricing sync database is nil")
	}
	if s.modelsFetcher == nil {
		return fmt.Errorf("pricing sync models fetcher is nil")
	}
	if s.httpClient == nil {
		return fmt.Errorf("pricing sync HTTP client is nil")
	}
	if s.sourceURL == "" {
		return fmt.Errorf("pricing source URL is required")
	}

	modelsResult, err := s.modelsFetcher.FetchModels(ctx)
	if err != nil {
		return fmt.Errorf("fetch CPA models for pricing sync: %w", err)
	}
	catalog, err := s.fetchCatalog(ctx)
	if err != nil {
		return err
	}
	models := normalizeCPAModelInfos(modelsResult)

	updates := make([]modelPriceUpdate, 0, len(models))
	for _, model := range models {
		price, ok := findCatalogPrice(model, catalog)
		if !ok {
			continue
		}
		updates = append(updates, modelPriceUpdate{model: strings.TrimSpace(model.ID), price: price})
	}
	if err := s.db.Transaction(func(tx *gorm.DB) error {
		for _, update := range updates {
			if _, err := repository.UpsertModelPriceSetting(tx, repository.ModelPriceSettingInput{
				Model:                update.model,
				PromptPricePer1M:     update.price.promptPer1M,
				CompletionPricePer1M: update.price.completionPer1M,
				CachePricePer1M:      update.price.cachePer1M,
			}); err != nil {
				return fmt.Errorf("save synced pricing for %q: %w", update.model, err)
			}
		}
		return nil
	}); err != nil {
		return err
	}

	logrus.WithFields(logrus.Fields{
		"catalog_providers": len(catalog),
		"source":            s.sourceURL,
		"updated":           len(updates),
		"used_models":       len(models),
	}).Info("external pricing sync completed")
	return nil
}

func (s *externalPricingSyncer) fetchCatalog(ctx context.Context) (modelsDevCatalog, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, s.sourceURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build pricing catalog request: %w", err)
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "cpa-usage-keeper/pricing-sync")

	response, err := s.httpClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("fetch pricing catalog: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("fetch pricing catalog: unexpected status %d", response.StatusCode)
	}

	var catalog modelsDevCatalog
	if err := json.NewDecoder(io.LimitReader(response.Body, 20<<20)).Decode(&catalog); err != nil {
		return nil, fmt.Errorf("decode pricing catalog: %w", err)
	}
	return catalog, nil
}

func normalizeCPAModelInfos(result *cpa.ModelsResult) []cpa.ModelInfo {
	if result == nil {
		return []cpa.ModelInfo{}
	}
	seen := make(map[string]struct{}, len(result.Payload.Data))
	models := make([]cpa.ModelInfo, 0, len(result.Payload.Data))
	for _, model := range result.Payload.Data {
		id := strings.TrimSpace(model.ID)
		if id == "" {
			continue
		}
		key := strings.ToLower(id)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		model.ID = id
		models = append(models, model)
	}
	return models
}

func findCatalogPrice(model cpa.ModelInfo, catalog modelsDevCatalog) (catalogPrice, bool) {
	for _, provider := range providerCandidates(model) {
		entry, ok := findProviderModel(provider, catalog[provider], model.ID)
		if !ok {
			continue
		}
		return parseCatalogPrice(entry.Cost)
	}
	return catalogPrice{}, false
}

func findProviderModel(providerName string, provider modelsDevProvider, modelID string) (modelsDevModel, bool) {
	wanted := strings.ToLower(strings.TrimSpace(modelID))
	if slash := strings.LastIndex(wanted, "/"); slash >= 0 && slash < len(wanted)-1 {
		wanted = wanted[slash+1:]
	}
	for _, candidate := range catalogModelCandidates(providerName, wanted) {
		for key, model := range provider.Models {
			if strings.ToLower(strings.TrimSpace(key)) == candidate || strings.ToLower(strings.TrimSpace(model.ID)) == candidate {
				return model, true
			}
		}
	}
	return modelsDevModel{}, false
}

func catalogModelCandidates(provider, model string) []string {
	candidates := []string{model}
	switch provider {
	case "anthropic":
		if strings.HasSuffix(model, "-thinking") {
			candidates = append(candidates, strings.TrimSuffix(model, "-thinking"))
		}
	case "google":
		for _, suffix := range []string{"-extra-low", "-low", "-medium", "-high"} {
			if strings.HasSuffix(model, suffix) {
				candidates = append(candidates, strings.TrimSuffix(model, suffix))
				break
			}
		}
	}
	return candidates
}

func providerCandidates(model cpa.ModelInfo) []string {
	candidates := make([]string, 0, 2)
	appendCandidate := func(provider string) {
		provider = normalizeProvider(provider)
		if provider == "" {
			return
		}
		for _, existing := range candidates {
			if existing == provider {
				return
			}
		}
		candidates = append(candidates, provider)
	}
	appendCandidate(inferProvider(model.ID))
	appendCandidate(model.OwnedBy)
	return candidates
}

func normalizeProvider(provider string) string {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "x-ai", "x_ai", "grok":
		return "xai"
	case "claude":
		return "anthropic"
	case "gemini", "google-vertex", "vertex":
		return "google"
	case "kimi", "moonshot":
		return "moonshotai"
	case "mimo":
		return "xiaomi"
	default:
		return strings.ToLower(strings.TrimSpace(provider))
	}
}

func inferProvider(modelID string) string {
	model := strings.ToLower(strings.TrimSpace(modelID))
	if slash := strings.Index(model, "/"); slash > 0 {
		provider := normalizeProvider(model[:slash])
		if provider != "" {
			return provider
		}
		model = model[slash+1:]
	}
	switch {
	case strings.HasPrefix(model, "grok-"):
		return "xai"
	case strings.HasPrefix(model, "gpt-"), strings.HasPrefix(model, "o1"), strings.HasPrefix(model, "o3"), strings.HasPrefix(model, "o4"):
		return "openai"
	case strings.HasPrefix(model, "claude-"):
		return "anthropic"
	case strings.HasPrefix(model, "gemini-"):
		return "google"
	case strings.HasPrefix(model, "deepseek-"):
		return "deepseek"
	case model == "kimi-for-coding":
		return "kimi-for-coding"
	case strings.HasPrefix(model, "kimi-"):
		return "moonshotai"
	case strings.HasPrefix(model, "mimo-"):
		return "xiaomi"
	default:
		return ""
	}
}

func parseCatalogPrice(cost modelsDevCost) (catalogPrice, bool) {
	if cost.Input == nil || cost.Output == nil || !validPrice(*cost.Input) || !validPrice(*cost.Output) {
		return catalogPrice{}, false
	}
	cache := float64(0)
	if cost.CacheRead != nil {
		if !validPrice(*cost.CacheRead) {
			return catalogPrice{}, false
		}
		cache = *cost.CacheRead
	}
	return catalogPrice{promptPer1M: *cost.Input, completionPer1M: *cost.Output, cachePer1M: cache}, true
}

func validPrice(value float64) bool {
	return value >= 0 && !math.IsInf(value, 0) && !math.IsNaN(value)
}
