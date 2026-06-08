// Package nemoguardrails is a thin client for the self-hosted Aratiri NeMo
// Guardrails service (a FastAPI wrapper around NeMo Guardrails), distinct from the
// NVIDIA NeMo Guard NIM client in package nemoguard.
//
// Contract (guardrails.service.consul:18030):
//
//	POST {baseURL}{inputPath}   (inputPath defaults to /guard/input)
//	  Body:     {"messages":[{"role","content"},...], "caller":"<id>"}
//	  Response: {"blocked":bool, "violation_type":str|null, "categories":[str],
//	             "language":str|null, "reason":str, "source":str}
//
//	POST {baseURL}{outputPath}  (outputPath defaults to /guard/output)
//	  Body:     {"text":"<assistant reply>", "caller":"<id>"}
//	  Response: {"flagged":bool, "categories":[str], "reason":str,
//	             "masked_text":str|null}
//
// The client is optional and env-gated by the caller (CONFIG_NEMOGUARDRAILS_URL).
// Input is fail-closed by policy at the call site; output is fail-open. On any
// transport/decode error these methods return an error and let the caller decide.
package nemoguardrails

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"
)

// InputVerdict is the parsed /guard/input response.
type InputVerdict struct {
	Blocked       bool     `json:"blocked"`
	ViolationType string   `json:"violation_type"`
	Categories    []string `json:"categories"`
	Language      string   `json:"language"`
	Reason        string   `json:"reason"`
	Source        string   `json:"source"`
}

// OutputVerdict is the parsed /guard/output response.
type OutputVerdict struct {
	Flagged    bool     `json:"flagged"`
	Categories []string `json:"categories"`
	Reason     string   `json:"reason"`
	MaskedText string   `json:"masked_text"`
}

// Stats is an atomic snapshot of client activity (live/uptime metrics).
type Stats struct {
	Enabled       bool   `json:"enabled"`
	Mode          string `json:"mode"`
	InputChecks   int64  `json:"input_checks"`
	OutputChecks  int64  `json:"output_checks"`
	InputBlocked  int64  `json:"input_blocked"`
	OutputFlagged int64  `json:"output_flagged"`
	Errors        int64  `json:"errors"`
}

// RuleSpec is one operator-authored Colang rail — the control-plane contract with
// the guardrails service's /config/rules CRUD API (server.py RuleModel). The proxy
// authors these from the Rule editor and pushes them here; the service compiles them
// to Colang and hot-reloads. block_terms uses params{"terms":[...]}, block_regex uses
// params{"pattern":"..."}, custom uses a raw Colang flow body.
type RuleSpec struct {
	Name     string         `json:"name"`
	Kind     string         `json:"kind"`      // "input" | "output"
	RailType string         `json:"rail_type"` // "custom" | "block_terms" | "block_regex"
	Params   map[string]any `json:"params,omitempty"`
	Colang   string         `json:"colang,omitempty"`
	Enabled  bool           `json:"enabled"`
	Priority int            `json:"priority,omitempty"`
}

// ErrNotFound is returned by GetRule/DeleteRule when the named rail does not exist
// (HTTP 404). DeleteRule swallows it so deletes are idempotent.
var ErrNotFound = errors.New("nemoguardrails: rule not found")

// Client calls the NeMo Guardrails service.
type Client struct {
	baseURL    string
	inputPath  string
	outputPath string
	configPath string // control-plane CRUD collection path (default /config/rules)
	apiKey     string
	mode       string // "block" | "flag"
	caller     string
	httpClient *http.Client

	inputChecks   atomic.Int64
	outputChecks  atomic.Int64
	inputBlocked  atomic.Int64
	outputFlagged atomic.Int64
	errors        atomic.Int64
}

// New builds a Client. inputPath/outputPath default to /guard/input + /guard/output,
// mode to "block", caller to "toko-mo-co".
func New(baseURL, inputPath, outputPath, apiKey, mode, caller string, timeout time.Duration) *Client {
	if inputPath == "" {
		inputPath = "/guard/input"
	}
	if outputPath == "" {
		outputPath = "/guard/output"
	}
	if mode != "flag" {
		mode = "block"
	}
	if caller == "" {
		caller = "toko-mo-co"
	}
	if timeout <= 0 {
		timeout = 12 * time.Second
	}
	return &Client{
		baseURL:    baseURL,
		inputPath:  inputPath,
		outputPath: outputPath,
		configPath: "/config/rules",
		apiKey:     apiKey,
		mode:       mode,
		caller:     caller,
		httpClient: &http.Client{Timeout: timeout},
	}
}

// Mode returns "block" or "flag".
func (c *Client) Mode() string { return c.mode }

// SetConfigPath overrides the control-plane CRUD collection path (default
// /config/rules). Empty is ignored.
func (c *Client) SetConfigPath(p string) {
	if p != "" {
		c.configPath = p
	}
}

func (c *Client) post(ctx context.Context, path string, payload any) ([]byte, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		c.errors.Add(1)
		return nil, err
	}
	url := strings.TrimRight(c.baseURL, "/") + path
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		c.errors.Add(1)
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		c.errors.Add(1)
		return nil, fmt.Errorf("nemoguardrails request: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		c.errors.Add(1)
		return nil, fmt.Errorf("nemoguardrails %s returned %d: %s", path, resp.StatusCode, truncate(string(respBody), 200))
	}
	return respBody, nil
}

// GuardInput runs the triage rails on the conversation. messages is the raw
// {role,content,...} list; only role + content are forwarded.
func (c *Client) GuardInput(ctx context.Context, messages []map[string]any) (InputVerdict, error) {
	c.inputChecks.Add(1)
	payload := map[string]any{"messages": toMsgPayload(messages), "caller": c.caller}
	respBody, err := c.post(ctx, c.inputPath, payload)
	if err != nil {
		return InputVerdict{}, err
	}
	var v InputVerdict
	if err := json.Unmarshal(respBody, &v); err != nil {
		c.errors.Add(1)
		return InputVerdict{}, fmt.Errorf("nemoguardrails input decode: %w", err)
	}
	if v.Blocked {
		c.inputBlocked.Add(1)
	}
	return v, nil
}

// GuardOutput runs the output rails (moderation + PII mask) on an assistant reply.
func (c *Client) GuardOutput(ctx context.Context, text string) (OutputVerdict, error) {
	c.outputChecks.Add(1)
	respBody, err := c.post(ctx, c.outputPath, map[string]any{"text": text, "caller": c.caller})
	if err != nil {
		return OutputVerdict{}, err
	}
	var v OutputVerdict
	if err := json.Unmarshal(respBody, &v); err != nil {
		c.errors.Add(1)
		return OutputVerdict{}, fmt.Errorf("nemoguardrails output decode: %w", err)
	}
	if v.Flagged {
		c.outputFlagged.Add(1)
	}
	return v, nil
}

// Stats returns an atomic snapshot.
func (c *Client) Stats() Stats {
	return Stats{
		Enabled:       true,
		Mode:          c.mode,
		InputChecks:   c.inputChecks.Load(),
		OutputChecks:  c.outputChecks.Load(),
		InputBlocked:  c.inputBlocked.Load(),
		OutputFlagged: c.outputFlagged.Load(),
		Errors:        c.errors.Load(),
	}
}

// ── Control plane (CRUD on authored rails) ───────────────────────────────────
// These hit the guardrails service's /config/rules API, gated by the
// X-Internal-Key header (GUARDRAILS_INTERNAL_KEY) — distinct from the /guard
// endpoints, which are unauthenticated. The shared key is carried in apiKey.

func (c *Client) rulePath(name string) string {
	return strings.TrimRight(c.configPath, "/") + "/" + url.PathEscape(name)
}

// configReq performs one control-plane request. payload/out may be nil. A 404 is
// surfaced as ErrNotFound so callers can treat get/delete idempotently.
func (c *Client) configReq(ctx context.Context, method, path string, payload, out any) error {
	var body io.Reader
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.baseURL, "/")+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.apiKey != "" {
		req.Header.Set("X-Internal-Key", c.apiKey)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		c.errors.Add(1)
		return fmt.Errorf("nemoguardrails config %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusNotFound {
		return ErrNotFound
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		c.errors.Add(1)
		return fmt.Errorf("nemoguardrails config %s %s returned %d: %s", method, path, resp.StatusCode, truncate(string(respBody), 300))
	}
	if out != nil && len(respBody) > 0 {
		if err := json.Unmarshal(respBody, out); err != nil {
			return fmt.Errorf("nemoguardrails config decode: %w", err)
		}
	}
	return nil
}

// PutRule upserts a rail (PUT /config/rules/{name}). The service validates the
// generated Colang, persists it, and hot-reloads LLMRails; it returns the stored rule.
func (c *Client) PutRule(ctx context.Context, rule RuleSpec) (RuleSpec, error) {
	if rule.Name == "" {
		return RuleSpec{}, fmt.Errorf("nemoguardrails: rule name required")
	}
	var out RuleSpec
	if err := c.configReq(ctx, http.MethodPut, c.rulePath(rule.Name), rule, &out); err != nil {
		return RuleSpec{}, err
	}
	return out, nil
}

// ListRules returns every authored rail (the service's compiled rule set).
func (c *Client) ListRules(ctx context.Context) ([]RuleSpec, error) {
	var out struct {
		Rules []RuleSpec `json:"rules"`
	}
	if err := c.configReq(ctx, http.MethodGet, c.configPath, nil, &out); err != nil {
		return nil, err
	}
	return out.Rules, nil
}

// GetRule returns one rail by name, or (nil, nil) when it does not exist.
func (c *Client) GetRule(ctx context.Context, name string) (*RuleSpec, error) {
	var out RuleSpec
	err := c.configReq(ctx, http.MethodGet, c.rulePath(name), nil, &out)
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// DeleteRule removes a rail. A missing rail is treated as success (idempotent).
func (c *Client) DeleteRule(ctx context.Context, name string) error {
	err := c.configReq(ctx, http.MethodDelete, c.rulePath(name), nil, nil)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

// toMsgPayload reduces arbitrary message maps to {role, content} the service expects.
func toMsgPayload(messages []map[string]any) []map[string]string {
	out := make([]map[string]string, 0, len(messages))
	for _, m := range messages {
		role, _ := m["role"].(string)
		content, _ := m["content"].(string)
		if role == "" && content == "" {
			continue
		}
		out = append(out, map[string]string{"role": role, "content": content})
	}
	return out
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
