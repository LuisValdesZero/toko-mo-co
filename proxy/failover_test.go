package proxy

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"tokomoco/config"
)

// newTestHandler builds a minimal Handler for exercising executeUpstreamRequest:
// retries + tier-fallback disabled so only the provider failover chain logic runs.
func newTestHandler() *Handler {
	return &Handler{
		cfg:        &config.Config{RetryEnabled: false, RetryMaxAttempts: 1, FallbackEnabled: false},
		httpClient: http.DefaultClient,
	}
}

func statusServer(t *testing.T, code int) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(code)
		_, _ = w.Write([]byte(`{"status":` + http.StatusText(code) + `}`))
	}))
	t.Cleanup(s.Close)
	return s
}

func TestProviderFailoverCascadesOn4xx(t *testing.T) {
	h := newTestHandler()
	body := []byte(`{"model":"m1"}`)

	// Primary 400, chain provider 200 → cascade to the chain and win.
	primary := statusServer(t, http.StatusBadRequest)
	good := statusServer(t, http.StatusOK)
	resp, status, err, _, fp, fm, _ := h.executeUpstreamRequest(
		"agent", "primary", "m1", primary.URL, body, http.Header{},
		[]providerAttempt{{name: "good", url: good.URL, model: "m2"}},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != 200 || fp != "good" || fm != "m2" {
		t.Fatalf("expected 200 via good/m2, got status=%d fp=%q fm=%q", status, fp, fm)
	}
	if resp != nil {
		resp.Body.Close()
	}

	// Primary 400, the only chain provider also 404 → return the last failed status
	// (404), never a nil response.
	bad := statusServer(t, http.StatusNotFound)
	resp2, status2, err2, _, _, _, _ := h.executeUpstreamRequest(
		"agent", "primary", "m1", primary.URL, body, http.Header{},
		[]providerAttempt{{name: "bad", url: bad.URL, model: "m3"}},
	)
	if err2 != nil {
		t.Fatalf("unexpected error: %v", err2)
	}
	if resp2 == nil || status2 != 404 {
		t.Fatalf("expected last-failed 404, got status=%d resp=%v", status2, resp2)
	}
	resp2.Body.Close()

	// No chain → a 4xx is returned to the client as-is (not cascaded).
	resp3, status3, _, _, _, _, _ := h.executeUpstreamRequest(
		"agent", "primary", "m1", primary.URL, body, http.Header{}, nil,
	)
	if status3 != 400 {
		t.Fatalf("expected 400 passthrough with no chain, got %d", status3)
	}
	if resp3 != nil {
		resp3.Body.Close()
	}
}
