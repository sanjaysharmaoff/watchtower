// Package openai talks to any OpenAI-compatible chat and embedding API over
// plain net/http. No vendor SDK, so the dependency surface stays small and the
// same adapter works against Azure OpenAI, vLLM, Ollama or a gateway.
package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/sanjaysharmaoff/watchtower/internal/platform/retry"
	"github.com/sanjaysharmaoff/watchtower/internal/ports"
	"github.com/sanjaysharmaoff/watchtower/internal/wire"
)

// Client is a shared handle for chat and embedding calls.
type Client struct {
	http    *http.Client
	baseURL string
	apiKey  string
	timeout time.Duration
	log     ports.Logger
}

// Shared returns the process-wide client.
func Shared(e *wire.Env) (*Client, error) {
	return wire.Resource(e, "openai.client", func() (*Client, error) {
		if e.Cfg.LLM.APIKey == "" {
			return nil, fmt.Errorf("openai: API key is not configured")
		}
		return &Client{
			http:    e.HTTP,
			baseURL: strings.TrimRight(e.Cfg.LLM.BaseURL, "/"),
			apiKey:  e.Cfg.LLM.APIKey,
			timeout: e.Cfg.LLM.Timeout,
			log:     e.Log.With("component", "openai"),
		}, nil
	})
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatRequest struct {
	Model          string            `json:"model"`
	Messages       []chatMessage     `json:"messages"`
	Temperature    float64           `json:"temperature,omitempty"`
	MaxTokens      int               `json:"max_tokens,omitempty"`
	ResponseFormat map[string]string `json:"response_format,omitempty"`
}

type chatResponse struct {
	Choices []struct {
		Message      chatMessage `json:"message"`
		FinishReason string      `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
}

// ChatOptions configures a single completion.
type ChatOptions struct {
	Model       string
	System      string
	User        string
	Temperature float64
	MaxTokens   int
	JSONOnly    bool
}

// Chat runs a completion and returns the assistant message content.
//
// When JSON mode is requested and the server rejects it, the call is retried once
// without it. Not every OpenAI-compatible server implements response_format, and
// the plan parser already tolerates fenced or prefixed JSON, so refusing to
// degrade here would mean refusing to work with half the ecosystem.
func (c *Client) Chat(ctx context.Context, opts ChatOptions) (string, error) {
	out, err := c.chat(ctx, opts)
	if err != nil && opts.JSONOnly && mentionsResponseFormat(err) {
		c.log.Warn("server rejected JSON mode, retrying without it", "model", opts.Model)
		opts.JSONOnly = false
		return c.chat(ctx, opts)
	}
	return out, err
}

func mentionsResponseFormat(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "response_format") ||
		strings.Contains(msg, "json_object") ||
		strings.Contains(msg, "json mode")
}

func (c *Client) chat(ctx context.Context, opts ChatOptions) (string, error) {
	req := chatRequest{
		Model:       opts.Model,
		Temperature: opts.Temperature,
		MaxTokens:   opts.MaxTokens,
		Messages: []chatMessage{
			{Role: "system", Content: opts.System},
			{Role: "user", Content: opts.User},
		},
	}
	if opts.JSONOnly {
		req.ResponseFormat = map[string]string{"type": "json_object"}
	}

	var out chatResponse
	if err := c.post(ctx, "/chat/completions", req, &out); err != nil {
		return "", err
	}
	if len(out.Choices) == 0 {
		return "", fmt.Errorf("openai: empty completion")
	}
	c.log.Debug("completion done", "model", opts.Model, "tokens", out.Usage.TotalTokens, "finish", out.Choices[0].FinishReason)
	return out.Choices[0].Message.Content, nil
}

type embedRequest struct {
	Model string `json:"model"`
	Input string `json:"input"`
}

type embedResponse struct {
	Data []struct {
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
}

// Embeddings returns the vector for a single input.
func (c *Client) Embeddings(ctx context.Context, model, input string) ([]float32, error) {
	var out embedResponse
	if err := c.post(ctx, "/embeddings", embedRequest{Model: model, Input: input}, &out); err != nil {
		return nil, err
	}
	if len(out.Data) == 0 || len(out.Data[0].Embedding) == 0 {
		return nil, fmt.Errorf("openai: empty embedding response")
	}
	return out.Data[0].Embedding, nil
}

// post issues a JSON request with retries on transient failures.
func (c *Client) post(ctx context.Context, path string, payload, into any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if c.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.timeout)
		defer cancel()
	}

	return retry.Do(ctx, retry.Default(), func(ctx context.Context) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(body))
		if err != nil {
			return retry.Permanent(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+c.apiKey)

		resp, err := c.http.Do(req)
		if err != nil {
			return fmt.Errorf("openai %s: %w", path, err)
		}
		defer resp.Body.Close()

		raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			return fmt.Errorf("openai %s: transient status %d: %s", path, resp.StatusCode, snippet(raw))
		}
		if resp.StatusCode >= 400 {
			return retry.Permanent(fmt.Errorf("openai %s: status %d: %s", path, resp.StatusCode, snippet(raw)))
		}
		if readErr != nil {
			return fmt.Errorf("openai %s: read body: %w", path, readErr)
		}
		if err := json.Unmarshal(raw, into); err != nil {
			return retry.Permanent(fmt.Errorf("openai %s: decode response: %w", path, err))
		}
		return nil
	})
}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 300 {
		return s[:300] + "…"
	}
	return s
}
