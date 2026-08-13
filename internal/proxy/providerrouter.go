package proxy

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/go-majordomo/majordomo-gateway/internal/models"
	"github.com/go-majordomo/majordomo-gateway/internal/pricing"
)

// ErrNoUsableEndpoint is returned by Route when a model IS in the catalog but no
// candidate endpoint can be used — none has a stored credential the deployment
// can authenticate with. The handler must surface a clear error rather than fall
// through to path-detection, which would forward the slug to the wrong provider.
var ErrNoUsableEndpoint = errors.New("no usable provider endpoint for model")

// ErrNoCompliantEndpoint is returned when credentialed candidates exist but none
// satisfies the request's data-handling policy (ZDR / data_collection). Distinct
// from ErrNoUsableEndpoint so the caller can explain the failure precisely.
var ErrNoCompliantEndpoint = errors.New("no provider endpoint satisfies the data policy for model")

// DataPolicy is a per-request data-handling requirement the router hard-filters
// candidate endpoints against before cost selection — constraint-satisfaction
// first, the invariant-router posture applied to the data-policy axis.
type DataPolicy struct {
	// RequireZDR keeps only endpoints with a zero-data-retention guarantee.
	RequireZDR bool
	// RequireNoDataCollection keeps only endpoints whose data_collection == "deny".
	RequireNoDataCollection bool
}

// Health-gate + cache tuning. A candidate is gated out only once it has enough
// recent samples to trust its error rate, so a brand-new endpoint is not
// penalised for having no history.
const (
	healthWindow       = 5 * time.Minute
	healthCacheTTL     = 30 * time.Second
	healthMinSamples   = 20
	healthMaxErrorRate = 0.20
)

// CandidateSource yields the catalog endpoints that can serve a model slug.
// Satisfied by *pricing.Service.
type CandidateSource interface {
	Candidates(model string) ([]pricing.Endpoint, bool)
}

// HealthStore yields recent per-(provider, model) health aggregates.
// Satisfied by *repositories.EndpointHealthRepository.
type HealthStore interface {
	GetEndpointHealth(ctx context.Context, since time.Time) ([]models.EndpointHealth, error)
}

// KeyAvailability reports whether the gateway has a stored, usable provider key.
// Satisfied by *repositories.ProviderKeyRepository.
type KeyAvailability interface {
	HasKey(ctx context.Context, provider string) (bool, error)
}

// Pricer resolves the price for a (provider, model) pair. Satisfied by
// *pricing.Service. Pricing is the single source of truth for both selection
// (here) and cost attribution, so the router never carries its own prices.
type Pricer interface {
	Price(provider, model string) (pricing.ModelPricing, bool)
}

// RouteDecision is the chosen endpoint plus a human-readable rationale, recorded
// on the request log so the (future) feedback loop has something to attribute
// against.
type RouteDecision struct {
	Provider        string
	BaseURL         string
	ProviderModelID string
	Reason          string
}

// ProviderRouter selects a provider endpoint for a virtual model slug by
// cost, health-gated on recent request-log outcomes and hard-filtered to
// endpoints the deployment can authenticate. This is the OpenRouter-parity
// core: constraint-satisfaction (credential + health) first, cost optimization
// second — the inverse of optimize-first-and-let-output-drift.
type ProviderRouter struct {
	catalog CandidateSource
	health  HealthStore
	keys    KeyAvailability
	pricer  Pricer

	// randFloat returns a value in [0,1); injectable for deterministic tests.
	randFloat func() float64

	mu       sync.Mutex
	snapshot *healthSnapshot
}

type healthSnapshot struct {
	byKey     map[string]models.EndpointHealth
	fetchedAt time.Time
}

// NewProviderRouter constructs a ProviderRouter. Routing authenticates with the
// gateway's own stored provider keys.
func NewProviderRouter(catalogSrc CandidateSource, health HealthStore, keys KeyAvailability, pricer Pricer) *ProviderRouter {
	return &ProviderRouter{
		catalog:   catalogSrc,
		health:    health,
		keys:      keys,
		pricer:    pricer,
		randFloat: rand.Float64,
	}
}

// Route selects an endpoint for the given model slug. Returns:
//   - (nil, nil)                  when the model is not in the catalog — caller
//     falls through to today's pass-through behavior.
//   - (decision, nil)             when an endpoint was chosen. The decision is
//     always credentialed.
//   - (nil, ErrNoUsableEndpoint)  when the model is in the catalog but no
//     candidate has a usable credential.
func (r *ProviderRouter) Route(ctx context.Context, model string, policy DataPolicy) (*RouteDecision, error) {
	candidates, ok := r.catalog.Candidates(model)
	if !ok {
		return nil, nil
	}

	health := r.getHealth(ctx)

	// Hard filter 1: credential availability. Only endpoints we can authenticate
	// are eligible — a routed decision is always credentialed.
	credentialed := candidates[:0:0]
	for _, ep := range candidates {
		if r.hasCredential(ctx, ep.Provider) {
			credentialed = append(credentialed, ep)
		}
	}
	if len(credentialed) == 0 {
		return nil, fmt.Errorf("%w: %s", ErrNoUsableEndpoint, model)
	}

	// Hard filter 2: data policy. Drop endpoints that don't satisfy the request's
	// data-handling requirements. Fail-closed: an endpoint with no explicit
	// guarantee never passes a require-* filter.
	eligible := credentialed[:0:0]
	for _, ep := range credentialed {
		if policy.RequireZDR && !ep.ZDR {
			continue
		}
		if policy.RequireNoDataCollection && !strings.EqualFold(ep.DataCollection, "deny") {
			continue
		}
		eligible = append(eligible, ep)
	}
	if len(eligible) == 0 {
		return nil, fmt.Errorf("%w: %s", ErrNoCompliantEndpoint, model)
	}

	// Hard filter 3: health gate. Drop endpoints whose recent error rate exceeds
	// the threshold once they have enough samples to trust. Endpoints with little
	// or no history pass (innocent until proven unhealthy).
	healthy := eligible[:0:0]
	for _, ep := range eligible {
		if isHealthy(health, ep) {
			healthy = append(healthy, ep)
		}
	}

	degraded := false
	pool := healthy
	if len(pool) == 0 {
		// Empty feasible set: every eligible endpoint is unhealthy. Fail-open to
		// serve (better a degraded provider than a hard failure), but record it.
		pool = eligible
		degraded = true
	}

	ep := r.selectByCost(pool)
	reason := fmt.Sprintf("cost-weighted among %d healthy of %d eligible candidate(s)", len(pool), len(eligible))
	if degraded {
		reason = fmt.Sprintf("all %d eligible candidate(s) unhealthy; fell back to cheapest", len(eligible))
	}

	return &RouteDecision{
		Provider:        ep.Provider,
		BaseURL:         ep.BaseURL,
		ProviderModelID: ep.ProviderModelID,
		Reason:          reason,
	}, nil
}

// hasCredential reports whether the gateway has a stored key for provider.
func (r *ProviderRouter) hasCredential(ctx context.Context, provider string) bool {
	ok, err := r.keys.HasKey(ctx, provider)
	return err == nil && ok
}

// selectByCost picks an endpoint via inverse-price weighting (cheaper endpoints
// get proportionally more weight), OpenRouter-style. Candidates are sorted for a
// deterministic tie-break, and selection uses the injectable rand source.
func (r *ProviderRouter) selectByCost(candidates []pricing.Endpoint) pricing.Endpoint {
	sorted := make([]pricing.Endpoint, len(candidates))
	copy(sorted, candidates)
	sort.Slice(sorted, func(i, j int) bool {
		pi, pj := r.blendedPrice(sorted[i]), r.blendedPrice(sorted[j])
		if pi != pj {
			return pi < pj // cheapest first
		}
		return sorted[i].Provider < sorted[j].Provider // stable tie-break
	})

	weights := make([]float64, len(sorted))
	var total float64
	for i, ep := range sorted {
		w := weightForPrice(r.blendedPrice(ep))
		weights[i] = w
		total += w
	}
	if total <= 0 {
		return sorted[0]
	}

	target := r.randFloat() * total
	var acc float64
	for i, w := range weights {
		acc += w
		if target < acc {
			return sorted[i]
		}
	}
	return sorted[len(sorted)-1]
}

// unknownPriceSentinel is the blended price assigned to an endpoint the pricing
// service can't price — large enough to deprioritize it in cost-weighted
// selection without dropping it entirely (it still wins when it is the only
// remaining candidate).
const unknownPriceSentinel = 1e9

// blendedPrice combines input and output per-million prices (from the pricing
// service, keyed by provider+model) into a single comparable cost. Equal
// weighting is a v1 simplification; a token-mix-aware blend can replace it later.
// A missing price yields the sentinel so an unpriced endpoint is deprioritized,
// not silently excluded.
func (r *ProviderRouter) blendedPrice(ep pricing.Endpoint) float64 {
	p, ok := r.pricer.Price(ep.Provider, ep.ProviderModelID)
	if !ok {
		return unknownPriceSentinel
	}
	return p.InputPricePerMillion + p.OutputPricePerMillion
}

// weightForPrice maps a price to a selection weight: cheaper → heavier. A
// zero/unknown price is treated as maximally cheap so it is never starved.
func weightForPrice(price float64) float64 {
	if price <= 0 {
		return 1e6
	}
	return 1 / price
}

// isHealthy reports whether an endpoint passes the health gate given the current
// snapshot. Endpoints with fewer than healthMinSamples recent requests pass
// unconditionally (not enough history to condemn them).
func isHealthy(h *healthSnapshot, ep pricing.Endpoint) bool {
	stat, ok := h.byKey[healthKey(ep.Provider, ep.ProviderModelID)]
	if !ok || stat.SampleCount < healthMinSamples {
		return true
	}
	return stat.ErrorRate <= healthMaxErrorRate
}

// getHealth returns the cached health snapshot, refreshing from the store when
// stale. On a store error it serves the stale snapshot rather than failing the
// request, and an empty snapshot when there is no prior data — an empty snapshot
// simply means every endpoint passes the health gate.
func (r *ProviderRouter) getHealth(ctx context.Context) *healthSnapshot {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.snapshot != nil && time.Since(r.snapshot.fetchedAt) < healthCacheTTL {
		return r.snapshot
	}

	rows, err := r.health.GetEndpointHealth(ctx, time.Now().Add(-healthWindow))
	if err != nil {
		if r.snapshot != nil {
			return r.snapshot // stale-on-error
		}
		return &healthSnapshot{byKey: map[string]models.EndpointHealth{}, fetchedAt: time.Now()}
	}

	byKey := make(map[string]models.EndpointHealth, len(rows))
	for _, row := range rows {
		byKey[healthKey(row.Provider, row.Model)] = row
	}
	r.snapshot = &healthSnapshot{byKey: byKey, fetchedAt: time.Now()}
	return r.snapshot
}

func healthKey(provider, model string) string {
	return provider + "\x00" + model
}
