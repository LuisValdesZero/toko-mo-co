package providers

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/mux"
)

// API provides REST endpoints for managing custom providers.
type API struct {
	store *ProviderStore
}

// NewAPI creates a new provider API handler.
func NewAPI(store *ProviderStore) *API {
	return &API{store: store}
}

// RegisterRoutes registers all custom provider management routes.
// The authWrap function wraps each handler with API key auth middleware.
func (api *API) RegisterRoutes(r *mux.Router, authWrap func(http.HandlerFunc) http.Handler) {
	r.Handle("/api/providers", authWrap(api.HandleList)).Methods("GET")
	r.Handle("/api/providers", authWrap(api.HandleCreate)).Methods("POST")
	r.Handle("/api/providers/test", authWrap(api.HandleTest)).Methods("POST")
	r.Handle("/api/providers/{id:[0-9]+}", authWrap(api.HandleGet)).Methods("GET")
	r.Handle("/api/providers/{id:[0-9]+}", authWrap(api.HandleUpdate)).Methods("PUT")
	r.Handle("/api/providers/{id:[0-9]+}", authWrap(api.HandleDelete)).Methods("DELETE")
	r.Handle("/api/providers/{id:[0-9]+}/toggle", authWrap(api.HandleToggle)).Methods("POST")
	r.Handle("/api/providers/{id:[0-9]+}/test", authWrap(api.HandleTestByID)).Methods("POST")
}

// HandleList returns all custom providers (enabled and disabled).
func (api *API) HandleList(w http.ResponseWriter, r *http.Request) {
	providers, err := api.store.All()
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusInternalServerError)
		return
	}
	if providers == nil {
		providers = []*CustomProvider{}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(providers)
}

// HandleGet returns a single custom provider by ID.
func (api *API) HandleGet(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(mux.Vars(r)["id"], 10, 64)
	cp, err := api.store.Get(id)
	if err != nil {
		http.Error(w, `{"error":"provider not found"}`, http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(cp)
}

// HandleCreate creates a new custom provider.
func (api *API) HandleCreate(w http.ResponseWriter, r *http.Request) {
	cp, err := parseProviderFromBody(r)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusBadRequest)
		return
	}

	id, err := api.store.Create(cp)
	if err != nil {
		status := http.StatusInternalServerError
		if strings.Contains(err.Error(), "validation failed") {
			status = http.StatusBadRequest
		}
		if strings.Contains(err.Error(), "UNIQUE constraint") {
			status = http.StatusConflict
			err = fmt.Errorf("a provider with name %q already exists", cp.Name)
		}
		http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), status)
		return
	}

	cp.ID = id
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(cp)
}

// HandleUpdate modifies an existing custom provider.
func (api *API) HandleUpdate(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(mux.Vars(r)["id"], 10, 64)

	// Verify it exists
	existing, err := api.store.Get(id)
	if err != nil {
		http.Error(w, `{"error":"provider not found"}`, http.StatusNotFound)
		return
	}

	cp, err := parseProviderFromBody(r)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusBadRequest)
		return
	}

	cp.ID = id
	// Name is immutable after creation — use existing name
	cp.Name = existing.Name

	if err := api.store.Update(cp); err != nil {
		status := http.StatusInternalServerError
		if strings.Contains(err.Error(), "validation failed") {
			status = http.StatusBadRequest
		}
		http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), status)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(cp)
}

// HandleDelete removes a custom provider.
func (api *API) HandleDelete(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(mux.Vars(r)["id"], 10, 64)
	if err := api.store.Delete(id); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"ok":true}`)
}

// HandleToggle enables or disables a custom provider.
func (api *API) HandleToggle(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(mux.Vars(r)["id"], 10, 64)

	var body struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, `{"error":"invalid JSON body"}`, http.StatusBadRequest)
		return
	}

	if err := api.store.Toggle(id, body.Enabled); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"ok":true,"enabled":%v}`, body.Enabled)
}

// TestResult holds the result of a connection test.
type TestResult struct {
	Reachable bool     `json:"reachable"`
	LatencyMs int64    `json:"latency_ms"`
	Error     string   `json:"error,omitempty"`
	Models    []string `json:"models,omitempty"`
	Note      string   `json:"note,omitempty"`
}

// HandleTest tests a connection to a custom provider using the provided config (not saved).
func (api *API) HandleTest(w http.ResponseWriter, r *http.Request) {
	var body struct {
		BaseURL    string `json:"base_url"`
		APIPath    string `json:"api_path"`
		AuthHeader string `json:"auth_header"`
		AuthEnvVar string `json:"auth_env_var"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, `{"error":"invalid JSON body"}`, http.StatusBadRequest)
		return
	}

	// Resolve auth header from env var if provided
	authHeader := body.AuthHeader
	if body.AuthEnvVar != "" {
		if val := os.Getenv(body.AuthEnvVar); val != "" {
			if strings.HasPrefix(val, "Bearer ") {
				authHeader = val
			} else {
				authHeader = "Bearer " + val
			}
		}
	}

	result := testConnection(body.BaseURL, authHeader)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

// HandleTestByID tests the connection to an existing saved provider.
func (api *API) HandleTestByID(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(mux.Vars(r)["id"], 10, 64)
	cp, err := api.store.Get(id)
	if err != nil {
		http.Error(w, `{"error":"provider not found"}`, http.StatusNotFound)
		return
	}

	authHeader := cp.ResolveAuthHeader()
	result := testConnection(cp.BaseURL, authHeader)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

// testConnection probes a provider endpoint to check connectivity and discover models.
func testConnection(baseURL, authHeader string) TestResult {
	baseURL = strings.TrimRight(baseURL, "/")
	client := &http.Client{Timeout: 5 * time.Second}

	// Try GET /v1/models first — works for Ollama, vLLM, most OpenAI-compatible endpoints
	modelsURL := baseURL + "/v1/models"
	req, err := http.NewRequest("GET", modelsURL, nil)
	if err != nil {
		return TestResult{Reachable: false, Error: fmt.Sprintf("invalid URL: %v", err)}
	}
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}

	start := time.Now()
	resp, err := client.Do(req)
	latency := time.Since(start).Milliseconds()

	if err != nil {
		// Try plain GET on base URL as fallback
		req2, err2 := http.NewRequest("GET", baseURL, nil)
		if err2 != nil {
			return TestResult{Reachable: false, Error: fmt.Sprintf("invalid base URL: %v", err2), LatencyMs: latency}
		}
		if authHeader != "" {
			req2.Header.Set("Authorization", authHeader)
		}

		start2 := time.Now()
		resp2, err2 := client.Do(req2)
		latency2 := time.Since(start2).Milliseconds()

		if err2 != nil {
			return TestResult{Reachable: false, Error: fmt.Sprintf("connection failed: %v", err), LatencyMs: latency}
		}
		resp2.Body.Close()
		return TestResult{
			Reachable: true,
			LatencyMs: latency2,
			Note:      "Base URL reachable but /v1/models not available. Models must be entered manually.",
		}
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return TestResult{
			Reachable: true,
			LatencyMs: latency,
			Note:      fmt.Sprintf("/v1/models returned status %d. Base URL is reachable.", resp.StatusCode),
		}
	}

	// Parse OpenAI-format model list response
	models := parseModelListResponse(resp)

	return TestResult{
		Reachable: true,
		LatencyMs: latency,
		Models:    models,
	}
}

// parseModelListResponse extracts model IDs from an OpenAI-format /v1/models response.
func parseModelListResponse(resp *http.Response) []string {
	bodyBytes, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 1MB max
	if err != nil {
		return nil
	}

	var data struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
		Models []struct {
			Name  string `json:"name"`
			Model string `json:"model"`
		} `json:"models"`
	}

	if err := json.Unmarshal(bodyBytes, &data); err != nil {
		return nil
	}

	var models []string
	// OpenAI format: { "data": [{"id": "model-name"}] }
	for _, m := range data.Data {
		if m.ID != "" {
			models = append(models, m.ID)
		}
	}
	// Ollama format: { "models": [{"name": "llama3.2"}] }
	if len(models) == 0 {
		for _, m := range data.Models {
			if m.Name != "" {
				// Ollama includes ":latest" suffix — strip it for cleaner display
				name := m.Name
				if strings.HasSuffix(name, ":latest") {
					name = strings.TrimSuffix(name, ":latest")
				}
				models = append(models, name)
			} else if m.Model != "" {
				models = append(models, m.Model)
			}
		}
	}

	return models
}

// parseProviderFromBody reads and parses a CustomProvider from the request body.
func parseProviderFromBody(r *http.Request) (*CustomProvider, error) {
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read body: %w", err)
	}

	cp := &CustomProvider{}
	if err := json.Unmarshal(bodyBytes, cp); err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}

	// Normalize name to lowercase
	cp.Name = strings.ToLower(strings.TrimSpace(cp.Name))

	// Set defaults
	if cp.APIFormat == "" {
		cp.APIFormat = "openai"
	}
	if cp.APIPath == "" {
		cp.APIPath = "/v1/chat/completions"
	}
	if cp.Models == nil {
		cp.Models = []string{}
	}

	return cp, nil
}

