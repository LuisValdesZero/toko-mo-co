package rules

import (
	"encoding/json"
	"log"
	"net/http"
	"strconv"

	"tokomoco/store"

	"github.com/gorilla/mux"
)

// APIHandler provides REST endpoints for managing rules.
// It wraps an Engine and calls Reload() after each mutating operation.
type APIHandler struct {
	engine *Engine
	db     *store.DB // for hit count and request log queries on the requests table
}

// NewAPIHandler creates an APIHandler bound to the given engine.
func NewAPIHandler(engine *Engine, db *store.DB) *APIHandler {
	return &APIHandler{engine: engine, db: db}
}

// RegisterRoutes attaches all CRUD endpoints to the router.
// The authWrap function wraps each handler with API key auth middleware.
// Prefix: /api/rules
func (h *APIHandler) RegisterRoutes(r *mux.Router, authWrap func(http.HandlerFunc) http.Handler) {
	r.Handle("/api/rules", authWrap(h.List)).Methods("GET")
	r.Handle("/api/rules", authWrap(h.Create)).Methods("POST")
	r.Handle("/api/rules/hit-counts", authWrap(h.HitCounts)).Methods("GET")
	r.Handle("/api/rules/templates", authWrap(h.Templates)).Methods("GET")
	r.Handle("/api/rules/from-template", authWrap(h.CreateFromTemplate)).Methods("POST")
	r.Handle("/api/rules/{id:[0-9]+}", authWrap(h.Get)).Methods("GET")
	r.Handle("/api/rules/{id:[0-9]+}", authWrap(h.Update)).Methods("PUT")
	r.Handle("/api/rules/{id:[0-9]+}", authWrap(h.Delete)).Methods("DELETE")
	r.Handle("/api/rules/{id:[0-9]+}/toggle", authWrap(h.Toggle)).Methods("POST")
	r.Handle("/api/rules/{id:[0-9]+}/requests", authWrap(h.RequestLog)).Methods("GET")
}

// ── Handlers ──────────────────────────────────────────────────────────────────

// List returns all rules (enabled and disabled) as JSON.
// GET /api/rules
func (h *APIHandler) List(w http.ResponseWriter, r *http.Request) {
	rules, err := h.engine.store.List()
	if err != nil {
		log.Printf("[API] list rules: %v", err)
		http.Error(w, "failed to list rules", http.StatusInternalServerError)
		return
	}
	// Return empty array instead of null when no rules exist
	if rules == nil {
		rules = []*Rule{}
	}
	respondJSON(w, http.StatusOK, rules)
}

// Get returns a single rule by ID.
// GET /api/rules/{id}
func (h *APIHandler) Get(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		http.Error(w, "invalid rule ID", http.StatusBadRequest)
		return
	}
	rule, err := h.engine.store.GetByID(id)
	if err != nil {
		log.Printf("[API] get rule %d: %v", id, err)
		http.Error(w, "rule not found", http.StatusNotFound)
		return
	}
	respondJSON(w, http.StatusOK, rule)
}

// Create inserts a new rule and triggers engine reload.
// POST /api/rules
// Body: JSON Rule (ID, CreatedAt, UpdatedAt are ignored)
func (h *APIHandler) Create(w http.ResponseWriter, r *http.Request) {
	var rule Rule
	if err := json.NewDecoder(r.Body).Decode(&rule); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	id, err := h.engine.store.Create(&rule)
	if err != nil {
		log.Printf("[API] create rule: %v", err)
		http.Error(w, "failed to create rule", http.StatusInternalServerError)
		return
	}
	h.engine.Reload()
	respondJSON(w, http.StatusCreated, map[string]int64{"id": id})
}

// Update replaces a rule's fields and triggers engine reload.
// PUT /api/rules/{id}
// Body: JSON Rule (ID in the body is ignored; URL id is authoritative)
func (h *APIHandler) Update(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		http.Error(w, "invalid rule ID", http.StatusBadRequest)
		return
	}
	var rule Rule
	if err := json.NewDecoder(r.Body).Decode(&rule); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	rule.ID = id // authoritative ID from URL
	if err := h.engine.store.Update(&rule); err != nil {
		log.Printf("[API] update rule %d: %v", id, err)
		http.Error(w, "failed to update rule", http.StatusInternalServerError)
		return
	}
	h.engine.Reload()
	respondJSON(w, http.StatusOK, map[string]string{"status": "updated"})
}

// Delete removes a rule and triggers engine reload.
// DELETE /api/rules/{id}
func (h *APIHandler) Delete(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		http.Error(w, "invalid rule ID", http.StatusBadRequest)
		return
	}
	if err := h.engine.store.Delete(id); err != nil {
		log.Printf("[API] delete rule %d: %v", id, err)
		http.Error(w, "failed to delete rule", http.StatusInternalServerError)
		return
	}
	h.engine.Reload()
	respondJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// Toggle flips the enabled flag and triggers engine reload.
// POST /api/rules/{id}/toggle
// Body: JSON {"enabled": true|false}
func (h *APIHandler) Toggle(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		http.Error(w, "invalid rule ID", http.StatusBadRequest)
		return
	}
	var body struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if err := h.engine.store.Toggle(id, body.Enabled); err != nil {
		log.Printf("[API] toggle rule %d: %v", id, err)
		http.Error(w, "failed to toggle rule", http.StatusInternalServerError)
		return
	}
	h.engine.Reload()
	respondJSON(w, http.StatusOK, map[string]string{"status": "toggled"})
}

// HitCounts returns rule trigger counts per rule.
// GET /api/rules/hit-counts?days=30
func (h *APIHandler) HitCounts(w http.ResponseWriter, r *http.Request) {
	daysStr := r.URL.Query().Get("days")
	days := 30 // default: last 30 days
	if daysStr != "" {
		if d, err := strconv.Atoi(daysStr); err == nil && d >= 0 {
			days = d
		}
	}

	counts, err := h.db.RuleHitCounts(days)
	if err != nil {
		log.Printf("[API] rule hit counts: %v", err)
		http.Error(w, "failed to fetch hit counts", http.StatusInternalServerError)
		return
	}

	// Build a map for O(1) lookup on the frontend: ruleID -> {count, last_triggered}
	result := make(map[string]interface{})
	for _, c := range counts {
		result[strconv.FormatInt(c.RuleID, 10)] = map[string]interface{}{
			"count":          c.Count,
			"last_triggered": c.LastTriggered.Unix(),
		}
	}

	respondJSON(w, http.StatusOK, result)
}

// RequestLog returns paginated requests that matched a specific rule.
// GET /api/rules/{id}/requests?limit=20&offset=0
func (h *APIHandler) RequestLog(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		http.Error(w, "invalid rule ID", http.StatusBadRequest)
		return
	}

	limit := 20
	offset := 0
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 && n <= 100 {
			limit = n
		}
	}
	if o := r.URL.Query().Get("offset"); o != "" {
		if n, err := strconv.Atoi(o); err == nil && n >= 0 {
			offset = n
		}
	}

	rows, total, err := h.db.RuleRequests(id, limit, offset)
	if err != nil {
		log.Printf("[API] rule request log %d: %v", id, err)
		http.Error(w, "failed to fetch request log", http.StatusInternalServerError)
		return
	}

	type requestEntry struct {
		ID            int64   `json:"id"`
		Timestamp     int64   `json:"timestamp"`
		AgentID       string  `json:"agent_id"`
		AppName       string  `json:"app_name"`
		Provider      string  `json:"provider"`
		Model         string  `json:"model"`
		PromptPreview string  `json:"prompt_preview"`
		InputTokens   int     `json:"input_tokens"`
		OutputTokens  int     `json:"output_tokens"`
		Cost          float64 `json:"cost"`
		LatencyMs     int64   `json:"latency_ms"`
		StatusCode    int     `json:"status_code"`
		ErrorMessage  string  `json:"error_message"`
	}

	entries := make([]requestEntry, len(rows))
	for i, row := range rows {
		entries[i] = requestEntry{
			ID:            row.ID,
			Timestamp:     row.Timestamp.Unix(),
			AgentID:       row.AgentID,
			AppName:       row.AppName,
			Provider:      row.Provider,
			Model:         row.Model,
			PromptPreview: row.PromptPreview,
			InputTokens:   row.InputTokens,
			OutputTokens:  row.OutputTokens,
			Cost:          row.Cost,
			LatencyMs:     row.LatencyMs,
			StatusCode:    row.StatusCode,
			ErrorMessage:  row.ErrorMessage,
		}
	}

	response := map[string]interface{}{
		"requests": entries,
		"total":    total,
		"limit":    limit,
		"offset":   offset,
	}

	respondJSON(w, http.StatusOK, response)
}

// Templates returns all built-in rule templates.
// GET /api/rules/templates
func (h *APIHandler) Templates(w http.ResponseWriter, r *http.Request) {
	templates := BuiltinTemplates()

	// Group by category for frontend convenience
	type categoryGroup struct {
		Category  string         `json:"category"`
		Templates []RuleTemplate `json:"templates"`
	}

	groupMap := make(map[string][]RuleTemplate)
	order := []string{}
	for _, t := range templates {
		if _, exists := groupMap[t.Category]; !exists {
			order = append(order, t.Category)
		}
		groupMap[t.Category] = append(groupMap[t.Category], t)
	}

	groups := make([]categoryGroup, 0, len(order))
	for _, cat := range order {
		groups = append(groups, categoryGroup{Category: cat, Templates: groupMap[cat]})
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"templates": templates,
		"groups":    groups,
	})
}

// CreateFromTemplate creates a new rule pre-filled from a template.
// POST /api/rules/from-template
// Body: {"template_id": "cost-guard-daily", "overrides": {"scope_agent_id": "my-agent", ...}}
func (h *APIHandler) CreateFromTemplate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		TemplateID string                 `json:"template_id"`
		Overrides  map[string]interface{} `json:"overrides"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}

	// Find the template
	templates := BuiltinTemplates()
	var tmpl *RuleTemplate
	for i := range templates {
		if templates[i].ID == body.TemplateID {
			tmpl = &templates[i]
			break
		}
	}
	if tmpl == nil {
		http.Error(w, "unknown template ID", http.StatusBadRequest)
		return
	}

	// Build rule from template — deep-copy conditions slice
	conditions := make([]ConditionSpec, len(tmpl.Conditions))
	copy(conditions, tmpl.Conditions)

	rule := Rule{
		Name:        tmpl.Name,
		Enabled:     true,
		Priority:    tmpl.Priority,
		Conditions:  conditions,
		Action:      tmpl.Action,
		Description: tmpl.Description,
		Evidence:    "Created from template: " + tmpl.ID,
	}

	// Apply overrides
	if v, ok := body.Overrides["name"].(string); ok && v != "" {
		rule.Name = v
	}
	if v, ok := body.Overrides["description"].(string); ok {
		rule.Description = v
	}
	if v, ok := body.Overrides["evidence"].(string); ok {
		rule.Evidence = v
	}
	if v, ok := body.Overrides["scope_agent_id"].(string); ok {
		rule.ScopeAgentID = v
	}
	if v, ok := body.Overrides["priority"].(float64); ok {
		rule.Priority = int(v)
	}

	// Condition-level overrides
	if v, ok := body.Overrides["threshold"].(float64); ok && len(rule.Conditions) > 0 {
		rule.Conditions[0].Threshold = v
	}
	if v, ok := body.Overrides["value"].(string); ok && len(rule.Conditions) > 0 {
		rule.Conditions[0].Value = v
	}
	if v, ok := body.Overrides["mode"].(string); ok && len(rule.Conditions) > 0 {
		rule.Conditions[0].Mode = MatchMode(v)
	}
	if v, ok := body.Overrides["window_sec"].(float64); ok && len(rule.Conditions) > 0 {
		rule.Conditions[0].WindowSec = int(v)
	}

	// Action-level overrides
	if v, ok := body.Overrides["block_message"].(string); ok {
		rule.Action.BlockMessage = v
	}
	if v, ok := body.Overrides["override_model"].(string); ok {
		rule.Action.OverrideModel = v
	}
	if v, ok := body.Overrides["injected_system_prompt"].(string); ok {
		rule.Action.InjectedSystemPrompt = v
	}
	if v, ok := body.Overrides["redirect_url"].(string); ok {
		rule.Action.RedirectURL = v
	}
	if v, ok := body.Overrides["rate_limit_requests"].(float64); ok {
		rule.Action.RateLimitRequests = int(v)
	}

	id, err := h.engine.store.Create(&rule)
	if err != nil {
		log.Printf("[API] create rule from template: %v", err)
		http.Error(w, "failed to create rule", http.StatusInternalServerError)
		return
	}
	h.engine.Reload()
	respondJSON(w, http.StatusCreated, map[string]interface{}{
		"id":          id,
		"template_id": tmpl.ID,
	})
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func parseID(r *http.Request) (int64, error) {
	vars := mux.Vars(r)
	return strconv.ParseInt(vars["id"], 10, 64)
}

func respondJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
