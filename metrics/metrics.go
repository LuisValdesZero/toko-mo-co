// Package metrics exposes Prometheus instrumentation for the proxy: per-request
// counters/histograms (token usage, cost, latency, cache hits, errors, and the
// value-add signals the proxy already computes) plus a scrape-time collector for
// response-cache stats. All metrics use the default registry, so the standard
// go_* and process_* collectors are exported alongside them.
package metrics

import (
	"net/http"
	"strconv"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Per-request metrics. agent_id/app_name are kept off the latency histogram on
// purpose — histograms multiply by bucket count, so high-cardinality labels there
// are the main TSDB risk. The agent set is bounded, so they are fine on counters.
var (
	requestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "tokomoco_requests_total",
		Help: "Total proxy requests by provider, model, agent, app, cache-hit and HTTP status class.",
	}, []string{"provider", "model", "agent_id", "app_name", "cache_hit", "status_class"})

	tokensTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "tokomoco_tokens_total",
		Help: "Total tokens processed, by direction (input|output).",
	}, []string{"provider", "model", "agent_id", "app_name", "direction"})

	costUSDTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "tokomoco_cost_usd_total",
		Help: "Cumulative request cost in USD.",
	}, []string{"provider", "model", "agent_id", "app_name"})

	requestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "tokomoco_request_duration_seconds",
		Help:    "Request latency in seconds (upstream call + proxy overhead).",
		Buckets: []float64{0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10, 30, 60, 120},
	}, []string{"provider", "model", "cache_hit"})

	fallbacksTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "tokomoco_fallbacks_total",
		Help: "Requests served via a fallback provider/model.",
	}, []string{"provider", "model"})

	loopsDetectedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "tokomoco_loops_detected_total",
		Help: "Requests where loop detection fired.",
	}, []string{"provider", "model"})

	piiRedactionsTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "tokomoco_pii_redactions_total",
		Help: "Total PII spans redacted across all requests.",
	})

	jailbreakDetectionsTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "tokomoco_jailbreak_detections_total",
		Help: "Requests flagged as jailbreak / prompt-injection attempts.",
	})
)

// RecordRequest emits all per-request metrics. Called once per request (including
// cache hits) from the proxy's persistAndBroadcast hook.
func RecordRequest(provider, model, agentID, appName string, inputTokens, outputTokens int, cost float64, latencyMs int64, statusCode int, cacheHit, fallback, loop bool, piiCount int, jailbreak bool) {
	cacheLabel := strconv.FormatBool(cacheHit)
	statusClass := classifyStatus(statusCode)

	requestsTotal.WithLabelValues(provider, model, agentID, appName, cacheLabel, statusClass).Inc()
	if inputTokens > 0 {
		tokensTotal.WithLabelValues(provider, model, agentID, appName, "input").Add(float64(inputTokens))
	}
	if outputTokens > 0 {
		tokensTotal.WithLabelValues(provider, model, agentID, appName, "output").Add(float64(outputTokens))
	}
	if cost > 0 {
		costUSDTotal.WithLabelValues(provider, model, agentID, appName).Add(cost)
	}
	requestDuration.WithLabelValues(provider, model, cacheLabel).Observe(float64(latencyMs) / 1000.0)

	if fallback {
		fallbacksTotal.WithLabelValues(provider, model).Inc()
	}
	if loop {
		loopsDetectedTotal.WithLabelValues(provider, model).Inc()
	}
	if piiCount > 0 {
		piiRedactionsTotal.Add(float64(piiCount))
	}
	if jailbreak {
		jailbreakDetectionsTotal.Inc()
	}
}

// classifyStatus buckets HTTP status codes into a low-cardinality label. 0 (no
// upstream response — network/proxy error) maps to "err".
func classifyStatus(code int) string {
	switch {
	case code >= 200 && code < 300:
		return "2xx"
	case code >= 300 && code < 400:
		return "3xx"
	case code >= 400 && code < 500:
		return "4xx"
	case code >= 500:
		return "5xx"
	default:
		return "err"
	}
}

// Handler returns the Prometheus exposition HTTP handler over the default registry.
func Handler() http.Handler {
	return promhttp.Handler()
}

// CacheStatsFn returns a snapshot of response-cache stats, read at scrape time.
type CacheStatsFn func() (entries, maxEntries int, hits, misses, tokensSaved int64, hitRate, costSaved float64)

// RegisterCacheCollector registers a collector that reports response-cache gauges
// and counters by calling fn on every scrape. Kept as a plain-primitive callback so
// this package does not import the cache package (avoids an import cycle).
func RegisterCacheCollector(fn CacheStatsFn) {
	prometheus.MustRegister(&cacheCollector{fn: fn})
}

type cacheCollector struct{ fn CacheStatsFn }

var (
	cacheEntriesDesc     = prometheus.NewDesc("tokomoco_cache_entries", "Current entries held in the response cache.", nil, nil)
	cacheMaxEntriesDesc  = prometheus.NewDesc("tokomoco_cache_max_entries", "Maximum entries the response cache holds.", nil, nil)
	cacheHitsDesc        = prometheus.NewDesc("tokomoco_cache_hits_total", "Response-cache hits.", nil, nil)
	cacheMissesDesc      = prometheus.NewDesc("tokomoco_cache_misses_total", "Response-cache misses.", nil, nil)
	cacheHitRateDesc     = prometheus.NewDesc("tokomoco_cache_hit_rate", "Response-cache hit rate (0-1).", nil, nil)
	cacheTokensSavedDesc = prometheus.NewDesc("tokomoco_cache_tokens_saved_total", "Tokens saved by serving from the response cache.", nil, nil)
	cacheCostSavedDesc   = prometheus.NewDesc("tokomoco_cache_cost_saved_usd_total", "USD saved by serving from the response cache.", nil, nil)
)

func (c *cacheCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- cacheEntriesDesc
	ch <- cacheMaxEntriesDesc
	ch <- cacheHitsDesc
	ch <- cacheMissesDesc
	ch <- cacheHitRateDesc
	ch <- cacheTokensSavedDesc
	ch <- cacheCostSavedDesc
}

func (c *cacheCollector) Collect(ch chan<- prometheus.Metric) {
	entries, maxEntries, hits, misses, tokensSaved, hitRate, costSaved := c.fn()
	ch <- prometheus.MustNewConstMetric(cacheEntriesDesc, prometheus.GaugeValue, float64(entries))
	ch <- prometheus.MustNewConstMetric(cacheMaxEntriesDesc, prometheus.GaugeValue, float64(maxEntries))
	ch <- prometheus.MustNewConstMetric(cacheHitsDesc, prometheus.CounterValue, float64(hits))
	ch <- prometheus.MustNewConstMetric(cacheMissesDesc, prometheus.CounterValue, float64(misses))
	ch <- prometheus.MustNewConstMetric(cacheHitRateDesc, prometheus.GaugeValue, hitRate)
	ch <- prometheus.MustNewConstMetric(cacheTokensSavedDesc, prometheus.CounterValue, float64(tokensSaved))
	ch <- prometheus.MustNewConstMetric(cacheCostSavedDesc, prometheus.CounterValue, costSaved)
}
