// Package llm wraps Ollama Cloud for structured extraction and synthesis.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
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
		Options: map[string]any{"temperature": temperature},
	}
	buf, err := json.Marshal(reqBody)
	if err != nil {
		return "", err
	}

	// Ollama Cloud occasionally times out / resets the connection; retry
	// transient failures with linear backoff.
	const maxAttempts = 4
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if attempt > 1 {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(time.Duration(attempt-1) * 3 * time.Second):
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
		retryable := resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500
		return "", retryable, fmt.Errorf("ollama %s: status %d: %s", o.Model, resp.StatusCode, snippet(body))
	}
	var cr chatResp
	if err := json.Unmarshal(body, &cr); err != nil {
		return "", false, fmt.Errorf("ollama decode: %w: %s", err, snippet(body))
	}
	if cr.Error != "" {
		return "", false, fmt.Errorf("ollama error: %s", cr.Error)
	}
	return cr.Message.Content, false, nil
}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 500 {
		return s[:500]
	}
	return s
}

// decodeJSON unmarshals a model response into v, tolerating leading/trailing
// prose by falling back to the outermost {...} span.
func decodeJSON(content string, v any) error {
	content = strings.TrimSpace(content)
	if err := json.Unmarshal([]byte(content), v); err == nil {
		return nil
	}
	start := strings.IndexByte(content, '{')
	end := strings.LastIndexByte(content, '}')
	if start >= 0 && end > start {
		if err := json.Unmarshal([]byte(content[start:end+1]), v); err == nil {
			return nil
		}
	}
	return fmt.Errorf("no parseable JSON object in model response: %s", snippet([]byte(content)))
}
