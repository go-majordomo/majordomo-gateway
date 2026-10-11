package pricing

import (
	"context"
	"encoding/json"
	"fmt"
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
	// RequestDefaults are request-body fields the router adds when it rewrites a
	// request for this endpoint and the caller left them unset. Keyed by top-level
	// field name; nil when the endpoint carries no defaults. Shared across
	// requests, so it must be treated as read-only.
	RequestDefaults map[string]json.RawMessage
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
	// RequestDefaults is optional. It exists per route entry rather than per
	// model because the same setting behaves differently on each provider.
	RequestDefaults map[string]json.RawMessage `json:"request_defaults"`
}

// routedModelField is the request-body field the router owns on a routed
// request. A request default may never set it.
const routedModelField = "model"

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

// loadPricing performs the initial price load. A failure leaves the service
// unpriced until the next refresh; it is logged rather than returned because
// the gateway must still proxy requests without cost attribution.
func (s *Service) loadPricing() {
	if err := s.reload(true); err != nil {
		slog.Error("failed to load pricing", "error", err)
	}
}

// reload rebuilds the price maps and routing catalog and swaps them in under a
// single lock, so readers never observe remote prices without the local catalog
// applied on top.
//
// The local catalog is authoritative: an entry present there replaces the
// remote feed's entry for the same model id (and the same provider/model pair).
// The remote feed only fills in models the catalog does not list. Routes come
// solely from the catalog.
//
// When the remote feed is unreachable, the initial load falls back to the
// catalog alone, while a refresh returns the error and keeps the current state
// so remote-only prices already loaded are not dropped. If the catalog cannot
// be read the remote prices are used alone and the previous routes are kept. An
// error is always returned when neither source yields data.
func (s *Service) reload(initial bool) error {
	localPricing, localByProvider, catalog, localErr := s.readCatalog()
	if localErr != nil {
		slog.Error("failed to load model catalog", "error", localErr, "file", s.catalogFile)
	}

	pricing := make(map[string]ModelPricing)
	byProvider := make(map[string]map[string]ModelPricing)
	if s.remoteURL != "" {
		remotePricing, remoteByProvider, updatedAt, err := s.fetchRemote()
		if err != nil {
			if localErr != nil || !initial {
				return err
			}
			slog.Warn("failed to fetch remote pricing, using local catalog only", "error", err)
		} else {
			pricing, byProvider = remotePricing, remoteByProvider
			slog.Info("fetched remote pricing", "models", len(pricing), "providers", len(byProvider), "updated_at", updatedAt)
		}
	} else if localErr != nil {
		return localErr
	}

	overridden := overlayCatalog(pricing, byProvider, localPricing, localByProvider)

	s.mu.Lock()
	s.pricing = pricing
	s.byProvider = byProvider
	if localErr == nil {
		s.catalog = catalog
	}
	routes := len(s.catalog)
	s.mu.Unlock()

	slog.Info("loaded pricing data", "models", len(pricing), "providers", len(byProvider), "routes", routes, "catalog_overrides", overridden)
	return nil
}

// fetchRemote downloads the remote price feed and returns it as a model-default
// price map and a per-provider price index. It does not modify the service.
func (s *Service) fetchRemote() (map[string]ModelPricing, map[string]map[string]ModelPricing, string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.remoteURL, nil)
	if err != nil {
		return nil, nil, "", fmt.Errorf("fetch remote pricing: %w", err)
	}

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, nil, "", fmt.Errorf("fetch remote pricing: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, nil, "", fmt.Errorf("fetch remote pricing: unexpected status %d", resp.StatusCode)
	}

	var remoteResp remotePricingResponse
	if err := json.NewDecoder(resp.Body).Decode(&remoteResp); err != nil {
		return nil, nil, "", fmt.Errorf("fetch remote pricing: decode response: %w", err)
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

	return pricing, byProvider, remoteResp.UpdatedAt, nil
}

// readCatalog reads and parses the local model catalog file.
func (s *Service) readCatalog() (map[string]ModelPricing, map[string]map[string]ModelPricing, map[string][]Endpoint, error) {
	data, err := os.ReadFile(s.catalogFile)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("read model catalog: %w", err)
	}

	pricing, byProvider, catalog, err := parseCatalog(data)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("parse model catalog: %w", err)
	}
	return pricing, byProvider, catalog, nil
}

// overlayCatalog writes the local catalog's prices over the given price maps in
// place, replacing any entry with the same model id (model-default map) or the
// same provider and model id (per-provider index). It returns how many existing
// entries were replaced.
func overlayCatalog(pricing map[string]ModelPricing, byProvider map[string]map[string]ModelPricing, localPricing map[string]ModelPricing, localByProvider map[string]map[string]ModelPricing) int {
	overridden := 0
	for model, mp := range localPricing {
		if _, exists := pricing[model]; exists {
			overridden++
		}
		pricing[model] = mp
	}
	for prov, models := range localByProvider {
		if byProvider[prov] == nil {
			byProvider[prov] = make(map[string]ModelPricing, len(models))
		}
		for model, mp := range models {
			if _, exists := byProvider[prov][model]; exists {
				overridden++
			}
			byProvider[prov][model] = mp
		}
	}
	return overridden
}

// parseCatalog parses model_catalog.json into the model-default price map, the
// per-provider price index, and the routing catalog. Prices come from each
// provider section's models; the flat default is populated in sorted-provider
// order (first provider that lists a model id owns the default, for stability).
// The routing catalog joins each routes entry to its provider section; entries
// referencing an unknown provider or model, or whose request_defaults set the
// model field, are skipped with a warning.
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
			if _, ok := re.RequestDefaults[routedModelField]; ok {
				slog.Warn("catalog route request_defaults must not set the model", "slug", slug, "provider", re.Provider, "model", re.Model)
				continue
			}
			catalog[slug] = append(catalog[slug], Endpoint{
				Provider:        re.Provider,
				BaseURL:         sec.BaseURL,
				ProviderModelID: re.Model,
				Region:          sec.Region,
				ZDR:             sec.ZDR,
				DataCollection:  sec.DataCollection,
				RequestDefaults: re.RequestDefaults,
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
			if err := s.reload(false); err != nil {
				slog.Warn("failed to refresh pricing", "error", err)
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
