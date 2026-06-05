// Package nemoguard is a thin client for an NVIDIA NeMo Guard jailbreak-detection
// NIM (nemoguard-jailbreak-detect) served behind an HTTP API.
//
// Contract (matches NeMo-Guardrails' own NIM client):
//
//	POST {baseURL}{classifyPath}        (classifyPath defaults to /v1/classify)
//	Headers: Content-Type/Accept: application/json [, Authorization: Bearer <key>]
//	Body:     {"input": "<text>"}
//	Response: {"jailbreak": <bool | number | "true"/"jailbreak">}
//
// The detector is optional and env-gated by the caller; on any transport/decode
// error it returns an error and the caller fails open (allows the request).
package nemoguard

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// Result is a single classification outcome.
type Result struct {
	Jailbreak bool    `json:"jailbreak"`
	Score     float64 `json:"score"` // 0..1 when the NIM returns a probability; else 0/1
}

// Stats is an atomic snapshot of detector activity (live/uptime metrics).
type Stats struct {
	Enabled    bool   `json:"enabled"`
	Mode       string `json:"mode"`
	Checks     int64  `json:"checks"`
	Jailbreaks int64  `json:"jailbreaks"`
	Blocked    int64  `json:"blocked"`
	Errors     int64  `json:"errors"`
}

// Detector calls the NeMo Guard NIM.
type Detector struct {
	baseURL      string
	classifyPath string
	apiKey       string
	mode         string // "block" | "flag"
	threshold    float64
	httpClient   *http.Client

	checks     atomic.Int64
	jailbreaks atomic.Int64
	blocked    atomic.Int64
	errors     atomic.Int64
}

// New builds a Detector. classifyPath defaults to "/v1/classify", mode to "block".
func New(baseURL, classifyPath, apiKey, mode string, threshold float64, timeout time.Duration) *Detector {
	if classifyPath == "" {
		classifyPath = "/v1/classify"
	}
	if mode != "flag" {
		mode = "block"
	}
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return &Detector{
		baseURL:      baseURL,
		classifyPath: classifyPath,
		apiKey:       apiKey,
		mode:         mode,
		threshold:    threshold,
		httpClient:   &http.Client{Timeout: timeout},
	}
}

// Mode returns "block" or "flag".
func (d *Detector) Mode() string { return d.mode }

// MarkBlocked records that a detected jailbreak resulted in a blocked request.
func (d *Detector) MarkBlocked() { d.blocked.Add(1) }

// Classify sends text to the NIM and returns the jailbreak verdict.
func (d *Detector) Classify(ctx context.Context, text string) (Result, error) {
	d.checks.Add(1)

	reqBody, err := json.Marshal(map[string]string{"input": text})
	if err != nil {
		d.errors.Add(1)
		return Result{}, err
	}

	url := strings.TrimRight(d.baseURL, "/") + d.classifyPath
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(reqBody))
	if err != nil {
		d.errors.Add(1)
		return Result{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if d.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+d.apiKey)
	}

	resp, err := d.httpClient.Do(req)
	if err != nil {
		d.errors.Add(1)
		return Result{}, fmt.Errorf("nemoguard request: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		d.errors.Add(1)
		return Result{}, fmt.Errorf("nemoguard returned %d: %s", resp.StatusCode, truncate(string(respBody), 200))
	}

	var parsed struct {
		Jailbreak any `json:"jailbreak"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		d.errors.Add(1)
		return Result{}, fmt.Errorf("nemoguard decode: %w", err)
	}

	jb, score := interpret(parsed.Jailbreak, d.threshold)
	if jb {
		d.jailbreaks.Add(1)
	}
	return Result{Jailbreak: jb, Score: score}, nil
}

// Stats returns an atomic snapshot.
func (d *Detector) Stats() Stats {
	return Stats{
		Enabled:    true,
		Mode:       d.mode,
		Checks:     d.checks.Load(),
		Jailbreaks: d.jailbreaks.Load(),
		Blocked:    d.blocked.Load(),
		Errors:     d.errors.Load(),
	}
}

// interpret turns the NIM's flexible "jailbreak" field into a (verdict, score).
// Accepts bool, a probability number (>= threshold = jailbreak), or a string
// label ("true"/"jailbreak"/"yes"/"1", or a numeric string).
func interpret(v any, threshold float64) (bool, float64) {
	switch t := v.(type) {
	case bool:
		if t {
			return true, 1.0
		}
		return false, 0.0
	case float64:
		return t >= threshold, t
	case json.Number:
		f, _ := t.Float64()
		return f >= threshold, f
	case string:
		s := strings.ToLower(strings.TrimSpace(t))
		switch s {
		case "true", "jailbreak", "yes", "1", "unsafe":
			return true, 1.0
		case "false", "safe", "no", "0":
			return false, 0.0
		}
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			return f >= threshold, f
		}
		return false, 0.0
	default:
		return false, 0.0
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
