package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"time"

	"orbitbeacon/internal/buildinfo"
)

const maxResponseBytes = 4 << 20

type apiClient struct {
	config config
	http   *http.Client
}
type textContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}
type toolResult struct {
	Content []textContent `json:"content"`
	IsError bool          `json:"isError"`
}

func result(text string, failed bool) toolResult {
	return toolResult{[]textContent{{"text", text}}, failed}
}
func newAPIClient(cfg config) *apiClient {
	return &apiClient{cfg, &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

var errorCodePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

func (c *apiClient) get(ctx context.Context, target string) toolResult {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.config.baseURL+target, nil)
	if err != nil {
		return result("HTTP request failed", true)
	}
	req.Header.Set("Authorization", "Bearer "+c.config.token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "orbitbeacon-mcp/"+buildinfo.Current().Version)
	resp, err := c.http.Do(req)
	if err != nil {
		return result("HTTP request failed", true)
	}
	defer resp.Body.Close()
	failure := func(code string) toolResult { return result(fmt.Sprintf("HTTP %d %s", resp.StatusCode, code), true) }
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if len(body) > maxResponseBytes {
		return failure("response_too_large")
	}
	if err != nil {
		return failure("response_read_failed")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var payload struct {
			Code string `json:"code"`
		}
		if json.Unmarshal(body, &payload) != nil || !errorCodePattern.MatchString(payload.Code) {
			payload.Code = "unknown_error"
		}
		return failure(payload.Code)
	}
	if !json.Valid(body) {
		return failure("invalid_json")
	}
	return result(string(body), false)
}
