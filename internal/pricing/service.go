package pricing

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"regexp"
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

type Service struct {
	remoteURL       string
	fallbackFile    string
	aliasesFile     string
	refreshInterval time.Duration

	mu      sync.RWMutex
	pricing map[string]ModelPricing
	aliases map[string]string // maps API model name -> pricing model name

	httpClient *http.Client
	done       chan struct{}
}

func NewService(remoteURL, fallbackFile, aliasesFile string, refreshInterval time.Duration) *Service {
	s := &Service{
		remoteURL:       remoteURL,
		fallbackFile:    fallbackFile,
		aliasesFile:     aliasesFile,
		refreshInterval: refreshInterval,
		pricing:         make(map[string]ModelPricing),
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
			slog.Warn("failed to fetch remote pricing, using fallback", "error", err)
			s.loadFallback()
			return
		}
		// Merge fallback entries that are missing from the remote data,
		// so pricing.json acts as a supplement for models not yet in the
		// remote source.
		s.mergeFallback()
	} else {
		s.loadFallback()
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
	for _, entry := range remoteResp.Prices {
		cachedPrice := 0.0
		if entry.InputCached != nil {
			cachedPrice = *entry.InputCached
		}
		pricing[entry.ID] = ModelPricing{
			InputPricePerMillion:  entry.Input,
			OutputPricePerMillion: entry.Output,
			CachedPricePerMillion: cachedPrice,
		}
	}

	s.mu.Lock()
	s.pricing = pricing
	s.mu.Unlock()

	slog.Info("loaded pricing data from remote", "models", len(pricing), "updated_at", remoteResp.UpdatedAt)
	return nil
}

func (s *Service) loadFallback() {
	data, err := os.ReadFile(s.fallbackFile)
	if err != nil {
		slog.Error("failed to load fallback pricing", "error", err)
		return
	}

	var fallbackData map[string]fallbackPriceEntry
	if err := json.Unmarshal(data, &fallbackData); err != nil {
		slog.Error("failed to parse fallback pricing", "error", err)
		return
	}

	pricing := make(map[string]ModelPricing)
	for model, entry := range fallbackData {
		pricing[model] = ModelPricing{
			InputPricePerMillion:  entry.InputPricePerMillion,
			OutputPricePerMillion: entry.OutputPricePerMillion,
			CachedPricePerMillion: entry.CachedPricePerMillion,
		}
	}

	s.mu.Lock()
	s.pricing = pricing
	s.mu.Unlock()

	slog.Info("loaded pricing data from fallback", "models", len(pricing))
}

func (s *Service) mergeFallback() {
	data, err := os.ReadFile(s.fallbackFile)
	if err != nil {
		return
	}

	var fallbackData map[string]fallbackPriceEntry
	if err := json.Unmarshal(data, &fallbackData); err != nil {
		return
	}

	s.mu.Lock()
	added := 0
	for model, entry := range fallbackData {
		if _, exists := s.pricing[model]; !exists {
			s.pricing[model] = ModelPricing{
				InputPricePerMillion:  entry.InputPricePerMillion,
				OutputPricePerMillion: entry.OutputPricePerMillion,
				CachedPricePerMillion: entry.CachedPricePerMillion,
			}
			added++
		}
	}
	s.mu.Unlock()

	if added > 0 {
		slog.Info("merged fallback pricing for models missing from remote", "count", added)
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
				s.mergeFallback()
			}
		case <-s.done:
			return
		}
	}
}

// resolve maps a provider-returned model id to its pricing entry: exact match,
// then the alias table, then both of those again after stripping a trailing
// date suffix (so newly released dated model ids resolve to their canonical
// entry without a hand-maintained alias). Caller must hold s.mu.
func (s *Service) resolve(model string) (ModelPricing, bool) {
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

func (s *Service) Calculate(metrics *models.UsageMetrics) models.Cost {
	s.mu.RLock()
	pricing, ok := s.resolve(metrics.Model)
	s.mu.RUnlock()

	if !ok {
		slog.Warn("no pricing found for model", "model", metrics.Model)
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
