package clickup

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

type Client struct {
	httpClient *http.Client
	apiKey     string
	baseURL    string
}

func New(apiKey string) *Client {
	return &Client{
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
		apiKey:  apiKey,
		baseURL: "https://api.clickup.com/api/v2",
	}
}

// WithBaseURL allows overriding the base URL (useful for tests)
func (c *Client) WithBaseURL(url string) *Client {
	c.baseURL = url
	return c
}

func (c *Client) do(ctx context.Context, method, path string, bodyVal interface{}, result interface{}) error {
	var body io.Reader
	if bodyVal != nil {
		b, err := json.Marshal(bodyVal)
		if err != nil {
			return fmt.Errorf("failed to marshal request body: %w", err)
		}
		body = bytes.NewReader(b)
	}

	url := c.baseURL + path
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return fmt.Errorf("failed to create http request: %w", err)
	}

	req.Header.Set("Authorization", c.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("http request execution failed: %w", err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK, http.StatusCreated, http.StatusAccepted, http.StatusNoContent:
		if result != nil {
			dec := json.NewDecoder(resp.Body)
			if err := dec.Decode(result); err != nil {
				return fmt.Errorf("failed to decode response: %w", err)
			}
		}
		return nil
	case http.StatusUnauthorized, http.StatusForbidden:
		return ErrUnauthorized
	case http.StatusNotFound:
		return ErrNotFound
	case http.StatusTooManyRequests:
		return ErrRateLimited
	default:
		if resp.StatusCode >= 500 {
			return ErrServerError
		}
		// Read error message from body if available
		respBytes, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("unexpected status code %d: %s", resp.StatusCode, string(respBytes))
	}
}
