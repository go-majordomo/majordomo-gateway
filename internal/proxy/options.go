package proxy

import "github.com/go-majordomo/majordomo-gateway/internal/secrets"

// HandlerOption configures optional behavior on a Handler. Optional dependencies
// are wired via options rather than lengthening NewHandler's positional
// signature, so the pass-through core stays constructable without them.
type HandlerOption func(*Handler)

// WithProviderRouting attaches provider routing: router selects a provider
// endpoint for a virtual model slug, keys resolves the stored (encrypted)
// credential to inject, and secrets decrypts it. All three are required together;
// without this option the handler is a pure pass-through proxy.
func WithProviderRouting(router *ProviderRouter, keys ProviderKeyResolver, secrets secrets.SecretStore) HandlerOption {
	return func(h *Handler) {
		h.providerRouter = router
		h.providerKeys = keys
		h.secretStore = secrets
	}
}
