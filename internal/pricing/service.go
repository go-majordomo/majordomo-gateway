package pricing

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"sort"
	"sync"
	"time"

	"github.com/go-majordomo/majordomo-gateway/internal/models"
)

// dateSuffixRE matches a trailing model version date: OpenAI-style "-2026-03-17"
// or Anthropic-style "-20251001". Providers return dated model ids in usage
// responses (e.g. gpt-5.4-mini-2026-03-17) that map to an undated canonical
// pricing entry (gpt-5.4-mini); stripping the suffix lets new releases resolve
// without a hand-maintained alias per date.
var dateSuffixRE = regexp.MustCompile(`-(\d{4}-\d{2}-\d{2}|\d{8})$`)

// stripDateSuffix removes a trailing date suffix, returning the base name and
// whether a suffix was stripped.
func stripDateSuffix(model string) (string, bool) {
	if loc := dateSuffixRE.FindStringIndex(model); loc != nil {
		return model[:loc[0]], true
	}
	return model, false
}

// Cache-write pricing multipliers, applied to the base input rate. Anthropic
// charges 1.25x the base input price for 5-minute cache writes and 2x for
// 1-hour writes; cache reads are 0.1x (priced per-model via CachedPricePerMillion).
// See https://platform.claude.com/docs/en/docs/build-with-claude/prompt-caching.
const (
	cacheWrite5mMultiplier = 1.25
	cacheWrite1hMultiplier = 2.0
)

type ModelPricing struct {
	InputPricePerMillion  float64
	OutputPricePerMillion float64
	CachedPricePerMillion float64
}

type remotePricingResponse struct {
	UpdatedAt string             `json:"updated_at"`
	Prices    []remotePriceEntry `json:"prices"`
}

type remotePriceEntry struct {
	ID          string   `json:"id"`
	Vendor      string   `json:"vendor"`
	Name        string   `json:"name"`
	Input       float64  `json:"input"`
	Output      float64  `json:"output"`
	InputCached *float64 `json:"input_cached"`
}

type fallbackPriceEntry struct {
	InputPricePerMillion  float64 `json:"input_price_per_million"`
	OutputPricePerMillion float64 `json:"output_price_per_million"`
	CachedPricePerMillion float64 `json:"cached_price_per_million"`
}

// Endpoint is one provider that can serve a canonical routing slug, with the
// vendor-native model id to send and the provider's data-handling posture. It is
// derived from the model catalog's routes + provider sections; prices are looked
// up separately via Price(provider, ProviderModelID).
type Endpoint struct {
	Provider        string
	BaseURL         string
	ProviderModelID string
	Region          string
	ZDR             bool
	DataCollection  string
}

// catalogFile is the on-disk shape of model_catalog.json: provider sections
// carrying prices + data posture, and a routes section grouping the (provider,
// model) pairs that serve each canonical slug.
type catalogFile struct {
	Providers map[string]providerSection `json:"providers"`
	Routes    map[string][]routeEntry    `json:"routes"`
}

type providerSection struct {
	BaseURL        string                        `json:"base_url"`
	ZDR            bool                          `json:"zdr"`
	DataCollection string                        `json:"data_collection"`
	Region         string                        `json:"region"`
	Models         map[string]fallbackPriceEntry `json:"models"`
}

type routeEntry struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

type Service struct {
	remoteURL       string
	catalogFile     string
	aliasesFile     string
	refreshInterval time.Duration

	mu sync.RWMutex
	// pricing is the provider-agnostic model-default map (model id -> pricing).
	pricing map[string]ModelPricing
	// byProvider is the per-provider price index (provider -> model id -> pricing),
	// checked before the model-default map so a model id served by several
	// providers resolves to the correct per-provider price.
	byProvider map[string]map[string]ModelPricing
	// catalog is the routing catalog (canonical slug -> candidate endpoints),
	// derived from the routes section joined to the provider sections. Sourced
	// only from the local catalog file (the remote price feed has no routes).
	catalog map[string][]Endpoint
	aliases map[string]string // maps API model name -> pricing model name

	httpClient *http.Client
	done       chan struct{}
}

func NewService(remoteURL, catalogFile, aliasesFile string, refreshInterval time.Duration) *Service {
	s := &Service{
		remoteURL:       remoteURL,
		catalogFile:     catalogFile,
		aliasesFile:     aliasesFile,
		refreshInterval: refreshInterval,
		pricing:         make(map[string]ModelPricing),
		byProvider:      make(map[string]map[string]ModelPricing),
		catalog:         make(map[string][]Endpoint),
		aliases:         make(map[string]string),
		httpClient:      &http.Client{Timeout: 30 * time.Second},
		done:            make(chan struct{}),
	}

	s.loadAliases()
	s.loadPricing()
	go s.refreshLoop()

	return s
}

func (s *Service) loadAliases() {
	if s.aliasesFile == "" {
		return
	}

	data, err := os.ReadFile(s.aliasesFile)
	if err != nil {
		slog.Warn("failed to load model aliases", "error", err, "file", s.aliasesFile)
		return
	}

	var aliases map[string]string
	if err := json.Unmarshal(data, &aliases); err != nil {
		slog.Error("failed to parse model aliases", "error", err)
		return
	}

	s.mu.Lock()
	s.aliases = aliases
	s.mu.Unlock()

	slog.Info("loaded model aliases", "count", len(aliases))
}

func (s *Service) loadPricing() {
	if s.remoteURL != "" {
		if err := s.fetchRemote(); err != nil {
			slog.Warn("failed to fetch remote pricing, using local catalog", "error", err)
			s.loadLocal()
			return
		}
		// Merge local catalog entries missing from the remote price feed (so the
		// catalog supplements models not yet in the remote source) and load the
		// routing catalog, which only the local file carries.
		s.mergeLocal()
	} else {
		s.loadLocal()
	}
}

func (s *Service) fetchRemote() error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.remoteURL, nil)
	if err != nil {
		return err
	}

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	var remoteResp remotePricingResponse
	if err := json.NewDecoder(resp.Body).Decode(&remoteResp); err != nil {
		return err
	}

	pricing := make(map[string]ModelPricing)
	byProvider := make(map[string]map[string]ModelPricing)
	for _, entry := range remoteResp.Prices {
		cachedPrice := 0.0
		if entry.InputCached != nil {
			cachedPrice = *entry.InputCached
		}
		mp := ModelPricing{
			InputPricePerMillion:  entry.Input,
			OutputPricePerMillion: entry.Output,
			CachedPricePerMillion: cachedPrice,
		}
		pricing[entry.ID] = mp
		// The remote source qualifies each price by vendor; index it so the same
		// model id served by multiple vendors resolves to the right per-provider price.
		if entry.Vendor != "" {
			if byProvider[entry.Vendor] == nil {
				byProvider[entry.Vendor] = make(map[string]ModelPricing)
			}
			byProvider[entry.Vendor][entry.ID] = mp
		}
	}

	s.mu.Lock()
	s.pricing = pricing
	s.byProvider = byProvider
	s.mu.Unlock()

	slog.Info("loaded pricing data from remote", "models", len(pricing), "providers", len(byProvider), "updated_at", remoteResp.UpdatedAt)
	return nil
}

func (s *Service) loadLocal() {
	data, err := os.ReadFile(s.catalogFile)
	if err != nil {
		slog.Error("failed to load model catalog", "error", err, "file", s.catalogFile)
		return
	}

	pricing, byProvider, catalog, err := parseCatalog(data)
	if err != nil {
		slog.Error("failed to parse model catalog", "error", err, "file", s.catalogFile)
		return
	}

	s.mu.Lock()
	s.pricing = pricing
	s.byProvider = byProvider
	s.catalog = catalog
	s.mu.Unlock()

	slog.Info("loaded model catalog", "models", len(pricing), "providers", len(byProvider), "routes", len(catalog))
}

func (s *Service) mergeLocal() {
	data, err := os.ReadFile(s.catalogFile)
	if err != nil {
		return
	}

	pricing, byProvider, catalog, err := parseCatalog(data)
	if err != nil {
		return
	}

	s.mu.Lock()
	added := 0
	for model, mp := range pricing {
		if _, exists := s.pricing[model]; !exists {
			s.pricing[model] = mp
			added++
		}
	}
	for prov, models := range byProvider {
		if s.byProvider[prov] == nil {
			s.byProvider[prov] = make(map[string]ModelPricing)
		}
		for model, mp := range models {
			if _, exists := s.byProvider[prov][model]; !exists {
				s.byProvider[prov][model] = mp
				added++
			}
		}
	}
	s.catalog = catalog // routes are only in the local file
	s.mu.Unlock()

	if added > 0 {
		slog.Info("merged catalog pricing missing from remote", "count", added)
	}
}

// parseCatalog parses model_catalog.json into the model-default price map, the
// per-provider price index, and the routing catalog. Prices come from each
// provider section's models; the flat default is populated in sorted-provider
// order (first provider that lists a model id owns the default, for stability).
// The routing catalog joins each routes entry to its provider section; entries
// referencing an unknown provider or model are skipped with a warning.
func parseCatalog(data []byte) (map[string]ModelPricing, map[string]map[string]ModelPricing, map[string][]Endpoint, error) {
	var cf catalogFile
	if err := json.Unmarshal(data, &cf); err != nil {
		return nil, nil, nil, err
	}

	pricing := make(map[string]ModelPricing)
	byProvider := make(map[string]map[string]ModelPricing)

	provNames := make([]string, 0, len(cf.Providers))
	for p := range cf.Providers {
		provNames = append(provNames, p)
	}
	sort.Strings(provNames)
	for _, prov := range provNames {
		sec := cf.Providers[prov]
		byProvider[prov] = make(map[string]ModelPricing, len(sec.Models))
		for model, entry := range sec.Models {
			mp := entry.toModelPricing()
			byProvider[prov][model] = mp
			if _, exists := pricing[model]; !exists {
				pricing[model] = mp
			}
		}
	}

	catalog := make(map[string][]Endpoint)
	for slug, entries := range cf.Routes {
		for _, re := range entries {
			sec, ok := cf.Providers[re.Provider]
			if !ok {
				slog.Warn("catalog route references unknown provider", "slug", slug, "provider", re.Provider)
				continue
			}
			if _, ok := sec.Models[re.Model]; !ok {
				slog.Warn("catalog route references unknown model", "slug", slug, "provider", re.Provider, "model", re.Model)
				continue
			}
			catalog[slug] = append(catalog[slug], Endpoint{
				Provider:        re.Provider,
				BaseURL:         sec.BaseURL,
				ProviderModelID: re.Model,
				Region:          sec.Region,
				ZDR:             sec.ZDR,
				DataCollection:  sec.DataCollection,
			})
		}
	}

	return pricing, byProvider, catalog, nil
}

func (e fallbackPriceEntry) toModelPricing() ModelPricing {
	return ModelPricing{
		InputPricePerMillion:  e.InputPricePerMillion,
		OutputPricePerMillion: e.OutputPricePerMillion,
		CachedPricePerMillion: e.CachedPricePerMillion,
	}
}

func (s *Service) refreshLoop() {
	ticker := time.NewTicker(s.refreshInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if err := s.fetchRemote(); err != nil {
				slog.Warn("failed to refresh pricing", "error", err)
			} else {
				s.mergeLocal()
			}
		case <-s.done:
			return
		}
	}
}

// resolve maps a (provider, model) pair to its pricing entry. It first consults
// the per-provider override map (exact id, then date-stripped) so the same model
// id served by multiple providers resolves to the right per-provider price, then
// falls back to the provider-agnostic model map: exact match, the alias table,
// then both again after stripping a trailing date suffix. provider may be empty
// (non-routed lookups), in which case only the model map is used. Caller must
// hold s.mu.
func (s *Service) resolve(provider, model string) (ModelPricing, bool) {
	if provider != "" {
		if pm, ok := s.byProvider[provider]; ok {
			if p, ok := pm[model]; ok {
				return p, true
			}
			if base, stripped := stripDateSuffix(model); stripped {
				if p, ok := pm[base]; ok {
					return p, true
				}
			}
		}
	}
	if p, ok := s.pricing[model]; ok {
		return p, true
	}
	if aliased, ok := s.aliases[model]; ok {
		if p, ok := s.pricing[aliased]; ok {
			return p, true
		}
	}
	if base, stripped := stripDateSuffix(model); stripped {
		if p, ok := s.pricing[base]; ok {
			return p, true
		}
		if aliased, ok := s.aliases[base]; ok {
			if p, ok := s.pricing[aliased]; ok {
				return p, true
			}
		}
	}
	return ModelPricing{}, false
}

// Price returns the pricing for a (provider, model) pair, for use by the router's
// cost-weighted selection. provider may be empty to look up by model alone. ok is
// false when no price is known.
func (s *Service) Price(provider, model string) (ModelPricing, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.resolve(provider, model)
}

// Candidates returns the routing catalog's candidate endpoints for a canonical
// model slug. ok is false when the slug is not routable, in which case the
// caller falls through to pass-through behavior. Satisfies the router's
// CandidateSource.
func (s *Service) Candidates(model string) ([]Endpoint, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	eps, ok := s.catalog[model]
	return eps, ok
}

func (s *Service) Calculate(metrics *models.UsageMetrics) models.Cost {
	s.mu.RLock()
	pricing, ok := s.resolve(metrics.Provider, metrics.Model)
	s.mu.RUnlock()

	if !ok {
		slog.Warn("no pricing found for model", "provider", metrics.Provider, "model", metrics.Model)
		return models.Cost{ModelAliasFound: false}
	}

	// InputTokens is the normalized total input (includes cache-read and
	// cache-creation tokens). Split it into three separately-priced buckets:
	// fresh input at the base rate, cache reads at the cached rate, and cache
	// writes at the base rate scaled by the per-TTL multiplier (5m = 1.25x,
	// 1h = 2x). splitCacheCreation in the parser guarantees the 5m/1h buckets
	// sum to CacheCreationTokens, attributing to 5m when the provider omits the
	// breakdown.
	// Guard the 5m+1h == CacheCreationTokens invariant: if a parser reports a
	// cache-creation total without the per-TTL breakdown, attribute the whole
	// amount to the 5-minute bucket (the default TTL) so those tokens are still
	// priced rather than silently dropped from the input bucket at zero cost.
	create5m := metrics.CacheCreation5mTokens
	create1h := metrics.CacheCreation1hTokens
	if create5m+create1h == 0 && metrics.CacheCreationTokens > 0 {
		create5m = metrics.CacheCreationTokens
	}

	nonCacheInput := metrics.InputTokens - metrics.CachedTokens - metrics.CacheCreationTokens
	inputCost := float64(nonCacheInput) * pricing.InputPricePerMillion / 1_000_000
	cachedCost := float64(metrics.CachedTokens) * pricing.CachedPricePerMillion / 1_000_000
	cacheCreationCost := (float64(create5m)*cacheWrite5mMultiplier +
		float64(create1h)*cacheWrite1hMultiplier) *
		pricing.InputPricePerMillion / 1_000_000
	outputCost := float64(metrics.OutputTokens) * pricing.OutputPricePerMillion / 1_000_000

	return models.Cost{
		InputCost:         inputCost,
		CachedCost:        cachedCost,
		CacheCreationCost: cacheCreationCost,
		OutputCost:        outputCost,
		TotalCost:         inputCost + cachedCost + cacheCreationCost + outputCost,
		ModelAliasFound:   true,
	}
}

func (s *Service) Close() {
	close(s.done)
}
