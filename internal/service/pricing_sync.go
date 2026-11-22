package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"regexp"
	"sort"
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
	Name string        `json:"name"`
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

// ModelAliasFetcher is optionally implemented by the models fetcher to expose CPA alias config.
type ModelAliasFetcher interface {
	FetchModelAliases(context.Context) ([]cpa.ModelAlias, error)
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

	priced := make(map[string]catalogPrice, len(models))
	updates := make([]modelPriceUpdate, 0, len(models))
	for _, model := range models {
		price, ok := findCatalogPrice(model, catalog)
		if isFreeModel(model.ID) {
			price, ok = catalogPrice{}, true
		}
		if !ok {
			continue
		}
		priced[strings.TrimSpace(model.ID)] = price
		updates = append(updates, modelPriceUpdate{model: strings.TrimSpace(model.ID), price: price})
	}
	updates = append(updates, s.aliasPriceUpdates(ctx, priced)...)
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

// aliasPriceUpdates copies alias prices to upstream model names, because usage events
// record the upstream name while CPA /v1/models only lists the alias.
func (s *externalPricingSyncer) aliasPriceUpdates(ctx context.Context, priced map[string]catalogPrice) []modelPriceUpdate {
	fetcher, ok := s.modelsFetcher.(ModelAliasFetcher)
	if !ok {
		return nil
	}
	aliases, err := fetcher.FetchModelAliases(ctx)
	if err != nil {
		logrus.WithError(err).Warn("fetch CPA model aliases for pricing sync failed; skipping alias prices")
		return nil
	}
	updates := make([]modelPriceUpdate, 0, len(aliases))
	for _, alias := range aliases {
		name := strings.TrimSpace(alias.Name)
		if _, done := priced[name]; done || name == "" {
			continue
		}
		price, ok := priced[strings.TrimSpace(alias.Alias)]
		if isFreeModel(name) {
			price, ok = catalogPrice{}, true
		}
		if !ok {
			continue
		}
		priced[name] = price
		updates = append(updates, modelPriceUpdate{model: name, price: price})
	}
	return updates
}

// freeModelToken matches the "free" tier tag in IDs such as "x:free", "x-free" and "cline-free/x".
var freeModelToken = regexp.MustCompile(`(^|[-:/_])free($|[-:/_])`)

// isFreeModel follows the free-tier ID convention (OpenRouter/Kilo/Cline), which models.dev prices at 0.
func isFreeModel(modelID string) bool {
	return freeModelToken.MatchString(strings.ToLower(strings.TrimSpace(modelID)))
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
	candidates := catalogModelCandidates(providerName, wanted)
	for _, candidate := range candidates {
		for key, model := range provider.Models {
			if strings.ToLower(strings.TrimSpace(key)) == candidate || strings.ToLower(strings.TrimSpace(model.ID)) == candidate {
				return model, true
			}
		}
	}
	for _, candidate := range candidates {
		if model, ok := findProviderModelByName(provider, candidate); ok {
			return model, true
		}
	}
	return modelsDevModel{}, false
}

// findProviderModelByName matches display names ("Claude Opus 5.5" -> claude-opus-5.5) and an
// undated ID against a single dated snapshot ("DeepSeek V4.1 Flash 1017" -> deepseek-v4.1-flash).
func findProviderModelByName(provider modelsDevProvider, wanted string) (modelsDevModel, bool) {
	keys := make([]string, 0, len(provider.Models))
	for key := range provider.Models {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	dated := make([]modelsDevModel, 0, 1)
	for _, key := range keys {
		model := provider.Models[key]
		name := strings.ToLower(strings.Join(strings.Fields(model.Name), "-"))
		if name == "" {
			continue
		}
		if name == wanted {
			return model, true
		}
		if suffix, ok := strings.CutPrefix(name, wanted+"-"); ok && suffix != "" && strings.Trim(suffix, "0123456789") == "" {
			dated = append(dated, model)
		}
	}
	if len(dated) == 1 {
		return dated[0], true
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
	appendCandidate(fallbackProvider(model.ID))
	return candidates
}

// fallbackProvider is tried after owned_by so reseller-hosted models (e.g. glm-5.3 on evroc) get list prices
// without overriding a reseller price the catalog already has (e.g. glm-5.2 on sensenova).
func fallbackProvider(modelID string) string {
	model := strings.ToLower(strings.TrimSpace(modelID))
	if slash := strings.LastIndex(model, "/"); slash >= 0 {
		model = model[slash+1:]
	}
	if strings.HasPrefix(model, "glm-") {
		return "zai"
	}
	return ""
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
	case "zai-org", "z-ai", "zhipu":
		return "zai"
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
