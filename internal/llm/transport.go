package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const (
	chatPath       = "/chat/completions"
	modelsPath     = "/models"
	responseLimit  = 1 << 20
	complaintLimit = 4 << 10
)

// NewHTTPClient creates a connection pool shared by a session's model clients.
func NewHTTPClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ForceAttemptHTTP2 = true
	transport.MaxIdleConns = 2 // Keep both backend servers warm when they differ.
	transport.MaxIdleConnsPerHost = 1
	transport.IdleConnTimeout = 0
	return &http.Client{Transport: transport}
}

func modelsURL(endpoint string) string {
	endpoint = strings.TrimRight(endpoint, "/")
	for _, path := range []string{chatPath, "/responses"} {
		if base, ok := strings.CutSuffix(endpoint, path); ok {
			return base + modelsPath
		}
	}
	return endpoint + modelsPath
}

func (c *Client) post(ctx context.Context, ask chatRequest) (io.ReadCloser, error) {
	payload, err := c.protocol.encode(ask)
	if err != nil {
		return nil, fmt.Errorf("llm: preparing the question: %w", err)
	}
	req, err := c.request(ctx, http.MethodPost, c.endpoint, payload)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("llm: asking %s at %s: %w", c.model, c.endpoint, err)
	}
	if resp.StatusCode != http.StatusOK {
		defer closeHTTP(resp.Body, true)
		return nil, fmt.Errorf("llm: %s answered %s: %s", c.endpoint, resp.Status, complaint(resp.Body))
	}
	return resp.Body, nil
}

func (c *Client) get(ctx context.Context) ([]byte, error) {
	req, err := c.request(ctx, http.MethodGet, modelsURL(c.endpoint), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("llm: reaching %s: %w", c.endpoint, err)
	}
	defer closeHTTP(resp.Body, true)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("llm: %s answered %s: %s", c.endpoint, resp.Status, complaint(resp.Body))
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, responseLimit))
	if err != nil {
		return nil, fmt.Errorf("llm: reading %s: %w", c.endpoint, err)
	}
	return body, nil
}

func (c *Client) request(ctx context.Context, method, rawURL string, body []byte) (*http.Request, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, reader)
	if err != nil {
		return nil, fmt.Errorf("llm: addressing %s: %w", rawURL, err)
	}
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

func closeHTTP(body io.Closer, reusable bool) {
	if reusable {
		if reader, ok := body.(io.Reader); ok {
			_, _ = io.Copy(io.Discard, io.LimitReader(reader, responseLimit))
		}
	}
	_ = body.Close()
}

func complaint(body io.Reader) string {
	data, err := io.ReadAll(io.LimitReader(body, complaintLimit))
	if err != nil || len(data) == 0 {
		return "no reason given"
	}
	var failure chatFailure
	if json.Unmarshal(data, &failure) == nil && failure.Error.Message != "" {
		return failure.Error.Message
	}
	return strings.TrimSpace(string(data))
}
