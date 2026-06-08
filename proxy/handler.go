package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"tokomoco/cache"
	"tokomoco/config"
	"tokomoco/dashboard"
	"tokomoco/detector"
	"tokomoco/injector"
	"tokomoco/memory"
	"tokomoco/metrics"
	"tokomoco/nemoguard"
	"tokomoco/providers"
	"tokomoco/redactor"
	"tokomoco/reliability"
	"tokomoco/rules"
	"tokomoco/store"
	"tokomoco/tracker"

	"github.com/google/uuid"
	"github.com/gorilla/mux"
)

const (
	OpenAIBaseURL    = "https://api.openai.com"
	AnthropicBaseURL = "https://api.anthropic.com"
	GeminiBaseURL    = "https://generativelanguage.googleapis.com"

	promptPreviewLen = 120

	// maxPersistGoroutines caps the number of concurrent goroutines used
	// for DB persistence + WebSocket broadcast after each request.
	// Prevents unbounded goroutine spawning under burst traffic.
	maxPersistGoroutines = 64
)

// modelAliases maps short proxy model IDs to valid API model identifiers.
// When a rule overrides the model using a short name, we resolve it here so
// the upstream provider receives a recognised model ID.
var modelAliases = map[string]string{
	// Anthropic Claude 4.x  (source: https://platform.claude.com/docs/en/about-claude/models/overview)
	// claude-opus-4-6 is already a valid alias accepted by the API
	"claude-opus-4-5":   "claude-opus-4-5-20251101",
	"claude-opus-4-1":   "claude-opus-4-1-20250805",
	"claude-opus-4":     "claude-opus-4-20250514",
	"claude-sonnet-4-5": "claude-sonnet-4-5-20250929",
	"claude-sonnet-4":   "claude-sonnet-4-20250514",
	"claude-haiku-4-5":  "claude-haiku-4-5-20251001",
	// Note: there is no "claude-haiku-4" model — only haiku-4-5 and 3-haiku exist.
	// Anthropic Claude 3.x
	"claude-sonnet-3-7": "claude-3-7-sonnet-20250219",
	"claude-3-5-sonnet": "claude-3-5-sonnet-20241022",
	"claude-3-5-haiku":  "claude-3-5-haiku-20241022",
	"claude-3-opus":     "claude-3-opus-20240229",
	"claude-3-haiku":    "claude-3-haiku-20240307",
}

// resolveModelAlias returns the full API model ID for a short alias.
// If the model is already a full ID (or unknown), it is returned unchanged.
func resolveModelAlias(model string) string {
	if full, ok := modelAliases[model]; ok {
		return full
	}
	return model
}

// Handler handles proxy requests
type Handler struct {
	sessionTracker *tracker.SessionTracker
	loopDetector   *detector.LoopDetector
	tokenCounter   *tracker.TokenCounter
	wsHub          *dashboard.Hub
	db             *store.DB
	cfg            *config.Config
	injCfg         injector.InjectionConfig
	httpClient     *http.Client
	rulesEngine    *rules.Engine              // optional; nil if rules are disabled
	fallbackStore  *reliability.FallbackStore // optional; nil if fallback configs not used
	responseCache  *cache.ResponseCache       // optional; nil if cache is disabled
	semanticCache  *cache.SemanticCache       // optional; nil if semantic cache is disabled
	memoryStore    *memory.Store              // optional; nil if memory layer is disabled
	providerStore  *providers.ProviderStore   // optional; nil if no custom providers configured
	nemoGuard      *nemoguard.Detector        // optional; nil if NeMo Guard jailbreak detection disabled
	persistSem     chan struct{}              // semaphore to cap concurrent persist goroutines
	scMu           sync.RWMutex               // protects semanticCache hot-swap
}

// NewHandler creates a new proxy handler.
// cfg must be a pointer so runtime settings changes (via the settings API) take effect immediately.
func NewHandler(st *tracker.SessionTracker, ld *detector.LoopDetector, hub *dashboard.Hub, db *store.DB, cfg *config.Config, re *rules.Engine, fs *reliability.FallbackStore, rc *cache.ResponseCache, sc *cache.SemanticCache, ms *memory.Store, ps *providers.ProviderStore, ng *nemoguard.Detector) *Handler {
	tc, _ := tracker.NewTokenCounter()

	// Map injection mode from config string to injector constant
	var mode injector.InjectionMode
	switch cfg.InjectionMode {
	case "content":
		mode = injector.ModeContent
	case "hybrid":
		mode = injector.ModeHybrid
	default:
		mode = injector.ModeMetadata
	}

	return &Handler{
		sessionTracker: st,
		loopDetector:   ld,
		tokenCounter:   tc,
		wsHub:          hub,
		db:             db,
		cfg:            cfg,
		injCfg: injector.InjectionConfig{
			Mode:             mode,
			ContentThreshold: cfg.ContentThreshold,
			InjectMetadata:   true,
			InjectContent:    mode == injector.ModeContent,
		},
		// Single shared HTTP client — connection pool is reused across requests.
		httpClient:    &http.Client{Timeout: cfg.UpstreamTimeout()},
		rulesEngine:   re, // nil if rules are disabled
		fallbackStore: fs, // nil if fallback configs not used
		responseCache: rc, // nil if cache is disabled
		semanticCache: sc, // nil if semantic cache is disabled
		memoryStore:   ms, // nil if memory is disabled
		providerStore: ps, // nil if no custom providers
		nemoGuard:     ng, // nil if NeMo Guard jailbreak detection disabled
		persistSem:    make(chan struct{}, maxPersistGoroutines),
	}
}

// SetSemanticCache hot-swaps the semantic cache at runtime (e.g. when
// embedding settings change via the dashboard). Thread-safe.
func (h *Handler) SetSemanticCache(sc *cache.SemanticCache) {
	h.scMu.Lock()
	h.semanticCache = sc
	h.scMu.Unlock()
}

// getSemanticCache returns the current semantic cache (thread-safe read).
func (h *Handler) getSemanticCache() *cache.SemanticCache {
	h.scMu.RLock()
	sc := h.semanticCache
	h.scMu.RUnlock()
	return sc
}

// currentRetryConfig builds a RetryConfig from the current live settings.
// Called per-request so settings API changes apply immediately.
func (h *Handler) currentRetryConfig() reliability.RetryConfig {
	return reliability.NewRetryConfig(
		h.cfg.RetryEnabled,
		h.cfg.RetryMaxAttempts,
		h.cfg.RetryInitialDelay,
		h.cfg.RetryMaxDelay,
	)
}

// HandleOpenAI handles OpenAI requests
func (h *Handler) HandleOpenAI(w http.ResponseWriter, r *http.Request) {
	h.handleRequest(w, r, "openai", OpenAIBaseURL+"/v1/chat/completions")
}

// HandleAnthropic handles Anthropic requests
func (h *Handler) HandleAnthropic(w http.ResponseWriter, r *http.Request) {
	h.handleRequest(w, r, "anthropic", AnthropicBaseURL+"/v1/messages")
}

// HandleGemini handles Gemini requests
func (h *Handler) HandleGemini(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	model := vars["model"]

	const maxBodySize = 10 << 20 // 10 MB
	bodyBytes, err := io.ReadAll(io.LimitReader(r.Body, maxBodySize+1))
	if err != nil {
		http.Error(w, "Failed to read request", http.StatusBadRequest)
		return
	}
	if len(bodyBytes) > maxBodySize {
		http.Error(w, "Request body too large (max 10 MB)", http.StatusRequestEntityTooLarge)
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(bodyBytes))

	endpoint := "/v1beta/models/" + model + ":generateContent"
	if strings.Contains(r.URL.Path, "streamGenerateContent") {
		endpoint = "/v1beta/models/" + model + ":streamGenerateContent?alt=sse"
	}

	h.handleRequest(w, r, "gemini", GeminiBaseURL+endpoint)
}

// executeUpstreamRequest performs the HTTP request with retry and optional fallback.
// Returns the response, status code, error, retry stats, and (if fallback was used)
// the fallback provider/model names and the matched fallback config ID.
func (h *Handler) executeUpstreamRequest(
	agentID, provider, model, upstreamURL string,
	bodyBytes []byte,
	headers http.Header,
) (*http.Response, int, error, reliability.RetryResult, string, string, int64) {

	// doRequest is the shared helper that builds and fires one HTTP request.
	doRequest := func(url string, body []byte, hdrs http.Header) (*http.Response, error) {
		req, err := http.NewRequest("POST", url, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		for key, values := range hdrs {
			for _, value := range values {
				req.Header.Add(key, value)
			}
		}
		req.ContentLength = int64(len(body))
		req.Header.Set("Content-Length", strconv.Itoa(len(body)))
		return h.httpClient.Do(req)
	}

	// ── Retry loop ──────────────────────────────────────────────────────────
	// We capture the *http.Response via a shared variable so we can return it
	// to the caller after the retry loop completes.
	var lastResp *http.Response

	operation := func() (int, error) {
		resp, err := doRequest(upstreamURL, bodyBytes, headers)
		if err != nil {
			return 0, err
		}
		// If the status code is retryable, close this response and signal retry
		if reliability.IsRetryableError(resp.StatusCode, nil) {
			resp.Body.Close()
			lastResp = nil
			return resp.StatusCode, fmt.Errorf("retryable error: status %d", resp.StatusCode)
		}
		// Success or non-retryable error — keep the response body open
		lastResp = resp
		return resp.StatusCode, nil
	}

	logPrefix := fmt.Sprintf("%s/%s", provider, model)
	_, retryErr, retryResult := reliability.RetryWithBackoff(h.currentRetryConfig(), operation, logPrefix)

	// If the retry loop succeeded, return the captured response directly
	if retryErr == nil && lastResp != nil {
		return lastResp, lastResp.StatusCode, nil, retryResult, "", "", 0
	}

	// ── Fallback (only when all retries are exhausted) ──────────────────────
	if retryErr != nil && h.cfg.FallbackEnabled {
		log.Printf("[FALLBACK] All retries exhausted for %s/%s. Attempting fallback...", provider, model)

		fallbackProvider, fallbackModel, fallbackConfigID, fbErr := reliability.SelectFallback(
			agentID, provider, model,
			reliability.FallbackStrategy(h.cfg.FallbackStrategy),
			h.fallbackStore,
		)
		if fbErr == nil {
			// Rewrite the model in the body for the fallback provider
			var reqData map[string]interface{}
			if err := json.Unmarshal(bodyBytes, &reqData); err != nil {
				log.Printf("[FALLBACK] failed to parse request body: %v", err)
			} else {
				reqData["model"] = fallbackModel
			}
			fallbackBody, _ := json.Marshal(reqData)

			// Determine fallback URL
			var fallbackURL string
			switch fallbackProvider {
			case "openai":
				fallbackURL = OpenAIBaseURL + "/v1/chat/completions"
			case "anthropic":
				fallbackURL = AnthropicBaseURL + "/v1/messages"
			case "google", "gemini":
				fallbackURL = GeminiBaseURL + "/v1beta/models/" + fallbackModel + ":generateContent"
			default:
				// Check custom providers for the fallback target
				if h.providerStore != nil {
					if cp := h.providerStore.Lookup(fallbackProvider); cp != nil {
						fallbackURL = cp.UpstreamURL()
					}
				}
			}

			if fallbackURL == "" {
				log.Printf("[FALLBACK] skipping — could not resolve URL for provider %q", fallbackProvider)
			} else {
				resp, err := doRequest(fallbackURL, fallbackBody, headers)
				if err == nil {
					log.Printf("[FALLBACK] Success: %s", reliability.FormatFallbackInfo(provider, model, fallbackProvider, fallbackModel))
					return resp, resp.StatusCode, nil, retryResult, fallbackProvider, fallbackModel, fallbackConfigID
				}
				log.Printf("[FALLBACK] Fallback request also failed: %v", err)
			}
		}
	}

	// Everything failed — return the last error
	return nil, 0, retryErr, retryResult, "", "", 0
}

// effectiveFormat returns the API format to use for response parsing.
// For built-in providers it maps provider name to format; for custom providers
// it uses the configured api_format. This allows format-aware branching
// without hardcoding provider names in every parser function.
func effectiveFormat(provider string, cp *providers.CustomProvider) string {
	if cp != nil {
		return cp.APIFormat
	}
	switch provider {
	case "anthropic":
		return "anthropic"
	case "gemini":
		return "gemini"
	default:
		return "openai"
	}
}

// handleRequest is the core proxy logic
func (h *Handler) handleRequest(w http.ResponseWriter, r *http.Request, provider, upstreamURL string) {
	// ── Read & parse request body (capped at 10 MB to prevent OOM) ──────────
	const maxBodySize = 10 << 20 // 10 MB
	bodyBytes, err := io.ReadAll(io.LimitReader(r.Body, maxBodySize+1))
	if err != nil {
		http.Error(w, "Failed to read request", http.StatusBadRequest)
		return
	}
	defer r.Body.Close()
	if len(bodyBytes) > maxBodySize {
		http.Error(w, "Request body too large (max 10 MB)", http.StatusRequestEntityTooLarge)
		return
	}

	var reqData map[string]interface{}
	if err := json.Unmarshal(bodyBytes, &reqData); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	// ── Session & agent identity ────────────────────────────────────────────
	sessionID := r.Header.Get("X-Session-ID")
	session := h.sessionTracker.GetOrCreateSession(sessionID)

	// Priority: X-Agent-ID > X-App-Name > parsed User-Agent > "unknown"
	agentID := r.Header.Get("X-Agent-ID")
	appName := r.Header.Get("X-App-Name")
	if agentID == "" {
		agentID = extractAgentFromUserAgent(r.Header.Get("User-Agent"))
	}
	if appName == "" {
		appName = agentID
	}

	// ── Model + token counting ──────────────────────────────────────────────
	model, _ := reqData["model"].(string)
	if model == "" {
		model = "gpt-4"
	}

	// ── Custom provider prefix routing ─────────────────────────────────────
	// If model contains "/" (e.g. "ollama/llama3.2"), check if the prefix matches
	// a registered custom provider. Only intercept if the prefix is a known provider;
	// models with slashes that don't match (e.g. "mistralai/mistral-7b" from OpenRouter)
	// pass through untouched.
	var customProvider *providers.CustomProvider
	if h.providerStore != nil {
		if idx := strings.IndexByte(model, '/'); idx > 0 {
			prefix := model[:idx]
			if cp := h.providerStore.Lookup(prefix); cp != nil {
				customProvider = cp
				provider = cp.Name
				model = model[idx+1:]
				reqData["model"] = model
				if b, err := json.Marshal(reqData); err == nil {
					bodyBytes = b
				}
				upstreamURL = cp.UpstreamURL()
				log.Printf("[CUSTOM] routing %s/%s → %s (format=%s)", cp.Name, model, upstreamURL, cp.APIFormat)
			}
		}
	}

	// Determine API format for response parsing — custom providers declare theirs;
	// built-in providers map from name. Used everywhere below instead of raw provider checks.
	apiFormat := effectiveFormat(provider, customProvider)

	// ── Model validation ───────────────────────────────────────────────────
	// Reject unrecognised models early instead of forwarding a doomed request
	// to the upstream provider. A model is recognised if:
	//   1. It matched a custom provider (customProvider != nil), or
	//   2. It's in the model alias map, or
	//   3. It has an exact/prefix match in the pricing system.
	if customProvider == nil {
		if _, isAlias := modelAliases[model]; !isAlias && !tracker.IsKnownModel(model) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			errMsg := fmt.Sprintf("Unknown model %q. This model is not registered with any provider. ", model)
			if strings.Contains(model, "/") {
				prefix := model[:strings.IndexByte(model, '/')]
				errMsg += fmt.Sprintf("If this is a custom/self-hosted model, register a provider named %q in Settings → Custom Providers, then send the request as %s.", prefix, model)
			} else {
				errMsg += "Check the model name for typos, or if this is a custom/self-hosted model, register a custom provider in Settings → Custom Providers and prefix the model name (e.g., provider-name/model-name)."
			}
			json.NewEncoder(w).Encode(map[string]interface{}{
				"error": map[string]interface{}{
					"message": errMsg,
					"type":    "invalid_request_error",
					"code":    "model_not_found",
				},
			})
			log.Printf("[PROXY] rejected unknown model %q from %s", model, agentID)
			return
		}
	}

	// Prepare upstream headers — clone client headers, strip hop-by-hop and proxy-specific
	// headers, then configure auth for custom providers.
	upstreamHeaders := r.Header.Clone()

	// Save client's x-api-key before stripping — needed for built-in providers (Anthropic).
	clientAPIKey := r.Header.Get("X-Api-Key")

	// Strip hop-by-hop headers that must not be forwarded to upstream providers.
	// Also strip Accept-Encoding so the upstream always returns uncompressed JSON —
	// the proxy needs to parse the response body for usage/token counting.
	for _, h := range []string{"Connection", "Keep-Alive", "Proxy-Authenticate",
		"Proxy-Authorization", "Te", "Trailers", "Transfer-Encoding", "Upgrade",
		"X-Proxy-Key", "X-API-Key", "X-Session-ID", "X-Agent-ID", "X-App-Name",
		"X-Cache-Control", "Host", "Accept-Encoding"} {
		upstreamHeaders.Del(h)
	}

	if customProvider != nil {
		if auth := customProvider.ResolveAuthHeader(); auth != "" {
			upstreamHeaders.Set("Authorization", auth)
		} else {
			// No auth configured (e.g., local Ollama) — remove any client auth
			upstreamHeaders.Del("Authorization")
		}
	} else if clientAPIKey != "" {
		// Built-in provider (Anthropic) — restore the client's x-api-key for upstream auth.
		upstreamHeaders.Set("X-Api-Key", clientAPIKey)
	}

	messages, _ := tracker.ExtractMessagesFromRequest(bodyBytes, apiFormat)
	var inputTokens int
	if apiFormat == "openai" {
		inputTokens, _ = h.tokenCounter.CountOpenAIMessages(messages, model)
	} else {
		inputTokens, _ = h.tokenCounter.CountAnthropicMessages(messages, model)
	}

	prompt := extractPrompt(messages)
	promptPreview := truncate(prompt, promptPreviewLen)

	// ── Prompt version tracking (before any injection modifies the system prompt) ──
	if h.db != nil && agentID != "" {
		if sysPrompt := extractSystemPrompt(reqData, apiFormat); sysPrompt != "" {
			go func() {
				defer func() {
					if r := recover(); r != nil {
						log.Printf("[PROMPT-VERSION] panic: %v", r)
					}
				}()
				id, isNew, err := h.db.RecordPromptVersion(agentID, appName, sysPrompt, provider, model)
				if err != nil {
					log.Printf("[PROMPT-VERSION] error recording: %v", err)
				} else if isNew {
					log.Printf("[PROMPT-VERSION] new version #%d for agent=%s hash=%s", id, agentID, store.HashSystemPrompt(sysPrompt)[:12])
				}
			}()
		}
	}

	// ── Loop detection ──────────────────────────────────────────────────────
	loopResult := h.loopDetector.DetectLoop(prompt, session.ID)

	// ── Rules evaluation ────────────────────────────────────────────────────
	var matchedRuleID int64
	estimatedCost := tracker.CalculateCost(model, inputTokens, 0, 0)

	// ── Jailbreak detection (NeMo Guard) ────────────────────────────────────
	// Runs once when enabled (CONFIG_NEMOGUARD_URL set). The verdict feeds the
	// rules engine (CondJailbreak) and the default auto-block below. Fail-open:
	// a detector error/timeout never blocks the request.
	var jbDetected bool
	var jbScore float64
	var jbCategory string
	if h.nemoGuard != nil {
		jbCtx, cancel := context.WithTimeout(r.Context(), h.cfg.NeMoGuardTimeout())
		res, jbErr := h.nemoGuard.Classify(jbCtx, extractConversationText(reqData, messages, apiFormat))
		cancel()
		if jbErr != nil {
			log.Printf("[NEMOGUARD] check failed (allowing): %v", jbErr)
		} else if res.Jailbreak {
			jbDetected, jbScore, jbCategory = true, res.Score, "nemoguard"
		}
	}

	if h.rulesEngine != nil {
		rctx := &rules.RuleContext{
			AgentID:           agentID,
			AppName:           appName,
			Provider:          provider,
			Model:             model,
			Session:           session,
			InputTokens:       inputTokens,
			Cost:              estimatedCost,
			LoopResult:        loopResult,
			PromptPreview:     promptPreview,
			RawMessages:       messages,
			JailbreakDetected: jbDetected,
			JailbreakScore:    jbScore,
		}
		result, matched := h.rulesEngine.Evaluate(rctx)
		if matched && result.MatchedRule != nil {
			matchedRuleID = result.MatchedRule.ID
			switch result.Action {
			case rules.ActionBlock, rules.ActionRateLimit:
				// Blocked by rule — return immediately
				http.Error(w, result.BlockMessage, result.BlockStatus)
				// Still log the blocked request
				h.persistAndBroadcast(session, provider, model, agentID, appName,
					promptPreview, inputTokens, 0, estimatedCost, 0,
					result.BlockStatus, false, loopResult,
					fmt.Sprintf("Blocked by rule: %s", result.MatchedRule.Name),
					"", "", 0, result.MatchedRule.ID, false,
					0, "",
					jbDetected, jbScore, jbCategory)
				return

			case rules.ActionOverrideModel:
				// Replace the model before upstream call.
				// Resolve short alias to full API ID so the provider accepts it.
				model = resolveModelAlias(result.OverrideModel)
				reqData["model"] = model
				if b, err := json.Marshal(reqData); err == nil {
					bodyBytes = b
				}

			case rules.ActionInjectPrompt:
				// Inject system prompt — format depends on provider API format
				injectSystemPromptInto(reqData, apiFormat, result.InjectedSystemPrompt)
				if b, err := json.Marshal(reqData); err == nil {
					bodyBytes = b
				}

			case rules.ActionRedirect:
				// Change upstream URL
				upstreamURL = result.RedirectURL
			}
		}
	}

	// ── NeMo Guard default auto-block ───────────────────────────────────────
	// A CondJailbreak rule (if any) already returned above. Otherwise, when a
	// jailbreak was detected and mode is "block" (default), reject here. In
	// "flag" mode the request is forwarded and the detection is recorded below.
	if jbDetected && h.nemoGuard != nil && h.nemoGuard.Mode() == "block" {
		h.nemoGuard.MarkBlocked()
		http.Error(w, "Request blocked: jailbreak attempt detected.", http.StatusForbidden)
		h.persistAndBroadcast(session, provider, model, agentID, appName,
			promptPreview, inputTokens, 0, estimatedCost, 0,
			http.StatusForbidden, false, loopResult,
			"Blocked: jailbreak detected (nemoguard)",
			"", "", 0, 0, false,
			0, "",
			jbDetected, jbScore, jbCategory)
		return
	}

	// ── PII Redaction ──────────────────────────────────────────────────────
	var piiRedactedCount int
	var piiCategoriesJSON string
	if h.cfg.PIIEnabled {
		piiCfg := redactor.Config{
			Enabled:    true,
			Mode:       h.cfg.PIIMode,
			Categories: redactor.ParseCategories(h.cfg.PIICategories),
		}
		// If no categories specified, enable all (default behavior)
		if len(piiCfg.Categories) == 0 {
			for _, cat := range redactor.AllCategories {
				piiCfg.Categories[cat.Key] = true
			}
		}
		if redactedBytes, count, catCounts, err := redactor.RedactRequestBody(bodyBytes, apiFormat, piiCfg); err != nil {
			log.Printf("[PII] redaction error (passing through): %v", err)
		} else if count > 0 {
			bodyBytes = redactedBytes
			piiRedactedCount = count
			if catJSON, err := json.Marshal(catCounts); err == nil {
				piiCategoriesJSON = string(catJSON)
			}
			log.Printf("[PII] redacted %d item(s) in request from %s — %v", count, agentID, catCounts)
		}
	}

	// ── Memory retrieval (inject relevant memories as context) ──────────────
	if h.memoryStore != nil && h.memoryStore.IsEnabled() && h.cfg.MemoryEnabled {
		memResults, memErr := h.memoryStore.Search(prompt, agentID, h.cfg.MemoryMaxResults)
		if memErr != nil {
			log.Printf("[MEMORY] search error: %v", memErr)
		} else if len(memResults) > 0 {
			memCtx := memory.BuildMemoryContext(memResults)
			if memCtx != "" {
				injectSystemPromptInto(reqData, apiFormat, memCtx)
				if b, err := json.Marshal(reqData); err == nil {
					bodyBytes = b
				}
				log.Printf("[MEMORY] injected %d memories for agent=%s", len(memResults), agentID)
			}
		}
	}

	// ── Cache lookup (non-streaming only) ───────────────────────────────────
	isStreaming := reqData["stream"] == true ||
		strings.Contains(upstreamURL, "streamGenerateContent")

	var cacheHash string
	var semanticKey string
	cacheHit := false

	if !isStreaming && h.responseCache != nil && h.responseCache.IsEnabled() {
		// Check opt-out header
		if r.Header.Get("X-Cache-Control") != "no-cache" {
			// Check temperature (only cache temp=0 if configured)
			temp, _ := reqData["temperature"].(float64)
			shouldCache := !h.cfg.CacheOnlyTemp0 || temp == 0

			if shouldCache {
				cacheHash = cache.BuildRequestHash(provider, model, bodyBytes)
				if entry, ok := h.responseCache.Lookup(cacheHash); ok {
					// EXACT CACHE HIT — serve directly
					cacheHit = true
					for key, values := range entry.ResponseHeaders {
						for _, v := range values {
							w.Header().Add(key, v)
						}
					}
					w.Header().Set("X-Cache", "HIT")
					w.Header().Del("Content-Length")
					w.WriteHeader(entry.StatusCode)
					w.Write(entry.ResponseBody)

					h.persistAndBroadcast(session, provider, model, agentID, appName,
						promptPreview, entry.InputTokens, entry.OutputTokens, entry.CostPerHit, 0,
						entry.StatusCode, false, loopResult, "",
						"", "", 0, matchedRuleID, true,
						piiRedactedCount, piiCategoriesJSON,
						jbDetected, jbScore, jbCategory)
					log.Printf("[CACHE] HIT for %s/%s hash=%s saved=$%.6f", provider, model, cacheHash[:12], entry.CostPerHit)
					return
				}

				// ── Semantic cache lookup (on exact-match miss) ──────────
				sc := h.getSemanticCache()
				if sc != nil && sc.IsEnabled() {
					semanticKey = cache.BuildSemanticKey(provider, model, bodyBytes)
					if semanticKey != "" {
						if semHash, sim, found := sc.Lookup(semanticKey, provider, model); found {
							if entry, ok := h.responseCache.Lookup(semHash); ok {
								cacheHit = true
								for key, values := range entry.ResponseHeaders {
									for _, v := range values {
										w.Header().Add(key, v)
									}
								}
								w.Header().Set("X-Cache", "SEMANTIC-HIT")
								w.Header().Set("X-Cache-Similarity", fmt.Sprintf("%.4f", sim))
								w.Header().Del("Content-Length")
								w.WriteHeader(entry.StatusCode)
								w.Write(entry.ResponseBody)

								h.persistAndBroadcast(session, provider, model, agentID, appName,
									promptPreview, entry.InputTokens, entry.OutputTokens, entry.CostPerHit, 0,
									entry.StatusCode, false, loopResult, "",
									"", "", 0, matchedRuleID, true,
									piiRedactedCount, piiCategoriesJSON,
									jbDetected, jbScore, jbCategory)
								log.Printf("[SEMANTIC-CACHE] HIT for %s/%s sim=%.4f saved=$%.6f", provider, model, sim, entry.CostPerHit)
								return
							}
						}
					}
				}
			}
		}
	}

	// ── Upstream request with retry and fallback ────────────────────────────
	upstreamStart := time.Now()
	resp, finalStatus, upstreamErr, retryResult, fallbackProvider, fallbackModel, fallbackConfigID := h.executeUpstreamRequest(
		agentID, provider, model, upstreamURL, bodyBytes, upstreamHeaders,
	)
	latencyMs := time.Since(upstreamStart).Milliseconds()

	// Capture original provider/model before overwriting with fallback
	originalProvider := ""
	originalModel := ""
	if fallbackProvider != "" {
		originalProvider = provider
		originalModel = model
		provider = fallbackProvider
		model = fallbackModel
	}

	// ── Handle network-level error (no response at all) ─────────────────────
	if upstreamErr != nil {
		errMsg := fmt.Sprintf("upstream error: %v", upstreamErr)
		if retryResult.Attempts > 1 {
			errMsg = fmt.Sprintf("%s (%s)", errMsg, reliability.FormatRetryInfo(retryResult))
		}
		log.Printf("[PROXY] %s", errMsg)
		http.Error(w, "Upstream request failed", http.StatusBadGateway)

		// Still record the failed attempt so the dashboard shows it.
		h.persistAndBroadcast(session, provider, model, agentID, appName,
			promptPreview, inputTokens, 0, 0, latencyMs,
			http.StatusBadGateway, false, loopResult, errMsg,
			originalProvider, originalModel, fallbackConfigID, matchedRuleID, false,
			piiRedactedCount, piiCategoriesJSON,
			jbDetected, jbScore, jbCategory)
		return
	}
	defer resp.Body.Close()

	// Log retry/fallback info if applicable
	if retryResult.Attempts > 1 {
		log.Printf("[RETRY] %s: %s", provider+"/"+model, reliability.FormatRetryInfo(retryResult))
	}
	if fallbackProvider != "" {
		log.Printf("[FALLBACK] Used fallback: %s", reliability.FormatFallbackInfo(provider, model, fallbackProvider, fallbackModel))
	}

	_ = finalStatus

	// Set X-Cache header for transparency
	if cacheHash != "" {
		w.Header().Set("X-Cache", "MISS")
	}

	var outputTokens int
	var upstreamErrMsg string
	var responseBodyBytes []byte // for cache storage (non-streaming only)
	var cachedInputTokens int

	if isStreaming {
		sResult := h.handleStreamingResponse(w, resp, session, model, inputTokens, apiFormat, prompt, loopResult)
		// Use exact provider-reported token counts when available
		inputTokens = sResult.inputTokens
		cachedInputTokens = sResult.cachedInputTokens
		outputTokens = sResult.outputTokens
	} else {
		nsResult := h.handleNonStreamingResponse(w, resp, session, model, inputTokens, apiFormat, prompt, loopResult)
		// Use exact provider-reported token counts (not tiktoken pre-estimates)
		inputTokens = nsResult.inputTokens
		cachedInputTokens = nsResult.cachedInputTokens
		outputTokens = nsResult.outputTokens
		upstreamErrMsg = nsResult.errorMessage
		responseBodyBytes = nsResult.responseBody
	}

	// ── Cache store (on 200 OK non-streaming responses) ─────────────────────
	if !isStreaming && !cacheHit && cacheHash != "" && resp.StatusCode == http.StatusOK && responseBodyBytes != nil {
		capturedHeaders := make(map[string][]string)
		for key, values := range resp.Header {
			lk := strings.ToLower(key)
			if lk == "content-length" || lk == "transfer-encoding" {
				continue
			}
			capturedHeaders[key] = values
		}
		entryCost := tracker.CalculateCost(model, inputTokens, cachedInputTokens, outputTokens)
		h.responseCache.Store(cacheHash, &cache.CacheEntry{
			Hash:            cacheHash,
			Provider:        provider,
			Model:           model,
			StatusCode:      resp.StatusCode,
			ResponseBody:    responseBodyBytes,
			ResponseHeaders: capturedHeaders,
			InputTokens:     inputTokens,
			OutputTokens:    outputTokens,
			CostPerHit:      entryCost,
			ExpiresAt:       time.Now().Add(h.responseCache.DefaultTTL()),
		})
		log.Printf("[CACHE] STORED %s/%s hash=%s ttl=%v", provider, model, cacheHash[:12], h.responseCache.DefaultTTL())

		// ── Semantic cache: store embedding vector (async) ───────────
		scStore := h.getSemanticCache()
		if scStore != nil && scStore.IsEnabled() {
			if semanticKey == "" {
				semanticKey = cache.BuildSemanticKey(provider, model, bodyBytes)
			}
			if semanticKey != "" {
				go func() {
					defer func() {
						if r := recover(); r != nil {
							log.Printf("[SEMANTIC-CACHE] panic in Store: %v", r)
						}
					}()
					scStore.Store(semanticKey, cacheHash, provider, model)
				}()
			}
		}
	}

	// ── Memory extraction (async — extract facts from conversation) ─────────
	if !isStreaming && h.memoryStore != nil && h.memoryStore.IsEnabled() && h.cfg.MemoryEnabled &&
		resp.StatusCode == http.StatusOK && responseBodyBytes != nil {
		capturedBody := bodyBytes         // request body
		capturedResp := responseBodyBytes // response body
		capturedAgent := agentID
		capturedSession := session.ID
		capturedProvider := provider
		capturedModel := model
		go func() {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("[MEMORY] panic in extraction: %v", r)
				}
			}()
			facts := memory.ExtractFacts(capturedProvider, capturedBody, capturedResp)
			for _, fact := range facts {
				if err := h.memoryStore.StoreFact(capturedAgent, capturedSession, fact, capturedProvider, capturedModel); err != nil {
					log.Printf("[MEMORY] store fact error: %v", err)
				}
			}
		}()
	}

	// ── Persist + broadcast (always, even on 4xx/5xx upstream responses) ────
	cost := tracker.CalculateCost(model, inputTokens, cachedInputTokens, outputTokens)
	h.persistAndBroadcast(session, provider, model, agentID, appName,
		promptPreview, inputTokens, outputTokens, cost, latencyMs,
		resp.StatusCode, isStreaming, loopResult, upstreamErrMsg,
		originalProvider, originalModel, fallbackConfigID, matchedRuleID, false,
		piiRedactedCount, piiCategoriesJSON,
		jbDetected, jbScore, jbCategory)

}

// persistAndBroadcast writes the request to SQLite, then broadcasts the WS
// event and updated agent summaries — in that order so replay is consistent.
//
// The DB write happens in a goroutine to avoid blocking the HTTP response.
// The WS broadcast fires AFTER the write so a dashboard refresh immediately
// after will find the row in the DB.
func (h *Handler) persistAndBroadcast(
	session *tracker.Session,
	provider, model, agentID, appName, promptPreview string,
	inputTokens, outputTokens int,
	cost float64,
	latencyMs int64,
	statusCode int,
	isStreaming bool,
	loopResult detector.LoopDetectionResult,
	errorMessage string,
	originalProvider, originalModel string,
	fallbackConfigID int64,
	matchedRuleID int64,
	cacheHit bool,
	piiRedactedCount int,
	piiCategories string,
	jailbreakDetected bool,
	jailbreakScore float64,
	jailbreakCategory string,
) {
	// Prometheus instrumentation — one call per request (incl. cache hits). Fallback is
	// detected the same way the event below does (originalProvider set ⇒ rerouted).
	metrics.RecordRequest(provider, model, agentID, appName, inputTokens, outputTokens, cost,
		latencyMs, statusCode, cacheHit, originalProvider != "", loopResult.LoopDetected,
		piiRedactedCount, jailbreakDetected)

	eventID := uuid.New().String()

	event := map[string]interface{}{
		"type":               "request_event",
		"id":                 eventID,
		"timestamp":          time.Now().Unix(),
		"session_id":         session.ID,
		"agent_id":           agentID,
		"app_name":           appName,
		"provider":           provider,
		"model":              model,
		"prompt_preview":     promptPreview,
		"input_tokens":       inputTokens,
		"output_tokens":      outputTokens,
		"cost":               cost,
		"latency_ms":         latencyMs,
		"status_code":        statusCode,
		"is_streaming":       isStreaming,
		"loop_detected":      loopResult.LoopDetected,
		"loop_severity":      loopResult.Severity,
		"error":              errorMessage,
		"is_error":           errorMessage != "",
		"original_provider":  originalProvider,
		"original_model":     originalModel,
		"fallback_config_id": fallbackConfigID,
		"fallback_used":      originalProvider != "",
		"matched_rule_id":    matchedRuleID,
		"cache_hit":          cacheHit,
		"pii_redacted":       piiRedactedCount,
		"jailbreak_detected": jailbreakDetected,
		"jailbreak_score":    jailbreakScore,
	}

	log.Printf("[EVENT] agent=%s provider=%s model=%s in=%d out=%d cost=$%.6f latency=%dms status=%d loop=%v",
		agentID, provider, model, inputTokens, outputTokens, cost, latencyMs, statusCode, loopResult.LoopDetected)

	// Always broadcast the live session totals (fast, in-memory)
	h.broadcastSessionUpdate(session, model, agentID, appName)

	if h.db == nil {
		// No DB: broadcast immediately
		if data, err := json.Marshal(event); err == nil {
			h.wsHub.Broadcast(data)
		}
		return
	}

	// With DB: write first, then broadcast for replay consistency.
	// Acquire semaphore slot to bound concurrent persist goroutines.
	dbWrite := func() {
		row := store.RequestRow{
			Timestamp:         time.Now(),
			SessionID:         session.ID,
			AgentID:           agentID,
			AppName:           appName,
			Provider:          provider,
			Model:             model,
			PromptPreview:     promptPreview,
			InputTokens:       inputTokens,
			OutputTokens:      outputTokens,
			Cost:              cost,
			LatencyMs:         latencyMs,
			StatusCode:        statusCode,
			IsStreaming:       isStreaming,
			LoopDetected:      loopResult.LoopDetected,
			LoopSeverity:      loopResult.Severity,
			ErrorMessage:      errorMessage,
			OriginalProvider:  originalProvider,
			OriginalModel:     originalModel,
			FallbackConfigID:  fallbackConfigID,
			MatchedRuleID:     matchedRuleID,
			CacheHit:          cacheHit,
			PIIRedactedCount:  piiRedactedCount,
			PIICategories:     piiCategories,
			JailbreakDetected: jailbreakDetected,
			JailbreakScore:    jailbreakScore,
			JailbreakCategory: jailbreakCategory,
		}
		if err := h.db.InsertRequest(row); err != nil {
			log.Printf("[DB] insert failed: %v", err)
		}

		// Broadcast request event after DB write
		if data, err := json.Marshal(event); err == nil {
			h.wsHub.Broadcast(data)
		}

		// Invalidate cached agent summaries so next query is fresh
		h.wsHub.InvalidateAgentSummaries()
	}

	select {
	case h.persistSem <- struct{}{}:
		// Slot acquired — run in background goroutine
		go func() {
			defer func() { <-h.persistSem }()
			dbWrite()
		}()
	default:
		// All slots full — persist synchronously to apply backpressure
		// rather than spawning an unbounded goroutine.
		log.Printf("[PROXY] persist semaphore full (%d) — running synchronously", maxPersistGoroutines)
		dbWrite()
	}
}

// streamingResult holds values returned by handleStreamingResponse
// so the caller can persist exact provider-reported token counts.
type streamingResult struct {
	inputTokens       int
	cachedInputTokens int
	outputTokens      int
}

// handleStreamingResponse handles streaming responses; returns a streamingResult
// with exact token counts when available from the provider's final SSE chunk.
func (h *Handler) handleStreamingResponse(w http.ResponseWriter, resp *http.Response, session *tracker.Session, model string, inputTokens int, apiFormat, prompt string, loopResult detector.LoopDetectionResult) streamingResult {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming not supported", http.StatusInternalServerError)
		return streamingResult{}
	}

	for key, values := range resp.Header {
		if strings.ToLower(key) == "content-type" {
			continue
		}
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}

	scanner := bufio.NewScanner(resp.Body)
	var outputTokens int
	var cachedInputTokens int
	var lastContentChunk []byte
	var warningInjected bool

	for scanner.Scan() {
		line := scanner.Text()

		if line == "" || strings.HasPrefix(line, ":") {
			fmt.Fprintf(w, "%s\n", line)
			flusher.Flush()
			continue
		}

		if strings.Contains(line, "[DONE]") {
			if !warningInjected && lastContentChunk != nil {
				h.injectWarningIntoChunk(w, lastContentChunk, session, model, inputTokens, outputTokens, apiFormat, prompt, loopResult, flusher)
				warningInjected = true
			}
			fmt.Fprintf(w, "%s\n", line)
			flusher.Flush()
			break
		}

		if strings.HasPrefix(line, "data: ") {
			data := strings.TrimPrefix(line, "data: ")
			// Try to read exact usage from the final chunk (providers emit this just
			// before [DONE]).  If found, overwrite our running totals with exact counts.
			if exactIn, cached, exactOut, ok := extractUsageFromResponse([]byte(data), apiFormat); ok && exactOut > 0 {
				inputTokens = exactIn
				cachedInputTokens = cached
				outputTokens = exactOut
			} else {
				tokens, _ := h.tokenCounter.CountTokens(extractContentFromChunk(data, apiFormat), model)
				outputTokens += tokens
			}
			lastContentChunk = []byte(data)
			fmt.Fprintf(w, "%s\n", line)
			flusher.Flush()
		} else {
			fmt.Fprintf(w, "%s\n", line)
			flusher.Flush()
		}
	}

	// Check for scanner errors (e.g. upstream connection drop mid-stream)
	if err := scanner.Err(); err != nil {
		log.Printf("[PROXY] streaming scanner error for %s: %v", model, err)
	}

	cost := tracker.CalculateCost(model, inputTokens, cachedInputTokens, outputTokens)
	session.AddCost(cost, inputTokens, outputTokens)

	return streamingResult{
		inputTokens:       inputTokens,
		cachedInputTokens: cachedInputTokens,
		outputTokens:      outputTokens,
	}
}

// injectWarningIntoChunk injects a loop warning into the last SSE chunk before [DONE]
func (h *Handler) injectWarningIntoChunk(w http.ResponseWriter, chunkData []byte, session *tracker.Session, model string, inputTokens, outputTokens int, apiFormat, prompt string, loopResult detector.LoopDetectionResult, flusher http.Flusher) {
	if !loopResult.LoopDetected {
		return
	}

	cost := tracker.CalculateCost(model, inputTokens, 0, outputTokens)
	totalCost, _, _, _ := session.GetStats()
	totalCost += cost

	warning := detector.GenerateWarningMessage(loopResult, totalCost)
	metadata := injector.Metadata{
		Warning:      warning,
		Severity:     loopResult.Severity,
		SessionCost:  totalCost,
		LoopDetected: true,
		WarningLevel: loopResult.WarningLevel,
	}

	var modifiedChunk []byte
	var err error
	if apiFormat == "openai" || apiFormat == "gemini" {
		modifiedChunk, err = injector.InjectIntoOpenAIChunk(chunkData, warning, metadata, h.injCfg)
	} else {
		modifiedChunk, err = injector.InjectIntoAnthropicChunk(chunkData, warning, metadata, h.injCfg)
	}

	if err == nil {
		fmt.Fprintf(w, "data: %s\n", string(modifiedChunk))
		flusher.Flush()
		h.logInjection(session.ID, loopResult, totalCost, model)
	}
}

// nonStreamingResult holds all values returned by handleNonStreamingResponse
// so the caller can persist exact provider-reported token counts and cost.
type nonStreamingResult struct {
	inputTokens       int
	cachedInputTokens int
	outputTokens      int
	errorMessage      string
	responseBody      []byte
}

// handleNonStreamingResponse handles non-streaming responses.
// It returns a nonStreamingResult with exact token counts from the provider
// (falling back to tiktoken estimates only when the provider omits usage).
func (h *Handler) handleNonStreamingResponse(w http.ResponseWriter, resp *http.Response, session *tracker.Session, model string, inputTokens int, apiFormat, prompt string, loopResult detector.LoopDetectionResult) nonStreamingResult {
	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		http.Error(w, "Failed to read response", http.StatusBadGateway)
		return nonStreamingResult{errorMessage: fmt.Sprintf("failed to read upstream body: %v", err)}
	}

	// Prefer the provider's own usage counts — they are exact.
	// Fall back to tiktoken re-counting only if the usage field is absent.
	exactIn, cachedIn, exactOut, ok := extractUsageFromResponse(bodyBytes, apiFormat)
	if ok {
		inputTokens = exactIn
	}
	outputTokens := exactOut
	if !ok {
		outputTokens = countTokensInResponse(bodyBytes, apiFormat, model, h.tokenCounter)
	}

	// Capture upstream error details for the dashboard (truncated for display)
	var errorMessage string
	if resp.StatusCode >= 400 {
		errorMessage = truncate(string(bodyBytes), 200)
		log.Printf("[PROXY] upstream %d for %s: %s", resp.StatusCode, model, errorMessage)
	}

	cost := tracker.CalculateCost(model, inputTokens, cachedIn, outputTokens)
	session.AddCost(cost, inputTokens, outputTokens)
	totalCost, _, _, _ := session.GetStats()

	// Only inject loop warnings on successful responses — don't mutate error bodies
	if loopResult.LoopDetected && resp.StatusCode < 400 {
		warning := detector.GenerateWarningMessage(loopResult, totalCost)
		metadata := injector.Metadata{
			Warning:      warning,
			Severity:     loopResult.Severity,
			SessionCost:  totalCost,
			LoopDetected: true,
			WarningLevel: loopResult.WarningLevel,
		}
		bodyBytes, _ = injector.InjectIntoNonStreamingResponse(bodyBytes, warning, metadata, h.injCfg, apiFormat)
		h.logInjection(session.ID, loopResult, totalCost, model)
	}

	// Forward upstream status + headers verbatim; strip Content-Length (body may differ after injection)
	w.Header().Del("Content-Length")
	for key, values := range resp.Header {
		if strings.ToLower(key) == "content-length" {
			continue
		}
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.WriteHeader(resp.StatusCode)
	w.Write(bodyBytes)

	return nonStreamingResult{
		inputTokens:       inputTokens,
		cachedInputTokens: cachedIn,
		outputTokens:      outputTokens,
		errorMessage:      errorMessage,
		responseBody:      bodyBytes,
	}
}

// broadcastSessionUpdate pushes in-memory session totals to all dashboard clients.
func (h *Handler) broadcastSessionUpdate(session *tracker.Session, model, agentID, appName string) {
	totalCost, inputTokens, outputTokens, requestCount := session.GetStats()
	update := map[string]interface{}{
		"type":          "cost_update",
		"session_id":    session.ID,
		"agent_id":      agentID,
		"app_name":      appName,
		"total_cost":    totalCost,
		"input_tokens":  inputTokens,
		"output_tokens": outputTokens,
		"request_count": requestCount,
		"model":         model,
		"duration":      session.GetDuration().Seconds(),
	}
	data, _ := json.Marshal(update)
	h.wsHub.Broadcast(data)
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func extractPrompt(messages []map[string]interface{}) string {
	if len(messages) == 0 {
		return ""
	}
	lastMsg := messages[len(messages)-1]
	// OpenAI format: content is a plain string
	if content, ok := lastMsg["content"].(string); ok {
		return content
	}
	// Anthropic format: content is an array of blocks [{type:"text", text:"..."}]
	if blocks, ok := lastMsg["content"].([]interface{}); ok {
		// Find the last "text" block (skip document blocks)
		for i := len(blocks) - 1; i >= 0; i-- {
			if block, ok := blocks[i].(map[string]interface{}); ok {
				if block["type"] == "text" {
					if text, ok := block["text"].(string); ok {
						return text
					}
				}
			}
		}
	}
	return ""
}

// messageText extracts the text of one message, handling OpenAI (content is a
// string) and Anthropic (content is an array of {type:"text", text}) shapes.
func messageText(m map[string]interface{}) string {
	if content, ok := m["content"].(string); ok {
		return content
	}
	if blocks, ok := m["content"].([]interface{}); ok {
		var parts []string
		for _, blk := range blocks {
			if bm, ok := blk.(map[string]interface{}); ok && bm["type"] == "text" {
				if t, ok := bm["text"].(string); ok {
					parts = append(parts, t)
				}
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

// extractConversationText builds the text sent to the jailbreak detector: the
// system prompt plus every user-role message, joined with newlines and capped so
// we never send an unbounded payload to the classifier. Falls back to the last
// message if nothing else is available.
func extractConversationText(reqData map[string]interface{}, messages []map[string]interface{}, apiFormat string) string {
	const maxLen = 16 * 1024
	var b strings.Builder
	if sys := extractSystemPrompt(reqData, apiFormat); sys != "" {
		b.WriteString(sys)
		b.WriteByte('\n')
	}
	for _, m := range messages {
		role, _ := m["role"].(string)
		if role != "" && role != "user" {
			continue // system handled above; skip assistant/tool turns
		}
		b.WriteString(messageText(m))
		b.WriteByte('\n')
		if b.Len() >= maxLen {
			break
		}
	}
	s := strings.TrimSpace(b.String())
	if len(s) > maxLen {
		s = s[:maxLen]
	}
	if s == "" {
		return extractPrompt(messages)
	}
	return s
}

func truncate(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "…"
}

func extractContentFromChunk(data, apiFormat string) string {
	var chunk map[string]interface{}
	if err := json.Unmarshal([]byte(data), &chunk); err != nil {
		return ""
	}
	if apiFormat == "openai" || apiFormat == "gemini" {
		if choices, ok := chunk["choices"].([]interface{}); ok && len(choices) > 0 {
			if delta, ok := choices[0].(map[string]interface{})["delta"].(map[string]interface{}); ok {
				if content, ok := delta["content"].(string); ok {
					return content
				}
			}
		}
	} else {
		if delta, ok := chunk["delta"].(map[string]interface{}); ok {
			if text, ok := delta["text"].(string); ok {
				return text
			}
		}
	}
	return ""
}

func countTokensInResponse(bodyBytes []byte, apiFormat, model string, tc *tracker.TokenCounter) int {
	var response map[string]interface{}
	if err := json.Unmarshal(bodyBytes, &response); err != nil {
		return 0
	}
	content := ""
	if apiFormat == "openai" || apiFormat == "gemini" {
		if choices, ok := response["choices"].([]interface{}); ok && len(choices) > 0 {
			if message, ok := choices[0].(map[string]interface{})["message"].(map[string]interface{}); ok {
				content, _ = message["content"].(string)
			}
		}
	} else {
		if contentArr, ok := response["content"].([]interface{}); ok && len(contentArr) > 0 {
			if block, ok := contentArr[0].(map[string]interface{}); ok {
				content, _ = block["text"].(string)
			}
		}
	}
	tokens, _ := tc.CountTokens(content, model)
	return tokens
}

// extractAgentFromUserAgent parses a User-Agent string to a short caller name.
func extractAgentFromUserAgent(ua string) string {
	if ua == "" {
		return "unknown"
	}
	token := ua
	if idx := strings.IndexAny(ua, " /"); idx > 0 {
		token = ua[:idx]
	}
	replacer := strings.NewReplacer(
		"anthropic-sdk-", "",
		"openai-sdk-", "",
		"python-httpx", "python",
		"node-fetch", "node",
	)
	token = replacer.Replace(strings.ToLower(token))
	if token == "" {
		return "unknown"
	}
	return token
}

// extractUsageFromResponse reads the provider's usage object from a response/SSE
// chunk body and returns (inputTokens, cachedInputTokens, outputTokens, ok).
// The apiFormat parameter determines which response shape to parse:
//
//	"openai" format (also used by most custom providers):
//	  {"usage": {"prompt_tokens": N, "completion_tokens": N,
//	             "prompt_tokens_details": {"cached_tokens": N}}}
//
//	"anthropic" format:
//	  {"usage": {"input_tokens": N, "output_tokens": N,
//	             "cache_read_input_tokens": N}}
//
//	"gemini" format:
//	  {"usageMetadata": {"promptTokenCount": N, "candidatesTokenCount": N,
//	                     "cachedContentTokenCount": N}}
func extractUsageFromResponse(body []byte, apiFormat string) (inputTokens, cachedInputTokens, outputTokens int, ok bool) {
	var data map[string]interface{}
	if err := json.Unmarshal(body, &data); err != nil {
		return 0, 0, 0, false
	}

	switch apiFormat {
	case "openai":
		usage, _ := data["usage"].(map[string]interface{})
		if usage == nil {
			return 0, 0, 0, false
		}
		in := int(toFloat(usage["prompt_tokens"]))
		out := int(toFloat(usage["completion_tokens"]))
		if in == 0 && out == 0 {
			return 0, 0, 0, false
		}
		var cached int
		if details, ok2 := usage["prompt_tokens_details"].(map[string]interface{}); ok2 {
			cached = int(toFloat(details["cached_tokens"]))
		}
		// inputTokens here is the TOTAL prompt_tokens (includes cached portion).
		// CalculateCost expects non-cached + cached split, so subtract.
		return in - cached, cached, out, true

	case "anthropic":
		usage, _ := data["usage"].(map[string]interface{})
		if usage == nil {
			return 0, 0, 0, false
		}
		in := int(toFloat(usage["input_tokens"]))
		out := int(toFloat(usage["output_tokens"]))
		cached := int(toFloat(usage["cache_read_input_tokens"]))
		if in == 0 && out == 0 {
			return 0, 0, 0, false
		}
		// Anthropic's input_tokens already excludes cached tokens (unlike OpenAI
		// where prompt_tokens includes them). Don't subtract again.
		return in, cached, out, true

	case "gemini":
		meta, _ := data["usageMetadata"].(map[string]interface{})
		if meta == nil {
			return 0, 0, 0, false
		}
		in := int(toFloat(meta["promptTokenCount"]))
		out := int(toFloat(meta["candidatesTokenCount"]))
		cached := int(toFloat(meta["cachedContentTokenCount"]))
		if in == 0 && out == 0 {
			return 0, 0, 0, false
		}
		// Gemini's promptTokenCount INCLUDES cached tokens (same as OpenAI).
		// CalculateCost expects non-cached + cached split, so subtract.
		return in - cached, cached, out, true
	}

	return 0, 0, 0, false
}

// toFloat safely coerces a JSON number (float64 from json.Unmarshal) to float64.
func toFloat(v interface{}) float64 {
	if f, ok := v.(float64); ok {
		return f
	}
	return 0
}

// injectSystemPromptInto modifies reqData in place to add a system prompt.
// Each provider API has a different system prompt format:
//
//   - Anthropic: top-level "system" — string OR array of content blocks.
//     When it's an array (common in production for prompt caching), we MUST
//     preserve existing blocks and their cache_control fields.
//   - OpenAI: {"role":"system"} message. We prepend to existing system message
//     content if one exists, or insert a new system message at position 0.
//   - Gemini: top-level "system_instruction" with "parts" array. We append
//     a new text part, preserving any existing instruction parts.
func injectSystemPromptInto(reqData map[string]interface{}, apiFormat, systemPrompt string) {
	switch apiFormat {
	case "anthropic":
		injectAnthropicSystem(reqData, systemPrompt)
	case "gemini":
		injectGeminiSystem(reqData, systemPrompt)
	default:
		injectOpenAISystem(reqData, systemPrompt)
	}
}

// injectAnthropicSystem handles Anthropic's system field which can be:
//   - absent → set as string
//   - string → prepend
//   - array of content blocks → prepend new text block (preserving cache_control on existing blocks)
func injectAnthropicSystem(reqData map[string]interface{}, systemPrompt string) {
	existing := reqData["system"]
	switch v := existing.(type) {
	case []interface{}:
		// Array format: [{type:"text", text:"...", cache_control:{...}}, ...]
		// Prepend our block WITHOUT cache_control so the user's cache_control
		// on their original blocks continues to work correctly.
		newBlock := map[string]interface{}{
			"type": "text",
			"text": systemPrompt,
		}
		reqData["system"] = append([]interface{}{newBlock}, v...)

	case string:
		if v != "" {
			reqData["system"] = systemPrompt + "\n\n" + v
		} else {
			reqData["system"] = systemPrompt
		}

	default:
		// No system field yet
		reqData["system"] = systemPrompt
	}
}

// injectOpenAISystem handles OpenAI's messages array. If a system message already
// exists at position 0, prepend to its content. Otherwise insert a new one.
func injectOpenAISystem(reqData map[string]interface{}, systemPrompt string) {
	msgs, _ := reqData["messages"].([]interface{})

	// Check if first message is already a system message
	if len(msgs) > 0 {
		if first, ok := msgs[0].(map[string]interface{}); ok {
			if role, _ := first["role"].(string); role == "system" {
				// Prepend to existing system message content
				existingContent, _ := first["content"].(string)
				first["content"] = systemPrompt + "\n\n" + existingContent
				return
			}
		}
	}

	// No existing system message — insert one at position 0
	systemMsg := map[string]interface{}{
		"role":    "system",
		"content": systemPrompt,
	}
	reqData["messages"] = append([]interface{}{systemMsg}, msgs...)
}

// injectGeminiSystem handles Gemini's system_instruction field:
//
//	{"system_instruction": {"parts": [{"text": "..."}]}}
//
// Appends a new text part, preserving any existing parts.
func injectGeminiSystem(reqData map[string]interface{}, systemPrompt string) {
	newPart := map[string]interface{}{"text": systemPrompt}

	si, exists := reqData["system_instruction"]
	if !exists {
		reqData["system_instruction"] = map[string]interface{}{
			"parts": []interface{}{newPart},
		}
		return
	}

	siMap, ok := si.(map[string]interface{})
	if !ok {
		reqData["system_instruction"] = map[string]interface{}{
			"parts": []interface{}{newPart},
		}
		return
	}

	parts, _ := siMap["parts"].([]interface{})
	siMap["parts"] = append([]interface{}{newPart}, parts...)
}

// extractSystemPrompt extracts the system prompt text from the parsed request body.
// Returns "" if no system prompt is present. Handles all provider formats:
//   - OpenAI: first message with role=="system"
//   - Anthropic: top-level "system" (string or array of text blocks)
//   - Gemini: "system_instruction" → "parts" → text
func extractSystemPrompt(reqData map[string]interface{}, apiFormat string) string {
	switch apiFormat {
	case "anthropic":
		switch v := reqData["system"].(type) {
		case string:
			return v
		case []interface{}:
			var parts []string
			for _, block := range v {
				if b, ok := block.(map[string]interface{}); ok {
					if t, ok := b["text"].(string); ok {
						parts = append(parts, t)
					}
				}
			}
			return strings.Join(parts, "\n\n")
		}
		return ""

	case "gemini":
		si, _ := reqData["system_instruction"].(map[string]interface{})
		if si == nil {
			return ""
		}
		parts, _ := si["parts"].([]interface{})
		var texts []string
		for _, p := range parts {
			if pm, ok := p.(map[string]interface{}); ok {
				if t, ok := pm["text"].(string); ok {
					texts = append(texts, t)
				}
			}
		}
		return strings.Join(texts, "\n\n")

	default: // openai
		msgs, _ := reqData["messages"].([]interface{})
		if len(msgs) == 0 {
			return ""
		}
		if first, ok := msgs[0].(map[string]interface{}); ok {
			if role, _ := first["role"].(string); role == "system" {
				if content, ok := first["content"].(string); ok {
					return content
				}
			}
		}
		return ""
	}
}

func (h *Handler) logInjection(sessionID string, result detector.LoopDetectionResult, cost float64, model string) {
	log.Printf("[INJECTION] Session: %s, Severity: %s, Cost: $%.4f, Model: %s, Similar: %d",
		sessionID, result.Severity, cost, model, result.SimilarCount)

	injection := map[string]interface{}{
		"type":          "injection",
		"session_id":    sessionID,
		"severity":      result.Severity,
		"cost":          cost,
		"model":         model,
		"similar_count": result.SimilarCount,
		"timestamp":     time.Now().Unix(),
	}
	data, _ := json.Marshal(injection)
	h.wsHub.Broadcast(data)
}
