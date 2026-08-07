package proxy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-majordomo/majordomo-gateway/internal/auth"
	"github.com/go-majordomo/majordomo-gateway/internal/config"
	"github.com/go-majordomo/majordomo-gateway/internal/deprecated"
	"github.com/go-majordomo/majordomo-gateway/internal/httputil"
	"github.com/go-majordomo/majordomo-gateway/internal/models"
	"github.com/go-majordomo/majordomo-gateway/internal/pricing"
	"github.com/go-majordomo/majordomo-gateway/internal/provider"
	"github.com/go-majordomo/majordomo-gateway/internal/spanid"
	"github.com/go-majordomo/majordomo-gateway/internal/storage"
	"github.com/google/uuid"
)

// RequestLogWriter is the minimal interface proxy.Handler needs for writing logs.
type RequestLogWriter interface {
	WriteRequestLog(ctx context.Context, log *models.RequestLog)
}

type Handler struct {
	upstream         *UpstreamClient
	storage          RequestLogWriter
	pricing          *pricing.Service
	resolver         *auth.Resolver
	config           *config.Config
	providers        map[provider.Provider]string
	deprecatedModels *deprecated.Service

	// Optional request/response body archival to object storage. nil when disabled.
	bodyStore     storage.Store
	bodyKeyPrefix string
}

// ProviderKeyInfo contains hashed provider API key information.
type ProviderKeyInfo struct {
	Hash  *string
	Alias *string
}

func NewHandler(
	store RequestLogWriter,
	pricingSvc *pricing.Service,
	deprecatedSvc *deprecated.Service,
	resolver *auth.Resolver,
	cfg *config.Config,
	bodyStore storage.Store,
) *Handler {
	providers := map[provider.Provider]string{
		provider.ProviderOpenAI:    cfg.Providers.OpenAI.BaseURL,
		provider.ProviderAnthropic: cfg.Providers.Anthropic.BaseURL,
		provider.ProviderGemini:    cfg.Providers.Gemini.BaseURL,
		provider.ProviderFireworks: cfg.Providers.Fireworks.BaseURL,
		provider.ProviderTogether:  cfg.Providers.Together.BaseURL,
		provider.ProviderDeepSeek:  cfg.Providers.DeepSeek.BaseURL,
	}

	return &Handler{
		upstream:         NewUpstreamClient(cfg.Server.UpstreamTimeout, cfg.Server.StreamHeaderTimeout),
		storage:          store,
		pricing:          pricingSvc,
		deprecatedModels: deprecatedSvc,
		resolver:         resolver,
		config:           cfg,
		providers:        providers,
		bodyStore:        bodyStore,
		bodyKeyPrefix:    cfg.BodyStore.KeyPrefix,
	}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	requestedAt := time.Now()
	requestID := uuid.New()

	// Validate the gateway API key.
	apiKey := r.Header.Get("X-Majordomo-Key")
	apiKeyInfo, err := h.resolver.ResolveAPIKey(ctx, apiKey)
	if err != nil {
		slog.Debug("API key validation failed", "error", err)
		httputil.WriteJSONError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	// Extract provider API key info (for tracking, not validation). The caller's
	// provider key is relayed upstream by the upstream client; we only hash it.
	providerKeyInfo := extractProviderKeyInfo(r)

	body, err := io.ReadAll(r.Body)
	if err != nil {
		httputil.WriteJSONError(w, http.StatusBadRequest, "failed to read request body")
		return
	}

	headers := extractHeaders(r.Header)
	r.URL.Path = provider.NormalizeOpenAIPath(r.URL.Path)
	providerInfo := provider.Detect(r.URL.Path, headers)

	if providerInfo.Provider == provider.ProviderUnknown {
		httputil.WriteJSONError(w, http.StatusBadRequest, fmt.Sprintf("unrecognized request path %q (host %q); supported paths: /v1/chat/completions, /v1/completions, /v1/embeddings, /v1/responses (OpenAI; /v1 prefix optional), /v1/messages (Anthropic), /<model>:generateContent (Gemini), /model/<modelId>/converse[-stream] (Bedrock). Alternatively, set X-Majordomo-Provider header.", r.URL.Path, r.Host))
		return
	}

	if providerInfo.Provider == provider.ProviderBedrock || providerInfo.Provider == provider.ProviderBedrockMantle {
		region, ok := resolveBedrockRegion(r)
		if !ok {
			httputil.WriteJSONError(w, http.StatusBadRequest, "Bedrock requests require either an X-Majordomo-Bedrock-Region header or a Host header of the form bedrock-runtime.<region>.amazonaws.com")
			return
		}
		if providerInfo.Provider == provider.ProviderBedrockMantle {
			// Client sends bare /v1/messages; Mantle expects /anthropic/v1/messages.
			// Carry the /anthropic prefix in the BaseURL so it concatenates with the request path.
			providerInfo.BaseURL = "https://bedrock-mantle." + region + ".api.aws/anthropic"
		} else {
			providerInfo.BaseURL = "https://bedrock-runtime." + region + ".amazonaws.com"
		}
	}

	baseURL := h.providers[providerInfo.Provider]
	if baseURL == "" {
		baseURL = providerInfo.BaseURL
	}

	// Deprecated model check: if the requested model is deprecated and the API key
	// is configured to redirect or warn, substitute the replacement before forwarding.
	// Response warning headers are set now so they are present regardless of whether
	// the path is streaming or buffered (headers must be set before WriteHeader).
	upstreamBody := body
	var deprecatedModelRedirected bool
	var deprecatedOriginalModel string
	if h.deprecatedModels != nil {
		parser := provider.GetParser(providerInfo.Provider)
		requestedModel := parser.ExtractModel(body)
		if providerInfo.Provider == provider.ProviderBedrock {
			requestedModel = provider.ExtractBedrockModelFromPath(r.URL.Path)
		}
		if replacement, isDeprecated := h.deprecatedModels.Lookup(requestedModel); isDeprecated {
			switch apiKeyInfo.DeprecatedModelBehavior {
			case models.DeprecatedModelBehaviorRedirect, models.DeprecatedModelBehaviorWarn:
				overridden, err := OverrideModel(body, replacement)
				if err != nil {
					slog.Warn("failed to override deprecated model", "model", requestedModel, "replacement", replacement, "error", err)
				} else {
					upstreamBody = overridden
					deprecatedModelRedirected = true
					deprecatedOriginalModel = requestedModel
					if apiKeyInfo.DeprecatedModelBehavior == models.DeprecatedModelBehaviorWarn {
						slog.Warn("deprecated model used", "model", requestedModel, "replacement", replacement, "api_key_id", apiKeyInfo.ID)
						w.Header().Set("X-Majordomo-Deprecated-Model", requestedModel)
						w.Header().Set("X-Majordomo-Deprecated-Replacement", replacement)
					}
				}
			}
		}
	}

	// Translate request if needed (e.g., OpenAI format → Anthropic format)
	if provider.IsTranslationRequired(providerInfo.Provider) {
		translated, newPath, err := provider.TranslateOpenAIToAnthropic(body)
		if err != nil {
			slog.Warn("request translation failed, forwarding as-is", "error", err, "request_id", requestID)
		} else {
			upstreamBody = translated
			r.URL.Path = newPath
		}

		// Convert Authorization: Bearer <key> → x-api-key: <key> for Anthropic
		if authHeader := r.Header.Get("Authorization"); authHeader != "" {
			apiKey := strings.TrimPrefix(authHeader, "Bearer ")
			r.Header.Set("X-Api-Key", apiKey)
			r.Header.Del("Authorization")
			r.Header.Set("Anthropic-Version", "2023-06-01")
		}
	}

	// Decide whether to use the streaming path.
	// Translation requires the full body up-front, so always buffer those.
	useStreaming := !provider.IsTranslationRequired(providerInfo.Provider)

	var resp *UpstreamResponse
	if useStreaming {
		streamResp, err := h.upstream.ForwardStream(ctx, baseURL, r, upstreamBody)
		if err != nil {
			slog.Error("upstream request failed", "error", err, "request_id", requestID)
			httputil.WriteJSONError(w, http.StatusBadGateway, "upstream request failed")
			return
		}

		contentType := streamResp.Headers.Get("Content-Type")
		isSSE := strings.Contains(contentType, "text/event-stream")
		isEventStream := strings.Contains(contentType, "vnd.amazon.eventstream")

		if isSSE || isEventStream {
			// --- Streaming SSE path ---

			// Disable the server's write deadline for this connection so
			// long-running streams are not killed.
			rc := http.NewResponseController(w)
			if err := rc.SetWriteDeadline(time.Time{}); err != nil {
				slog.Debug("failed to clear write deadline", "error", err)
			}

			// Copy response headers (skip hop-by-hop / Content-Encoding).
			copyResponseHeaders(streamResp.Headers, w.Header())
			w.WriteHeader(streamResp.StatusCode)

			// Flush headers immediately so the client sees them.
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}

			// Tee the stream: relay to client while capturing for logging.
			var buf bytes.Buffer
			tee := io.TeeReader(streamResp.Body, &buf)

			// Stream chunks to client, flushing after each io.Copy chunk.
			flushWriter := newFlushWriter(w)
			_, copyErr := io.Copy(flushWriter, tee)
			streamResp.Body.Close()

			if copyErr != nil {
				slog.Warn("error streaming response to client", "error", copyErr, "request_id", requestID)
			}

			respondedAt := time.Now()

			// Build an UpstreamResponse from the buffered data for logging.
			resp = &UpstreamResponse{
				StatusCode:   streamResp.StatusCode,
				Headers:      streamResp.Headers,
				Body:         buf.Bytes(),
				ResponseTime: streamResp.ResponseTime,
			}

			h.logAndFinish(r, requestID, apiKeyInfo, providerKeyInfo, providerInfo, body, resp, requestedAt, respondedAt, headers, deprecatedModelRedirected, deprecatedOriginalModel)
			return
		}

		// Non-SSE response received via streaming client — buffer the rest.
		respBody, err := io.ReadAll(streamResp.Body)
		streamResp.Body.Close()
		if err != nil {
			slog.Error("failed to read upstream response", "error", err, "request_id", requestID)
			httputil.WriteJSONError(w, http.StatusBadGateway, "upstream request failed")
			return
		}

		resp = &UpstreamResponse{
			StatusCode:   streamResp.StatusCode,
			Headers:      streamResp.Headers,
			Body:         respBody,
			ResponseTime: streamResp.ResponseTime,
		}
	} else {
		// Buffered path (translation required).
		var err error
		resp, err = h.upstream.Forward(ctx, baseURL, r, upstreamBody)
		if err != nil {
			slog.Error("upstream request failed", "error", err, "request_id", requestID)
			httputil.WriteJSONError(w, http.StatusBadGateway, "upstream request failed")
			return
		}

		// Translate response back (e.g., Anthropic format → OpenAI format)
		if resp.StatusCode < 400 {
			translated, err := provider.TranslateAnthropicToOpenAI(resp.Body, "")
			if err != nil {
				slog.Warn("response translation failed, returning as-is", "error", err, "request_id", requestID)
			} else {
				resp.Body = translated
			}
		}
	}

	respondedAt := time.Now()

	// Copy response headers, filtering out hop-by-hop and Content-Encoding
	copyResponseHeaders(resp.Headers, w.Header())

	// Check if we should compress the response for the client.
	// Skip compression for SSE — it defeats streaming (already handled above).
	acceptEncoding := r.Header.Get("Accept-Encoding")
	contentType := resp.Headers.Get("Content-Type")
	responseBody := resp.Body

	if !strings.Contains(contentType, "text/event-stream") && ShouldCompress(acceptEncoding, contentType, len(resp.Body)) {
		compressed, err := GzipCompress(resp.Body)
		if err != nil {
			slog.Warn("failed to compress response, sending uncompressed", "error", err, "request_id", requestID)
		} else {
			responseBody = compressed
			w.Header().Set("Content-Encoding", "gzip")
			w.Header().Set("Vary", "Accept-Encoding")
		}
	}

	w.WriteHeader(resp.StatusCode)
	w.Write(responseBody)

	h.logAndFinish(r, requestID, apiKeyInfo, providerKeyInfo, providerInfo, body, resp, requestedAt, respondedAt, headers, deprecatedModelRedirected, deprecatedOriginalModel)
}

// logAndFinish extracts session metadata from request headers and dispatches
// the async log. Shared by both the buffered and streaming paths.
func (h *Handler) logAndFinish(
	r *http.Request,
	requestID uuid.UUID,
	apiKeyInfo *models.APIKeyInfo,
	providerKeyInfo *ProviderKeyInfo,
	providerInfo provider.ProviderInfo,
	reqBody []byte,
	resp *UpstreamResponse,
	requestedAt, respondedAt time.Time,
	headers map[string]string,
	deprecatedModelRedirected bool,
	deprecatedOriginalModel string,
) {
	go h.logRequest(context.Background(), requestID, apiKeyInfo, providerKeyInfo, providerInfo, r, reqBody, resp, requestedAt, respondedAt, headers, deprecatedModelRedirected, deprecatedOriginalModel)
}

func (h *Handler) logRequest(
	ctx context.Context,
	requestID uuid.UUID,
	apiKeyInfo *models.APIKeyInfo,
	providerKeyInfo *ProviderKeyInfo,
	providerInfo provider.ProviderInfo,
	req *http.Request,
	reqBody []byte,
	resp *UpstreamResponse,
	requestedAt, respondedAt time.Time,
	customHeaders map[string]string,
	deprecatedModelRedirected bool,
	deprecatedOriginalModel string,
) {
	parser := provider.GetParser(providerInfo.Provider)
	metrics, err := parser.ParseResponse(resp.Body)
	if err != nil {
		slog.Warn("failed to parse response", "error", err, "request_id", requestID)
		metrics = &models.UsageMetrics{
			Provider: string(providerInfo.Provider),
			Model:    parser.ExtractModel(reqBody),
		}
	}

	// Fall back to request model if response doesn't include it
	if metrics.Model == "" {
		metrics.Model = parser.ExtractModel(reqBody)
	}
	if metrics.Model == "" && providerInfo.Provider == provider.ProviderBedrock {
		metrics.Model = provider.ExtractBedrockModelFromPath(req.URL.Path)
	}

	metrics.ResponseTime = resp.ResponseTime

	cost := h.pricing.Calculate(metrics)

	var errMsg *string
	if resp.StatusCode >= 400 {
		msg := string(resp.Body)
		if len(msg) > 500 {
			msg = msg[:500]
		}
		errMsg = &msg
	}

	log := &models.RequestLog{
		ID: requestID,

		// Gateway API key (validated)
		MajordomoAPIKeyID: &apiKeyInfo.ID,

		// Provider API key (relayed; hash kept for usage tracking)
		ProviderAPIKeyHash:  providerKeyInfo.Hash,
		ProviderAPIKeyAlias: providerKeyInfo.Alias,

		// Attribute the request to the provider it was ROUTED to (from path/header
		// detection), not the response format. OpenAI-compatible providers
		// (Fireworks, Together, DeepSeek) all parse with the OpenAI parser, which
		// would otherwise mislabel them as "openai".
		Provider:      string(providerInfo.Provider),
		Model:         metrics.Model,
		RequestPath:   req.URL.Path,
		RequestMethod: req.Method,

		RequestedAt:    requestedAt,
		RespondedAt:    respondedAt,
		ResponseTimeMs: resp.ResponseTime.Milliseconds(),

		InputTokens:           metrics.InputTokens,
		OutputTokens:          metrics.OutputTokens,
		CachedTokens:          metrics.CachedTokens,
		CacheCreationTokens:   metrics.CacheCreationTokens,
		CacheCreation5mTokens: metrics.CacheCreation5mTokens,
		CacheCreation1hTokens: metrics.CacheCreation1hTokens,

		InputCost:         cost.InputCost,
		CachedCost:        cost.CachedCost,
		CacheCreationCost: cost.CacheCreationCost,
		OutputCost:        cost.OutputCost,
		TotalCost:         cost.TotalCost,

		StatusCode:   resp.StatusCode,
		ErrorMessage: errMsg,

		RawMetadata:     extractCustomMetadata(customHeaders),
		ModelAliasFound: cost.ModelAliasFound,
	}

	// Attach agent-run call-tree identity when the request carries a trace id.
	applyRunTracing(log, requestID, customHeaders)

	// Attach deprecated model redirect info. OriginalModel records what the client
	// actually requested before the gateway substituted the replacement.
	if deprecatedModelRedirected {
		log.DeprecatedModelRedirected = true
		log.OriginalModel = &deprecatedOriginalModel
	}

	// Optionally archive request/response bodies to object storage. Runs inside this
	// background goroutine, so it never adds latency to the proxied request.
	h.archiveBodies(ctx, log, apiKeyInfo.ID, requestID, requestedAt, req, customHeaders, reqBody, resp)

	h.storage.WriteRequestLog(ctx, log)
}

// BedrockRegionHeader carries an explicit AWS region for Bedrock requests when
// the Host header is unavailable (e.g. behind a fixed-Host ingress gateway).
const BedrockRegionHeader = "X-Majordomo-Bedrock-Region"

// resolveBedrockRegion determines the AWS region for a Bedrock request.
// The X-Majordomo-Bedrock-Region header takes precedence; otherwise the region
// is parsed from the Host header. Returns (region, true) on success.
func resolveBedrockRegion(r *http.Request) (string, bool) {
	if v := r.Header.Get(BedrockRegionHeader); v != "" {
		if isValidAWSRegion(v) {
			return v, true
		}
		return "", false
	}
	return parseBedrockRegionFromHost(r.Host)
}

// parseBedrockRegionFromHost extracts the AWS region from a Host header of the
// form bedrock-runtime.<region>.amazonaws.com. The port suffix, if any, is
// stripped. Returns (region, true) on a valid match; ("", false) otherwise.
func parseBedrockRegionFromHost(host string) (string, bool) {
	if i := strings.IndexByte(host, ':'); i >= 0 {
		host = host[:i]
	}
	const prefix = "bedrock-runtime."
	const suffix = ".amazonaws.com"
	if !strings.HasPrefix(host, prefix) || !strings.HasSuffix(host, suffix) {
		return "", false
	}
	region := host[len(prefix) : len(host)-len(suffix)]
	if !isValidAWSRegion(region) {
		return "", false
	}
	return region, true
}

// isValidAWSRegion reports whether s is a syntactically valid AWS region
// (non-empty, lowercase a-z / 0-9 / hyphen only).
func isValidAWSRegion(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-') {
			return false
		}
	}
	return true
}

// extractProviderKeyInfo extracts and hashes the provider API key from the Authorization header.
func extractProviderKeyInfo(r *http.Request) *ProviderKeyInfo {
	info := &ProviderKeyInfo{}

	// Hash the Authorization header if present
	authHeader := r.Header.Get("Authorization")
	if authHeader != "" {
		hash := auth.HashAPIKey(authHeader)
		info.Hash = &hash
	}

	// Get optional provider alias header
	if alias := r.Header.Get("X-Majordomo-Provider-Alias"); alias != "" {
		info.Alias = &alias
	}

	return info
}

func extractHeaders(h http.Header) map[string]string {
	result := make(map[string]string)
	for key, values := range h {
		lowerKey := strings.ToLower(key)
		if strings.HasPrefix(lowerKey, "x-majordomo") {
			result[lowerKey] = values[0]
		}
	}
	return result
}

// Reserved x-majordomo-* headers (lowercased) that carry dedicated behavior and
// must not leak into free-form request metadata.
const (
	traceIDHeader  = "x-majordomo-trace-id"
	spanPathHeader = "x-majordomo-span-path"
	spanNameHeader = "x-majordomo-span-name"
)

var reservedMajordomoHeaders = map[string]bool{
	"x-majordomo-key":            true,
	"x-majordomo-provider":       true,
	"x-majordomo-provider-alias": true,
	"x-majordomo-client":         true,
	traceIDHeader:                true,
	spanPathHeader:               true,
	spanNameHeader:               true,
}

func extractCustomMetadata(headers map[string]string) map[string]string {
	metadata := make(map[string]string)
	for key, value := range headers {
		if reservedMajordomoHeaders[key] {
			continue
		}
		cleanKey := strings.TrimPrefix(key, "x-majordomo-")
		metadata[cleanKey] = value
	}
	return metadata
}

// applyRunTracing populates the agent-run call-tree fields on log from the request's
// custom headers. A request participates in a run only when it carries a trace id;
// without one the fields stay nil and the request is a standalone log entry.
//
// span_id is the leaf's own id (the request id); parent_span_id is the deterministic
// id of the interior step named by span_path, so a later Tier 2 tool-span ingest can
// attach real tool nodes to the same identity graph. log.Model must already be set,
// as it is the default span name.
func applyRunTracing(log *models.RequestLog, requestID uuid.UUID, headers map[string]string) {
	traceID := headers[traceIDHeader]
	if traceID == "" {
		return
	}

	canonicalPath := spanid.CanonicalPath(headers[spanPathHeader])
	spanName := headers[spanNameHeader]
	if spanName == "" {
		spanName = log.Model
	}
	spanID := requestID
	parentSpanID := spanid.InteriorSpanID(traceID, canonicalPath)

	log.TraceID = &traceID
	log.SpanPath = &canonicalPath
	log.SpanName = &spanName
	log.SpanID = &spanID
	log.ParentSpanID = &parentSpanID
}

// archiveBodies uploads the request/response bodies to object storage (when enabled)
// and records the object key on the log. A failure is logged but never blocks the
// usage record from being written.
func (h *Handler) archiveBodies(ctx context.Context, log *models.RequestLog, apiKeyID, requestID uuid.UUID, requestedAt time.Time, req *http.Request, customHeaders map[string]string, reqBody []byte, resp *UpstreamResponse) {
	if h.bodyStore == nil {
		return
	}
	key := storage.GenerateKey(h.bodyKeyPrefix, apiKeyID, requestID, requestedAt)
	payload := &storage.BodyPayload{
		RequestID:       requestID,
		Timestamp:       requestedAt,
		RequestMethod:   req.Method,
		RequestPath:     req.URL.Path,
		RequestHeaders:  extractCustomMetadata(customHeaders),
		RequestBody:     reqBody,
		ResponseStatus:  resp.StatusCode,
		ResponseHeaders: storage.ExtractResponseHeaders(resp.Headers),
		ResponseBody:    resp.Body,
	}
	if err := h.bodyStore.Upload(ctx, key, payload); err != nil {
		slog.Warn("failed to archive request bodies", "error", err, "request_id", requestID)
		return
	}
	log.BodyS3Key = &key
}

// flushWriter wraps an http.ResponseWriter and flushes after every Write
// if the underlying writer supports http.Flusher. This ensures SSE chunks
// are delivered to the client immediately.
type flushWriter struct {
	w       http.ResponseWriter
	flusher http.Flusher
}

func newFlushWriter(w http.ResponseWriter) *flushWriter {
	f, _ := w.(http.Flusher)
	return &flushWriter{w: w, flusher: f}
}

func (fw *flushWriter) Write(p []byte) (int, error) {
	n, err := fw.w.Write(p)
	if fw.flusher != nil {
		fw.flusher.Flush()
	}
	return n, err
}
