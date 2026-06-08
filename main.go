package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"tokomoco/auth"
	"tokomoco/cache"
	"tokomoco/config"
	"tokomoco/dashboard"
	"tokomoco/detector"
	"tokomoco/embedding"
	"tokomoco/memory"
	"tokomoco/nemoguard"
	"tokomoco/providers"
	"tokomoco/proxy"
	"tokomoco/reliability"
	"tokomoco/rules"
	"tokomoco/store"
	"tokomoco/tracker"
	"tokomoco/vectorstore"

	"github.com/gorilla/mux"
)

// newEmbedderFromConfig builds the embedding provider selected in config.
// "aratiri-bge-m3" → the cluster bge-m3 service (dense+sparse hybrid); anything
// else → OpenAI. Used by the semantic cache + memory layer (init and hot-reload).
func newEmbedderFromConfig(c *config.Config) (embedding.Embedder, error) {
	switch c.EmbeddingProvider {
	case "aratiri-bge-m3", "aratiri", "bge-m3":
		return embedding.NewAratiriEmbedder(
			embedding.WithAratiriBaseURL(c.EmbeddingBaseURL),
			embedding.WithAratiriAPIKey(c.EmbeddingAPIKey),
		)
	default: // "openai" or unset
		return embedding.NewOpenAIEmbedder(
			embedding.WithModel(c.EmbeddingModel),
			embedding.WithAPIKey(c.EmbeddingAPIKey),
		)
	}
}

func main() {
	// ── Configuration ───────────────────────────────────────────────────────
	// Optional config file path via CONFIG_FILE env var; defaults to config.json
	// if it exists in the working directory, otherwise all defaults apply.
	configPath := os.Getenv("CONFIG_FILE")
	if configPath == "" {
		if _, err := os.Stat("config.json"); err == nil {
			configPath = "config.json"
		}
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		log.Fatalf("config error: %v", err)
	}

	// ── Persistence ─────────────────────────────────────────────────────────
	// Postgres when CONFIG_DATABASE_URL is set; otherwise SQLite at CONFIG_DB_PATH.
	db, err := store.Open(cfg.DatabaseURL, cfg.DBPath)
	if err != nil {
		log.Fatalf("Failed to open database: %v", err)
	}
	defer db.Close()

	// ── Restore persisted settings ─────────────────────────────────────────
	// Settings saved via the dashboard UI are stored in the DB and must be
	// restored before any component reads cfg (semantic cache, memory, etc.).
	settingsAPI := config.NewSettingsAPI(&cfg, nil, db)
	if settingsAPI.LoadFromDB() {
		log.Printf("[STARTUP] settings restored from database")
	}

	db.LogStats()

	// Cancellable context for background goroutines — cancelled during shutdown
	// so they stop before the DB connection closes.
	bgCtx, bgCancel := context.WithCancel(context.Background())
	defer bgCancel()

	// Daily pruning goroutine — keeps the DB from growing unboundedly.
	go func() {
		// Run once at startup (in case the proxy was down for a few days)
		if _, err := db.Prune(cfg.DBKeepDays); err != nil {
			log.Printf("[DB] startup prune failed: %v", err)
		}
		ticker := time.NewTicker(24 * time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-bgCtx.Done():
				log.Printf("[DB] prune goroutine stopped")
				return
			case <-ticker.C:
				if _, err := db.Prune(cfg.DBKeepDays); err != nil {
					log.Printf("[DB] daily prune failed: %v", err)
				}
			}
		}
	}()

	// ── Core components ─────────────────────────────────────────────────────
	sessionTracker := tracker.NewSessionTracker(cfg.SessionMaxAgeDuration(), cfg.SessionMaxSize)
	requestStore := detector.NewRequestStore(cfg.LoopWindowDuration())
	loopDetector := detector.NewLoopDetector(requestStore, cfg.LoopThreshold, cfg.LoopSimilarity)
	wsHub := dashboard.NewHub(db, cfg.HistoryLimit)
	wsUpgrader := dashboard.NewUpgrader(cfg.AllowedOriginList())

	// Restore session totals from DB into memory so metrics survive restarts
	if sessions, err := db.AllSessions(); err == nil {
		for _, s := range sessions {
			sessionTracker.RestoreSession(
				s.SessionID, s.TotalCost, s.InputTokens, s.OutputTokens,
				s.RequestCount, s.StartTime,
			)
		}
		log.Printf("[STARTUP] restored %d session(s) from DB", len(sessions))
	} else {
		log.Printf("[STARTUP] session restore failed: %v", err)
	}

	// Start WebSocket hub
	go wsHub.Run()

	// ── Rules Engine ────────────────────────────────────────────────────────
	var rulesEngine *rules.Engine
	var rulesAPI *rules.APIHandler
	if cfg.RulesEnabled {
		ruleStore := rules.NewRuleStore(db.DB())
		var err error
		rulesEngine, err = rules.NewEngine(ruleStore)
		if err != nil {
			log.Printf("[RULES] engine init failed (continuing without rules): %v", err)
			rulesEngine = nil
		} else {
			rulesAPI = rules.NewAPIHandler(rulesEngine, db)
			log.Printf("[RULES] engine started with %d rules", rulesEngine.RuleCount())
		}
	} else {
		log.Printf("[RULES] rules engine disabled")
	}

	// ── Fallback Configuration ──────────────────────────────────────────────
	fallbackStore := reliability.NewFallbackStore(db.DB())
	fallbackAPI := reliability.NewFallbackAPI(fallbackStore, db)

	// Seed default fallback configurations on first run
	if err := fallbackStore.SeedDefaults(); err != nil {
		log.Printf("[FALLBACK] failed to seed defaults: %v", err)
	}

	// ── Custom Providers ───────────────────────────────────────────────────
	providerStore := providers.NewProviderStore(db.DB())
	// Seed a ready-to-use OpenRouter provider on first run (set OPENROUTER_API_KEY
	// to use it; call models as openrouter/<id>).
	if seededOR, err := providerStore.SeedOpenRouter(); err != nil {
		log.Printf("[PROVIDERS] failed to seed OpenRouter: %v", err)
	} else if seededOR {
		log.Printf("[PROVIDERS] seeded OpenRouter provider (set OPENROUTER_API_KEY, then use model openrouter/<id>)")
	}
	providerAPI := providers.NewAPI(providerStore)
	providerNames := providerStore.AllNames()
	if len(providerNames) > 0 {
		log.Printf("[PROVIDERS] loaded %d custom provider(s): %v", len(providerNames), providerNames)
	} else {
		log.Printf("[PROVIDERS] no custom providers configured")
	}

	// ── Model Pricing (DB-backed with in-memory cache) ─────────────────────
	pricingStore := tracker.InitPricingStore(db)
	seeded, _ := pricingStore.SeedDefaults()
	if seeded {
		log.Printf("[PRICING] seeded %d default model pricing entries", pricingStore.CacheSize())
	} else {
		log.Printf("[PRICING] loaded %d model pricing entries from DB", pricingStore.CacheSize())
	}
	pricingAPI := tracker.NewPricingAPIHandler(pricingStore, db)

	// Auto-refresh OpenRouter prices on boot + once a day (if OPENROUTER_API_KEY
	// is set). Manual refresh is also available via POST /api/pricing/refresh-openrouter.
	if cfg.PricingOpenRouterAutoRefresh {
		go func() {
			refresh := func() {
				if os.Getenv("OPENROUTER_API_KEY") == "" {
					return // no key — nothing to do; manual button still works once set
				}
				if n, err := tracker.RefreshFromOpenRouter(db, pricingStore); err != nil {
					log.Printf("[PRICING] openrouter auto-refresh failed: %v", err)
				} else {
					log.Printf("[PRICING] openrouter auto-refresh updated %d models", n)
				}
			}
			refresh()
			ticker := time.NewTicker(24 * time.Hour)
			defer ticker.Stop()
			for range ticker.C {
				refresh()
			}
		}()
	}

	// ── Response Cache ──────────────────────────────────────────────────────
	responseCache := cache.NewResponseCache(
		db,
		cfg.CacheMaxEntries,
		time.Duration(cfg.CacheTTLMinutes)*time.Minute,
		cfg.CacheEnabled,
	)
	log.Printf("[CACHE] response cache enabled=%v maxEntries=%d ttl=%dm",
		cfg.CacheEnabled, cfg.CacheMaxEntries, cfg.CacheTTLMinutes)

	// ── Semantic Cache ─────────────────────────────────────────────────────
	var semanticCache *cache.SemanticCache
	if cfg.SemanticCacheEnabled {
		emb, embErr := newEmbedderFromConfig(&cfg)
		if embErr != nil {
			log.Printf("[SEMANTIC-CACHE] ⚠ embedding init failed: %v — semantic cache disabled", embErr)
		} else {
			vs, vsErr := vectorstore.New(db.DB(), emb.Dimensions(), cfg.SemanticCacheMaxVectors)
			if vsErr != nil {
				log.Printf("[SEMANTIC-CACHE] ⚠ vector store init failed: %v — semantic cache disabled", vsErr)
			} else {
				semanticCache = cache.NewSemanticCache(emb, vs, cfg.SemanticCacheThreshold, cfg.SemanticCacheSparseWeight, true)
				log.Printf("[SEMANTIC-CACHE] enabled provider=%s dims=%d threshold=%.2f sparseWeight=%.2f maxVectors=%d",
					cfg.EmbeddingProvider, emb.Dimensions(), cfg.SemanticCacheThreshold, cfg.SemanticCacheSparseWeight, cfg.SemanticCacheMaxVectors)
			}
		}
	} else {
		log.Printf("[SEMANTIC-CACHE] disabled (set CONFIG_SEMANTIC_CACHE_ENABLED=true to enable)")
	}

	// ── Memory Layer ──────────────────────────────────────────────────────
	var memoryStore *memory.Store
	if cfg.MemoryEnabled {
		// Memory layer reuses the same embedding provider as the semantic cache
		// (built from the same config; selected by cfg.EmbeddingProvider).
		var memEmb embedding.Embedder
		memEmb2, memEmbErr := newEmbedderFromConfig(&cfg)
		if memEmbErr != nil {
			log.Printf("[MEMORY] ⚠ embedding init failed: %v — memory disabled", memEmbErr)
		} else {
			memEmb = memEmb2
		}

		if memEmb != nil {
			var memErr error
			memoryStore, memErr = memory.NewStore(db.DB(), memEmb, cfg.MemoryMaxEntries, cfg.MemoryThreshold, true,
				memory.WithRecencyLambda(cfg.MemoryRecencyLambda),
				memory.WithConflictThreshold(cfg.MemoryConflictThresh),
				memory.WithTTLDays(cfg.MemoryTTLDays),
			)
			if memErr != nil {
				log.Printf("[MEMORY] ⚠ store init failed: %v — memory disabled", memErr)
				memoryStore = nil
			} else {
				log.Printf("[MEMORY] enabled threshold=%.2f maxEntries=%d maxResults=%d memories=%d",
					cfg.MemoryThreshold, cfg.MemoryMaxEntries, cfg.MemoryMaxResults, memoryStore.Count())
			}
		}
	} else {
		log.Printf("[MEMORY] disabled (set CONFIG_MEMORY_ENABLED=true to enable)")
	}

	// onChange callback is wired AFTER proxyHandler and cacheAPI are created (below).

	// ── Authentication ─────────────────────────────────────────────────────
	authMiddleware := auth.NewMiddleware(db, cfg.AuthEnabled)
	authAPI := auth.NewAPIHandler(db, authMiddleware)

	if cfg.AuthEnabled {
		keyCount, _ := db.CountAPIKeys()
		if keyCount == 0 {
			log.Printf("[AUTH] ⚠ auth_enabled=true but no API keys exist — proxy endpoints will reject all requests!")
			log.Printf("[AUTH] Create a key via the dashboard or: POST /api/keys {\"name\":\"my-key\"}")
		} else {
			log.Printf("[AUTH] API key auth enabled (%d active keys)", keyCount)
		}
	} else {
		log.Printf("[AUTH] API key auth disabled (all requests pass through)")
	}

	// ── Security headers middleware ──────────────────────────────────────────
	// Applied to all responses for defense-in-depth.
	securityHeaders := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Content-Type-Options", "nosniff")
			w.Header().Set("X-Frame-Options", "DENY")
			w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
			w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
			// CSP for dashboard pages only (not API responses)
			if r.URL.Path == "/" || r.URL.Path == "/settings" || strings.HasPrefix(r.URL.Path, "/static/") {
				w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self' 'unsafe-inline' https://cdn.jsdelivr.net; style-src 'self' 'unsafe-inline'; connect-src 'self' ws: wss:; img-src 'self' data:;")
			}
			next.ServeHTTP(w, r)
		})
	}

	// ── Panic recovery middleware ────────────────────────────────────────────
	// Gorilla/mux does NOT recover from handler panics — an unrecovered panic
	// in any handler crashes the entire process. This middleware catches panics,
	// logs the stack trace, and returns 500 so the server stays up.
	panicRecovery := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					log.Printf("[PANIC] recovered from panic in %s %s: %v", r.Method, r.URL.Path, rec)
					http.Error(w, "Internal Server Error", http.StatusInternalServerError)
				}
			}()
			next.ServeHTTP(w, r)
		})
	}

	// ── Router ──────────────────────────────────────────────────────────────
	r := mux.NewRouter()
	r.Use(panicRecovery)
	r.Use(securityHeaders)

	// ── NeMo Guard jailbreak detector (optional; enabled when CONFIG_NEMOGUARD_URL is set) ──
	var nemoGuard *nemoguard.Detector
	if cfg.NeMoGuardURL != "" {
		nemoGuard = nemoguard.New(cfg.NeMoGuardURL, cfg.NeMoGuardClassifyPath, cfg.NeMoGuardAPIKey,
			cfg.NeMoGuardMode, cfg.NeMoGuardThreshold, cfg.NeMoGuardTimeout())
		log.Printf("[NEMOGUARD] enabled url=%s mode=%s", cfg.NeMoGuardURL, cfg.NeMoGuardMode)
	} else {
		log.Printf("[NEMOGUARD] disabled (set CONFIG_NEMOGUARD_URL to enable)")
	}

	proxyHandler := proxy.NewHandler(sessionTracker, loopDetector, wsHub, db, &cfg, rulesEngine, fallbackStore, responseCache, semanticCache, memoryStore, providerStore, nemoGuard)

	// authWrap wraps a handler with API key auth middleware.
	// When auth is disabled, this is a no-op passthrough.
	authWrap := func(h http.HandlerFunc) http.Handler { return authMiddleware.WrapFunc(h) }

	// Proxy endpoints — protected by API key auth.
	// Chain: incoming → security headers → auth middleware → proxy handler.
	r.Handle("/v1/chat/completions", authWrap(proxyHandler.HandleOpenAI)).Methods("POST")
	r.Handle("/v1/messages", authWrap(proxyHandler.HandleAnthropic)).Methods("POST")
	r.Handle("/v1beta/models/{model}:streamGenerateContent", authWrap(proxyHandler.HandleGemini)).Methods("POST")
	r.Handle("/v1beta/models/{model}:generateContent", authWrap(proxyHandler.HandleGemini)).Methods("POST")

	// Health check — always public (used by load balancers).
	r.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"status":"ok"}`)
	}).Methods("GET")

	// Dashboard — public (served from browser, auth is for API calls)
	r.HandleFunc("/", dashboard.ServeIndex).Methods("GET")
	r.HandleFunc("/settings", dashboard.ServeSettings).Methods("GET")
	r.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		dashboard.ServeWS(wsHub, wsUpgrader, w, r)
	})

	// ── Management APIs — protected by auth middleware ───────────────────────
	// All dashboard management endpoints require a valid API key when auth is enabled.
	// When auth is disabled (auth_enabled=false), the middleware is a no-op passthrough.

	// Rules API (if enabled)
	if rulesAPI != nil {
		rulesAPI.RegisterRoutes(r, authWrap)
	}

	// Fallback Configuration API
	r.Handle("/api/fallback-configs", authWrap(fallbackAPI.HandleList)).Methods("GET")
	r.Handle("/api/fallback-configs", authWrap(fallbackAPI.HandleCreate)).Methods("POST")
	r.Handle("/api/fallback-configs/hit-counts", authWrap(fallbackAPI.HandleHitCounts)).Methods("GET")
	r.Handle("/api/fallback-configs/for/{provider}/{model}", authWrap(fallbackAPI.HandleGetForModel)).Methods("GET")
	r.Handle("/api/fallback-configs/{id}", authWrap(fallbackAPI.HandleGet)).Methods("GET")
	r.Handle("/api/fallback-configs/{id}", authWrap(fallbackAPI.HandleUpdate)).Methods("PUT")
	r.Handle("/api/fallback-configs/{id}", authWrap(fallbackAPI.HandleDelete)).Methods("DELETE")
	r.Handle("/api/fallback-configs/{id}/toggle", authWrap(fallbackAPI.HandleToggle)).Methods("POST")
	r.Handle("/api/fallback-configs/{id}/requests", authWrap(fallbackAPI.HandleRequestLog)).Methods("GET")

	// Model Pricing API
	pricingAPI.RegisterRoutes(r, authWrap)

	// Custom Providers API
	providerAPI.RegisterRoutes(r, authWrap)

	// Memory API
	if memoryStore != nil {
		memoryAPI := memory.NewAPIHandler(memoryStore)
		memoryAPI.RegisterRoutes(r, authWrap)
	}

	// Cache API
	cacheAPI := cache.NewAPIHandler(responseCache, semanticCache)
	r.Handle("/api/cache", authWrap(cacheAPI.HandleStats)).Methods("GET")
	r.Handle("/api/cache/flush", authWrap(cacheAPI.HandleFlush)).Methods("POST")

	// Wire settings onChange callback now that proxyHandler and cacheAPI exist.
	{
		var lastEmbeddingKey string
		if cfg.EmbeddingAPIKey != "" {
			lastEmbeddingKey = cfg.EmbeddingAPIKey
		}
		settingsAPI.SetOnChange(func(c *config.Config) {
			// Sync memory store parameters
			if memoryStore != nil {
				memoryStore.SetEnabled(c.MemoryEnabled)
				memoryStore.SetThreshold(c.MemoryThreshold)
				memoryStore.SetRecencyLambda(c.MemoryRecencyLambda)
				memoryStore.SetConflictThreshold(c.MemoryConflictThresh)
				memoryStore.SetTTLDays(c.MemoryTTLDays)
			}

			// Sync exact cache parameters
			if responseCache != nil {
				responseCache.SetEnabled(c.CacheEnabled)
			}

			// Hot-reload semantic cache if embedding settings changed
			embeddingChanged := c.EmbeddingAPIKey != "" && c.EmbeddingAPIKey != lastEmbeddingKey
			if embeddingChanged || (c.SemanticCacheEnabled && semanticCache == nil && c.EmbeddingAPIKey != "") {
				emb, embErr := newEmbedderFromConfig(c)
				if embErr != nil {
					log.Printf("[SEMANTIC-CACHE] ⚠ embedding init failed on settings change: %v", embErr)
				} else {
					vs, vsErr := vectorstore.New(db.DB(), emb.Dimensions(), c.SemanticCacheMaxVectors)
					if vsErr != nil {
						log.Printf("[SEMANTIC-CACHE] ⚠ vector store init failed on settings change: %v", vsErr)
					} else {
						newSC := cache.NewSemanticCache(emb, vs, c.SemanticCacheThreshold, c.SemanticCacheSparseWeight, c.SemanticCacheEnabled)
						semanticCache = newSC
						proxyHandler.SetSemanticCache(newSC)
						cacheAPI.SetSemanticCache(newSC)
						lastEmbeddingKey = c.EmbeddingAPIKey
						log.Printf("[SEMANTIC-CACHE] ✓ hot-reloaded: provider=%s dims=%d threshold=%.2f sparseWeight=%.2f",
							c.EmbeddingProvider, emb.Dimensions(), c.SemanticCacheThreshold, c.SemanticCacheSparseWeight)
					}
				}
			} else if semanticCache != nil {
				// Just update threshold/enabled without rebuilding
				semanticCache.SetEnabled(c.SemanticCacheEnabled)
				semanticCache.SetThreshold(c.SemanticCacheThreshold)
			}
		})
	}

	// Analytics API — cost savings breakdown
	r.Handle("/api/analytics/savings", authWrap(func(w http.ResponseWriter, r *http.Request) {
		breakdown := db.GetSavingsBreakdown()
		// CacheHits and CacheSaved are now sourced from the requests table
		// (cache_hit=1), which covers both exact-match and semantic cache hits.

		resp := map[string]interface{}{
			"cache_hits":         breakdown.CacheHits,
			"cache_cost_saved":   breakdown.CacheSaved,
			"rules_blocked":      breakdown.RulesBlocked,
			"rules_cost_saved":   breakdown.RulesSaved,
			"fallback_count":     breakdown.FallbackCount,
			"total_cost_saved":   breakdown.TotalSaved,
			"pii_request_count":  breakdown.PIIRequestCount,
			"pii_items_redacted": breakdown.PIIItemsRedacted,
		}

		// Add memory analytics if available
		if memoryStore != nil {
			ms := memoryStore.GetStats()
			resp["memory"] = map[string]interface{}{
				"enabled":     ms.Enabled,
				"memories":    ms.Memories,
				"lookups":     ms.Lookups,
				"hits":        ms.Hits,
				"hit_rate":    ms.HitRate,
				"updated":     ms.Updated,
				"evicted":     ms.Evicted,
				"stale_count": ms.StaleCount,
			}
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	})).Methods("GET")

	// Security API — PII redaction analytics
	r.Handle("/api/security/pii", authWrap(func(w http.ResponseWriter, r *http.Request) {
		stats := db.GetPIIStats()
		resp := map[string]interface{}{
			"pii_enabled":              cfg.PIIEnabled,
			"pii_mode":                 cfg.PIIMode,
			"total_requests_scanned":   stats.TotalRequestsScanned,
			"requests_with_redactions": stats.RequestsWithRedactions,
			"total_items_redacted":     stats.TotalItemsRedacted,
			"categories":               stats.Categories,
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	})).Methods("GET")

	// Security API — enriched PII details (timeline, by-agent, recent)
	r.Handle("/api/security/pii/details", authWrap(func(w http.ResponseWriter, r *http.Request) {
		details := db.GetPIIDetails()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(details)
	})).Methods("GET")

	// Security API — NeMo Guard jailbreak detection analytics
	r.Handle("/api/security/nemoguard", authWrap(func(w http.ResponseWriter, r *http.Request) {
		stats := db.GetNemoGuardStats()
		resp := map[string]interface{}{
			"enabled":                cfg.NeMoGuardURL != "",
			"mode":                   cfg.NeMoGuardMode,
			"total_requests_scanned": stats.TotalRequestsScanned,
			"jailbreaks_detected":    stats.JailbreaksDetected,
			"blocked":                stats.Blocked,
		}
		if nemoGuard != nil {
			resp["live"] = nemoGuard.Stats()
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	})).Methods("GET")

	// Security API — enriched NeMo Guard details (timeline, by-agent, recent)
	r.Handle("/api/security/nemoguard/details", authWrap(func(w http.ResponseWriter, r *http.Request) {
		details := db.GetNemoGuardDetails()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(details)
	})).Methods("GET")

	// Sessions API — drill into session detail and request breakdown
	r.Handle("/api/sessions", authWrap(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		appName := q.Get("app_name")
		limit := 50
		offset := 0
		if v, err := strconv.Atoi(q.Get("limit")); err == nil && v > 0 && v <= 200 {
			limit = v
		}
		if v, err := strconv.Atoi(q.Get("offset")); err == nil && v >= 0 {
			offset = v
		}
		sessions, total, err := db.SessionList(appName, limit, offset)
		if err != nil {
			http.Error(w, "failed to query sessions", http.StatusInternalServerError)
			return
		}
		items := make([]map[string]interface{}, 0, len(sessions))
		for _, s := range sessions {
			items = append(items, map[string]interface{}{
				"session_id":    s.SessionID,
				"agent_id":      s.AgentID,
				"app_name":      s.AppName,
				"total_cost":    s.TotalCost,
				"input_tokens":  s.InputTokens,
				"output_tokens": s.OutputTokens,
				"request_count": s.RequestCount,
				"start_time":    s.StartTime.Unix(),
				"last_seen":     s.LastSeen.Unix(),
				"duration_sec":  int(s.LastSeen.Sub(s.StartTime).Seconds()),
			})
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"sessions": items,
			"total":    total,
			"limit":    limit,
			"offset":   offset,
		})
	})).Methods("GET")

	r.Handle("/api/sessions/{session_id}/requests", authWrap(func(w http.ResponseWriter, r *http.Request) {
		sessionID := mux.Vars(r)["session_id"]
		requests, err := db.SessionRequests(sessionID)
		if err != nil {
			http.Error(w, "failed to query session requests", http.StatusInternalServerError)
			return
		}
		session, _ := db.GetSession(sessionID)
		items := make([]map[string]interface{}, 0, len(requests))
		for _, req := range requests {
			item := map[string]interface{}{
				"id":             req.ID,
				"timestamp":      req.Timestamp.Unix(),
				"agent_id":       req.AgentID,
				"app_name":       req.AppName,
				"provider":       req.Provider,
				"model":          req.Model,
				"prompt_preview": req.PromptPreview,
				"input_tokens":   req.InputTokens,
				"output_tokens":  req.OutputTokens,
				"cost":           req.Cost,
				"latency_ms":     req.LatencyMs,
				"status_code":    req.StatusCode,
				"is_streaming":   req.IsStreaming,
				"loop_detected":  req.LoopDetected,
				"loop_severity":  req.LoopSeverity,
				"error_message":  req.ErrorMessage,
				"cache_hit":      req.CacheHit,
				"pii_redacted":   req.PIIRedactedCount,
			}
			if req.OriginalProvider != "" {
				item["original_provider"] = req.OriginalProvider
				item["original_model"] = req.OriginalModel
				item["fallback_used"] = true
			}
			items = append(items, item)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"session": map[string]interface{}{
				"session_id":    session.SessionID,
				"agent_id":      session.AgentID,
				"app_name":      session.AppName,
				"total_cost":    session.TotalCost,
				"input_tokens":  session.InputTokens,
				"output_tokens": session.OutputTokens,
				"request_count": session.RequestCount,
				"start_time":    session.StartTime.Unix(),
				"last_seen":     session.LastSeen.Unix(),
			},
			"requests": items,
		})
	})).Methods("GET")

	// Agent Timeline API
	r.Handle("/api/timeline/{agent_id}", authWrap(func(w http.ResponseWriter, r *http.Request) {
		agentID := mux.Vars(r)["agent_id"]
		q := r.URL.Query()
		var from, to int64
		if v, err := strconv.ParseInt(q.Get("from"), 10, 64); err == nil {
			from = v
		}
		if v, err := strconv.ParseInt(q.Get("to"), 10, 64); err == nil {
			to = v
		}
		limit := 50
		if v, err := strconv.Atoi(q.Get("limit")); err == nil && v > 0 && v <= 200 {
			limit = v
		}
		events, err := db.AgentTimeline(agentID, from, to, limit)
		if err != nil {
			http.Error(w, "failed to build timeline", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"agent_id": agentID,
			"events":   events,
			"count":    len(events),
		})
	})).Methods("GET")

	// Agent Requests API — recent requests for a specific agent (from DB)
	r.Handle("/api/agents/{agent_id}/requests", authWrap(func(w http.ResponseWriter, r *http.Request) {
		agentID := mux.Vars(r)["agent_id"]
		limit := 20
		if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v > 0 && v <= 200 {
			limit = v
		}
		requests, err := db.AgentRequests(agentID, limit)
		if err != nil {
			log.Printf("[AGENT-REQUESTS] error for agent=%s: %v", agentID, err)
			http.Error(w, "failed to query agent requests", http.StatusInternalServerError)
			return
		}
		items := make([]map[string]interface{}, 0, len(requests))
		for _, req := range requests {
			item := map[string]interface{}{
				"id":             req.ID,
				"timestamp":      req.Timestamp.Unix(),
				"session_id":     req.SessionID,
				"agent_id":       req.AgentID,
				"app_name":       req.AppName,
				"provider":       req.Provider,
				"model":          req.Model,
				"prompt_preview": req.PromptPreview,
				"input_tokens":   req.InputTokens,
				"output_tokens":  req.OutputTokens,
				"cost":           req.Cost,
				"latency_ms":     req.LatencyMs,
				"status_code":    req.StatusCode,
				"is_streaming":   req.IsStreaming,
				"loop_detected":  req.LoopDetected,
				"loop_severity":  req.LoopSeverity,
				"error_message":  req.ErrorMessage,
				"cache_hit":      req.CacheHit,
				"pii_redacted":   req.PIIRedactedCount,
			}
			if req.OriginalProvider != "" {
				item["original_provider"] = req.OriginalProvider
				item["original_model"] = req.OriginalModel
				item["fallback_used"] = true
			}
			items = append(items, item)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"agent_id": agentID,
			"requests": items,
			"count":    len(items),
		})
	})).Methods("GET")

	// Agent Trends API — daily aggregated metrics
	r.Handle("/api/analytics/agents/{agent_id}/trends", authWrap(func(w http.ResponseWriter, r *http.Request) {
		agentID := mux.Vars(r)["agent_id"]
		period := r.URL.Query().Get("period")
		days := 7
		if strings.HasSuffix(period, "d") {
			if v, err := strconv.Atoi(strings.TrimSuffix(period, "d")); err == nil && v > 0 && v <= 365 {
				days = v
			}
		}
		trends, err := db.AgentTrends(agentID, days)
		if err != nil {
			log.Printf("[TRENDS] error for agent=%s: %v", agentID, err)
			http.Error(w, "failed to compute trends", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"agent_id": agentID,
			"period":   fmt.Sprintf("%dd", days),
			"trends":   trends,
		})
	})).Methods("GET")

	// Cost-over-time API — powers the dashboard cumulative cost chart
	r.Handle("/api/analytics/cost-over-time", authWrap(func(w http.ResponseWriter, r *http.Request) {
		bucketMinutes := 60
		hours := 24
		if v, err := strconv.Atoi(r.URL.Query().Get("bucket")); err == nil && v > 0 && v <= 1440 {
			bucketMinutes = v
		}
		if v, err := strconv.Atoi(r.URL.Query().Get("hours")); err == nil && v > 0 && v <= 8760 {
			hours = v
		}
		buckets, err := db.CostOverTime(bucketMinutes, hours)
		if err != nil {
			log.Printf("[COST-OVER-TIME] error: %v", err)
			http.Error(w, "failed to compute cost over time", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"bucket_minutes": bucketMinutes,
			"hours":          hours,
			"buckets":        buckets,
		})
	})).Methods("GET")

	// Outcome Feedback API
	r.Handle("/api/feedback", authWrap(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			SessionID string                 `json:"session_id"`
			RequestID int64                  `json:"request_id"`
			AgentID   string                 `json:"agent_id"`
			Outcome   string                 `json:"outcome"`
			Score     float64                `json:"score"`
			Details   string                 `json:"details"`
			Metadata  map[string]interface{} `json:"metadata"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		// Validate outcome
		switch body.Outcome {
		case "success", "failure", "partial":
			// ok
		default:
			http.Error(w, `outcome must be "success", "failure", or "partial"`, http.StatusBadRequest)
			return
		}

		// Resolve agent_id: prefer explicit > request-level > session-level (first agent only).
		agentID := body.AgentID
		appName := ""
		if agentID == "" && body.RequestID > 0 {
			// Resolve from the specific request — always a single agent_id
			if req, err := db.GetRequestByID(body.RequestID); err == nil {
				agentID = req.AgentID
				appName = req.AppName
			}
		}
		if agentID == "" && body.SessionID != "" {
			if sess, err := db.GetSession(body.SessionID); err == nil && sess.AgentID != "" {
				// Session.AgentID may be comma-separated for multi-agent sessions;
				// use only the first agent as a reasonable fallback.
				if idx := strings.Index(sess.AgentID, ","); idx > 0 {
					agentID = sess.AgentID[:idx]
				} else {
					agentID = sess.AgentID
				}
				appName = sess.AppName
			}
		}

		metaJSON := "{}"
		if body.Metadata != nil {
			if b, err := json.Marshal(body.Metadata); err == nil {
				metaJSON = string(b)
			}
		}

		row := store.FeedbackRow{
			SessionID: body.SessionID,
			RequestID: body.RequestID,
			AgentID:   agentID,
			AppName:   appName,
			Outcome:   body.Outcome,
			Score:     body.Score,
			Details:   body.Details,
			Metadata:  metaJSON,
		}
		id, err := db.InsertFeedback(row)
		if err != nil {
			http.Error(w, "failed to store feedback", http.StatusInternalServerError)
			return
		}
		log.Printf("[FEEDBACK] stored #%d agent=%s outcome=%s score=%.2f", id, agentID, body.Outcome, body.Score)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"id":       id,
			"agent_id": agentID,
			"outcome":  body.Outcome,
		})
	})).Methods("POST")

	r.Handle("/api/feedback", authWrap(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		agentID := q.Get("agent_id")
		outcome := q.Get("outcome")
		var from, to int64
		if v, err := strconv.ParseInt(q.Get("from"), 10, 64); err == nil {
			from = v
		}
		if v, err := strconv.ParseInt(q.Get("to"), 10, 64); err == nil {
			to = v
		}
		limit := 50
		offset := 0
		if v, err := strconv.Atoi(q.Get("limit")); err == nil && v > 0 && v <= 200 {
			limit = v
		}
		if v, err := strconv.Atoi(q.Get("offset")); err == nil && v >= 0 {
			offset = v
		}
		items, total, err := db.QueryFeedback(agentID, outcome, from, to, limit, offset)
		if err != nil {
			http.Error(w, "failed to query feedback", http.StatusInternalServerError)
			return
		}
		result := make([]map[string]interface{}, 0, len(items))
		for _, f := range items {
			entry := map[string]interface{}{
				"id":         f.ID,
				"session_id": f.SessionID,
				"request_id": f.RequestID,
				"agent_id":   f.AgentID,
				"app_name":   f.AppName,
				"outcome":    f.Outcome,
				"score":      f.Score,
				"details":    f.Details,
				"created_at": f.CreatedAt.Unix(),
			}
			// Parse metadata JSON back to object for clean API response
			var meta map[string]interface{}
			if json.Unmarshal([]byte(f.Metadata), &meta) == nil && len(meta) > 0 {
				entry["metadata"] = meta
			}
			result = append(result, entry)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"feedback": result,
			"total":    total,
			"limit":    limit,
			"offset":   offset,
		})
	})).Methods("GET")

	r.Handle("/api/analytics/agents/{agent_id}/accuracy", authWrap(func(w http.ResponseWriter, r *http.Request) {
		agentID := mux.Vars(r)["agent_id"]
		sinceDays := 0
		if v, err := strconv.Atoi(r.URL.Query().Get("days")); err == nil && v > 0 {
			sinceDays = v
		}
		acc, err := db.GetAgentAccuracy(agentID, sinceDays)
		if err != nil {
			http.Error(w, "failed to compute accuracy", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"agent_id":     acc.AgentID,
			"total":        acc.TotalCount,
			"success":      acc.SuccessCount,
			"failure":      acc.FailureCount,
			"partial":      acc.PartialCount,
			"avg_score":    acc.AvgScore,
			"success_rate": acc.SuccessRate,
		})
	})).Methods("GET")

	// Prompt Version Tracking API
	r.Handle("/api/prompts/{agent_id}/history", authWrap(func(w http.ResponseWriter, r *http.Request) {
		agentID := mux.Vars(r)["agent_id"]
		q := r.URL.Query()
		limit := 50
		offset := 0
		if v, err := strconv.Atoi(q.Get("limit")); err == nil && v > 0 && v <= 200 {
			limit = v
		}
		if v, err := strconv.Atoi(q.Get("offset")); err == nil && v >= 0 {
			offset = v
		}
		versions, total, err := db.PromptVersionHistory(agentID, limit, offset)
		if err != nil {
			http.Error(w, "failed to query prompt versions", http.StatusInternalServerError)
			return
		}
		items := make([]map[string]interface{}, 0, len(versions))
		for _, v := range versions {
			items = append(items, map[string]interface{}{
				"id":            v.ID,
				"agent_id":      v.AgentID,
				"app_name":      v.AppName,
				"content_hash":  v.ContentHash,
				"previous_hash": v.PreviousHash,
				"provider":      v.Provider,
				"model":         v.Model,
				"created_at":    v.CreatedAt.Unix(),
				"content_len":   len(v.Content),
			})
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"versions": items,
			"total":    total,
			"limit":    limit,
			"offset":   offset,
		})
	})).Methods("GET")

	r.Handle("/api/prompts/{agent_id}/versions/{version_id}", authWrap(func(w http.ResponseWriter, r *http.Request) {
		vars := mux.Vars(r)
		versionID, err := strconv.ParseInt(vars["version_id"], 10, 64)
		if err != nil {
			http.Error(w, "invalid version_id", http.StatusBadRequest)
			return
		}
		v, err := db.PromptVersionByID(versionID)
		if err != nil {
			http.Error(w, "version not found", http.StatusNotFound)
			return
		}
		// Verify the version belongs to the requested agent
		if v.AgentID != vars["agent_id"] {
			http.Error(w, "version not found for this agent", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"id":            v.ID,
			"agent_id":      v.AgentID,
			"app_name":      v.AppName,
			"content_hash":  v.ContentHash,
			"content":       v.Content,
			"previous_hash": v.PreviousHash,
			"provider":      v.Provider,
			"model":         v.Model,
			"created_at":    v.CreatedAt.Unix(),
		})
	})).Methods("GET")

	r.Handle("/api/prompts/{agent_id}/diff/{version_a}/{version_b}", authWrap(func(w http.ResponseWriter, r *http.Request) {
		vars := mux.Vars(r)
		agentID := vars["agent_id"]
		idA, errA := strconv.ParseInt(vars["version_a"], 10, 64)
		idB, errB := strconv.ParseInt(vars["version_b"], 10, 64)
		if errA != nil || errB != nil {
			http.Error(w, "invalid version IDs", http.StatusBadRequest)
			return
		}
		vA, err := db.PromptVersionByID(idA)
		if err != nil || vA.AgentID != agentID {
			http.Error(w, "version A not found", http.StatusNotFound)
			return
		}
		vB, err := db.PromptVersionByID(idB)
		if err != nil || vB.AgentID != agentID {
			http.Error(w, "version B not found", http.StatusNotFound)
			return
		}

		// Simple line-by-line diff for the dashboard
		linesA := strings.Split(vA.Content, "\n")
		linesB := strings.Split(vB.Content, "\n")
		var diffLines []map[string]interface{}
		maxLen := len(linesA)
		if len(linesB) > maxLen {
			maxLen = len(linesB)
		}
		for i := 0; i < maxLen; i++ {
			var lineA, lineB string
			if i < len(linesA) {
				lineA = linesA[i]
			}
			if i < len(linesB) {
				lineB = linesB[i]
			}
			if lineA != lineB {
				if lineA != "" && lineB != "" {
					diffLines = append(diffLines, map[string]interface{}{"type": "changed", "line": i + 1, "old": lineA, "new": lineB})
				} else if lineA == "" {
					diffLines = append(diffLines, map[string]interface{}{"type": "added", "line": i + 1, "new": lineB})
				} else {
					diffLines = append(diffLines, map[string]interface{}{"type": "removed", "line": i + 1, "old": lineA})
				}
			}
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"version_a": map[string]interface{}{
				"id": vA.ID, "content_hash": vA.ContentHash, "created_at": vA.CreatedAt.Unix(),
			},
			"version_b": map[string]interface{}{
				"id": vB.ID, "content_hash": vB.ContentHash, "created_at": vB.CreatedAt.Unix(),
			},
			"changes": len(diffLines),
			"diff":    diffLines,
			"lines_a": len(linesA),
			"lines_b": len(linesB),
		})
	})).Methods("GET")

	// Settings API — protected (can change auth/security settings!)
	r.Handle("/api/settings", authWrap(settingsAPI.HandleGet)).Methods("GET")
	r.Handle("/api/settings", authWrap(settingsAPI.HandlePut)).Methods("PUT")

	// API Key Management — auth-status is public, CRUD is protected
	authAPI.RegisterRoutes(r, authWrap)

	// Static files — no-cache so CSS/JS updates always load fresh in dev
	staticFS := http.FileServer(http.Dir("./dashboard/static"))
	r.PathPrefix("/static/").Handler(http.StripPrefix("/static/", http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
		staticFS.ServeHTTP(w, req)
	})))

	// ── Start ───────────────────────────────────────────────────────────────
	fmt.Printf("🚀 Toko-Mo-Co started on http://localhost:%s\n", cfg.Port)
	fmt.Printf("📊 Dashboard:   http://localhost:%s/\n", cfg.Port)
	fmt.Printf("❤️  Health:      http://localhost:%s/health\n", cfg.Port)
	fmt.Printf("💾 Database:    %s (keep %d days)\n", cfg.DBPath, cfg.DBKeepDays)
	fmt.Printf("🔐 Auth:        %v\n", cfg.AuthEnabled)
	fmt.Printf("💾 Cache:       enabled=%v max=%d ttl=%dm\n", cfg.CacheEnabled, cfg.CacheMaxEntries, cfg.CacheTTLMinutes)
	if memoryStore != nil {
		fmt.Printf("🧠 Memory:      enabled=%v threshold=%.2f max=%d\n", cfg.MemoryEnabled, cfg.MemoryThreshold, cfg.MemoryMaxEntries)
	}
	fmt.Printf("🔌 Endpoints:\n")
	fmt.Printf("   OpenAI:    POST http://localhost:%s/v1/chat/completions\n", cfg.Port)
	fmt.Printf("   Anthropic: POST http://localhost:%s/v1/messages\n", cfg.Port)
	fmt.Printf("   Gemini:    POST http://localhost:%s/v1beta/models/{model}:generateContent\n", cfg.Port)
	for _, name := range providerNames {
		cp := providerStore.Lookup(name)
		if cp != nil {
			fmt.Printf("   %-10s POST http://localhost:%s/v1/chat/completions  (model: %s/<model>)\n",
				cp.DisplayName+":", cfg.Port, cp.Name)
		}
	}

	// ── Graceful shutdown ────────────────────────────────────────────────
	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           r,
		ReadTimeout:       30 * time.Second,
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      120 * time.Second, // generous for streaming responses
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20, // 1MB max header size
	}

	// Listen for SIGINT/SIGTERM in background
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	// Channel to signal fatal listen errors from the server goroutine.
	// Using log.Fatalf inside a goroutine calls os.Exit(1) immediately,
	// bypassing deferred cleanup (db.Close, graceful drain). Instead,
	// send the error to the main goroutine so it can shut down cleanly.
	listenErr := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("[SERVER] listen error: %v", err)
			listenErr <- err
		}
	}()

	// Block until shutdown signal OR fatal listen error
	select {
	case sig := <-quit:
		log.Printf("[SHUTDOWN] received %v — draining in-flight requests...", sig)
	case err := <-listenErr:
		log.Printf("[SHUTDOWN] server failed to listen: %v — shutting down...", err)
	}

	// Stop background goroutines first so they don't access closed resources
	bgCancel()

	// Give in-flight requests up to 15 seconds to finish
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("[SHUTDOWN] forced shutdown: %v", err)
	}

	log.Printf("[SHUTDOWN] clean shutdown complete")
}
