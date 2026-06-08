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
	"fmt"
	"io"
	"net/http"
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

// Client calls the NeMo Guardrails service.
type Client struct {
	baseURL    string
	inputPath  string
	outputPath string
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
		apiKey:     apiKey,
		mode:       mode,
		caller:     caller,
		httpClient: &http.Client{Timeout: timeout},
	}
}

// Mode returns "block" or "flag".
func (c *Client) Mode() string { return c.mode }

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
