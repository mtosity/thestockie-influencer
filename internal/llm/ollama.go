// Package llm wraps Ollama Cloud for structured extraction and synthesis.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"strings"
	"time"
)

// Ollama is a minimal client for the Ollama Cloud chat API
// (https://ollama.com/api/chat), authenticated with a bearer API key.
type Ollama struct {
	Host   string // e.g. https://ollama.com
	APIKey string
	Model  string
	HTTP   *http.Client
}

type chatMsg struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatReq struct {
	Model    string          `json:"model"`
	Messages []chatMsg       `json:"messages"`
	Stream   bool            `json:"stream"`
	Format   json.RawMessage `json:"format,omitempty"`
	Options  map[string]any  `json:"options,omitempty"`
}

type chatResp struct {
	Message struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"message"`
	Error string `json:"error,omitempty"`
}

const (
	maxAttempts           = 6
	baseBackoff           = 2 * time.Second
	maxBackoff            = 60 * time.Second
	sessionLimitBackoff   = 5 * time.Minute
	sessionLimitMarker    = "session usage limit"
)

// Chat sends a system+user turn and returns the assistant content. When format
// is non-nil it is sent as the structured-output JSON schema.
func (o *Ollama) Chat(ctx context.Context, system, user string, format json.RawMessage, temperature float64) (string, error) {
	reqBody := chatReq{
		Model: o.Model,
		Messages: []chatMsg{
			{Role: "system", Content: system},
			{Role: "user", Content: user},
		},
		Stream:  false,
		Format:  format,
		Options: map[string]any{
			"temperature": temperature,
			// Ollama defaults to a 4096-token context window, which is too
			// small for long transcripts + structured JSON output. The model
			// runs out of tokens mid-response and returns truncated JSON.
			// num_ctx = total context window (prompt + response).
			// num_predict = max tokens to generate.
			// 32768 ctx leaves ample room for a 12K-char transcript (~3K
			// tokens) plus a full extraction response (~2K tokens).
			"num_ctx":     32768,
			"num_predict": 8192,
		},
	}
	buf, err := json.Marshal(reqBody)
	if err != nil {
		return "", err
	}

	// Ollama Cloud occasionally times out / resets the connection; retry
	// transient failures with exponential backoff and jitter. If we hit the
	// account-level session usage limit, pause for several minutes before
	// retrying since that limit does not clear quickly.
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if attempt > 1 {
			delay := o.backoff(attempt, lastErr)
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(delay):
			}
		}
		content, retryable, err := o.chatOnce(ctx, buf)
		if err == nil {
			return content, nil
		}
		lastErr = err
		if !retryable {
			return "", err
		}
	}
	return "", fmt.Errorf("ollama: %d attempts failed: %w", maxAttempts, lastErr)
}

// backoff returns an exponential-backoff-with-jitter delay. If the error looks
// like the Ollama Cloud account session limit, use a long fixed cooldown.
func (o *Ollama) backoff(attempt int, lastErr error) time.Duration {
	if lastErr != nil && strings.Contains(strings.ToLower(lastErr.Error()), sessionLimitMarker) {
		return sessionLimitBackoff
	}
	// 2^(attempt-2) * base, capped at maxBackoff, with full jitter.
	factor := 1 << max(0, attempt-2)
	d := time.Duration(factor) * baseBackoff
	if d > maxBackoff {
		d = maxBackoff
	}
	jitter := time.Duration(rand.Int63n(int64(d)))
	return d + jitter
}

// chatOnce performs a single request. The bool reports whether the error is
// worth retrying (network failure, 429, or 5xx).
func (o *Ollama) chatOnce(ctx context.Context, buf []byte) (string, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.Host+"/api/chat", bytes.NewReader(buf))
	if err != nil {
		return "", false, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+o.APIKey)

	resp, err := o.HTTP.Do(req)
	if err != nil {
		return "", true, err // network error → retryable
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))

	if resp.StatusCode != http.StatusOK {
		bodyStr := string(body)
		retryable := resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500
		return "", retryable, fmt.Errorf("ollama %s: status %d: %s", o.Model, resp.StatusCode, snippet(bodyStr))
	}
	var cr chatResp
	if err := json.Unmarshal(body, &cr); err != nil {
		return "", false, fmt.Errorf("ollama decode: %w: %s", err, snippet(string(body)))
	}
	if cr.Error != "" {
		return "", false, fmt.Errorf("ollama error: %s", cr.Error)
	}
	// Flash/reasoning models sometimes return HTTP 200 with an empty content
	// field (the model emitted everything into its thinking channel). Treat
	// empty responses as retryable so the retry loop gets another shot.
	if strings.TrimSpace(cr.Message.Content) == "" {
		return "", true, fmt.Errorf("ollama %s: empty response (model may have emitted to thinking channel)", o.Model)
	}
	return cr.Message.Content, false, nil
}

func snippet(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 500 {
		return s[:500]
	}
	return s
}

// decodeJSON unmarshals a model response into v, tolerating leading/trailing
// prose and markdown code fences (```json ... ```) by stripping them before
// falling back to the outermost {...} span.
func decodeJSON(content string, v any) error {
	content = strings.TrimSpace(content)

	// Strip markdown code fences and broken fence attempts.
	// Models sometimes return: ```json\n{...}\n```
	// or broken variants like: {"json\n{...}\n"}
	// or: {```json\n{...}}
	// Strategy: strip ``` and ```json prefixes, then strip any leading
	// {"json or {"```json artifacts.
	fenceReplaced := true
	for fenceReplaced {
		fenceReplaced = false
		// Standard markdown fences
		if strings.HasPrefix(content, "```") {
			if nl := strings.IndexByte(content, '\n'); nl >= 0 {
				content = strings.TrimSpace(content[nl+1:])
				fenceReplaced = true
			}
			content = strings.TrimSuffix(content, "```")
			content = strings.TrimSpace(content)
			fenceReplaced = true
		}
		// Broken variant: {"json\n{... → strip the {"json prefix
		if strings.HasPrefix(content, "{\"json") {
			rest := content[6:] // skip {"json
			// Find the next { which starts the real JSON
			if idx := strings.IndexByte(rest, '{'); idx >= 0 {
				content = strings.TrimSpace(rest[idx:])
				fenceReplaced = true
			}
		}
		// Broken variant: {"```json\n{ → strip
		if strings.HasPrefix(content, "{\"```") {
			rest := content[5:] // skip {"```
			if idx := strings.IndexByte(rest, '{'); idx >= 0 {
				content = strings.TrimSpace(rest[idx:])
				fenceReplaced = true
			}
		}
	}

	// Some models drop the opening {" prefix and return something like:
	//   "summary":"...",\n  "mentions":[...]
	// or:
	//   summary":"...",\n  "mentions":[...]
	// Try to repair by prepending the missing brace.
	if !strings.HasPrefix(content, "{") {
		// Case A: starts with " (dropped {"key prefix)
		if strings.HasPrefix(content, "\"") {
			content = "{\"summary\":" + content
		} else if idx := strings.Index(content, "\":"); idx > 0 && idx < 30 {
			// Case B: starts with key: (dropped {" prefix entirely)
			// e.g. summary":"..." → {"summary":"..."
			key := content[:idx]
			content = "{\"" + key + content[idx:]
		}
	}

	// Try direct unmarshal first.
	if err := json.Unmarshal([]byte(content), v); err == nil {
		return nil
	}
	// Fallback: extract the outermost {...} span. This handles cases where
	// the model wraps JSON in prose or has trailing/leading text.
	start := strings.IndexByte(content, '{')
	end := strings.LastIndexByte(content, '}')
	if start >= 0 && end > start {
		if err := json.Unmarshal([]byte(content[start:end+1]), v); err == nil {
			return nil
		}
		// The {...} span found but malformed. Try progressively trimming
		// trailing characters that might be fence remnants or prose.
		sub := content[start : end+1]
		// Sometimes the model emits multiple JSON objects or appends prose
		// after the closing brace. Try finding each top-level object.
		for len(sub) > 0 {
			if err := json.Unmarshal([]byte(sub), v); err == nil {
				return nil
			}
			// Trim last char and try again (handles trailing commas, prose, etc.)
			sub = strings.TrimSpace(sub[:len(sub)-1])
			if i := strings.LastIndexByte(sub, '}'); i >= 0 {
				sub = sub[:i+1]
			} else {
				break
			}
		}
	}

	// Last resort: the model ran out of tokens mid-JSON (truncated response).
	// Try to salvage partial data by closing open structures and re-parsing.
	if recovered := recoverTruncatedJSON(content, v); recovered {
		return nil
	}

	return fmt.Errorf("no parseable JSON object in model response: %s", snippet(content))
}

// recoverTruncatedJSON attempts to salvage usable data from a truncated JSON
// response. When the model hits the token limit mid-string, the JSON is
// incomplete (missing closing braces/brackets/quotes). This function:
//  1. Finds the first { and takes everything after it
//  2. Closes any unterminated string with a "
//  3. Truncates at the last complete key-value pair or array element
//  4. Closes open brackets/braces
//  5. Attempts to unmarshal the repaired JSON
//
// This is a best-effort recovery — partial mentions/summary are better than
// losing the entire extraction.
func recoverTruncatedJSON(content string, v any) bool {
	start := strings.IndexByte(content, '{')
	if start < 0 {
		return false
	}
	sub := content[start:]

	// Count open/close braces and brackets to determine nesting depth.
	// Track whether we're inside a string to avoid counting braces in strings.
	inString := false
	escape := false
	depth := 0
	lastCompleteIdx := -1 // index of the last } or ] at depth 1 (top-level fields)

	for i, ch := range sub {
		if escape {
			escape = false
			continue
		}
		if ch == '\\' {
			escape = true
			continue
		}
		if ch == '"' {
			inString = !inString
			continue
		}
		if inString {
			continue
		}
		if ch == '{' || ch == '[' {
			depth++
		}
		if ch == '}' || ch == ']' {
			depth--
			// At depth 0 after closing, we have a complete top-level object.
			if depth == 0 {
				lastCompleteIdx = i
			}
		}
	}

	// If depth is 0, the JSON is actually complete — shouldn't happen here
	// but handle it anyway.
	if depth <= 0 && lastCompleteIdx >= 0 {
		repaired := sub[:lastCompleteIdx+1]
		if err := json.Unmarshal([]byte(repaired), v); err == nil {
			return true
		}
	}

	// Truncated: we need to close the open structures.
	// Strategy: find the last complete field value or array element, truncate
	// there, then close all open braces/brackets.

	// Look for the last comma at the shallowest nesting depth that we can
	// safely truncate at. This is a heuristic: find the last , or : that
	// precedes a complete value.
	bestTruncIdx := -1
	depth = 0
	inString = false
	escape = false
	for i, ch := range sub {
		if escape {
			escape = false
			continue
		}
		if ch == '\\' {
			escape = true
			continue
		}
		if ch == '"' {
			inString = !inString
			continue
		}
		if inString {
			continue
		}
		if ch == '{' || ch == '[' {
			depth++
		}
		if ch == '}' || ch == ']' {
			depth--
		}
		// A comma at depth 1 means we just completed a top-level field.
		// Truncating here and closing braces gives us all fields up to this point.
		if ch == ',' && depth == 1 {
			bestTruncIdx = i
		}
	}

	if bestTruncIdx >= 0 {
		// Truncate at the last complete top-level field, remove trailing comma.
		repaired := sub[:bestTruncIdx]
		repaired = strings.TrimRight(repaired, " 	\n\r,")
		// Close any remaining open structures.
		repaired += closeOpenStructures(repaired)
		if err := json.Unmarshal([]byte(repaired), v); err == nil {
			return true
		}
	}

	// Even more aggressive: try closing at the current position with just
	// string + brace closure.
	repaired := sub
	// Close unterminated string.
	if inString {
		repaired += `"`
	}
	// Remove trailing incomplete fragments (partial key/value after last comma).
	if idx := strings.LastIndex(repaired, ","); idx >= 0 {
		repaired = strings.TrimRight(repaired[:idx], " 	\n\r,")
	}
	repaired += closeOpenStructures(repaired)
	if err := json.Unmarshal([]byte(repaired), v); err == nil {
		return true
	}

	return false
}

// closeOpenStructures scans the (possibly truncated) JSON string and appends
// the necessary closing ] and } characters for any unclosed arrays/objects.
func closeOpenStructures(s string) string {
	inString := false
	escape := false
	var stack []byte // stack of expected closers

	for _, ch := range s {
		if escape {
			escape = false
			continue
		}
		if ch == '\\' {
			escape = true
			continue
		}
		if ch == '"' {
			inString = !inString
			continue
		}
		if inString {
			continue
		}
		switch ch {
		case '{':
			stack = append(stack, '}')
		case '[':
			stack = append(stack, ']')
		case '}', ']':
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
		}
	}

	// Close in reverse order (innermost first).
	var closers strings.Builder
	for i := len(stack) - 1; i >= 0; i-- {
		closers.WriteByte(stack[i])
	}
	return closers.String()
}
