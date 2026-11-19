// Package hubclient implements the experimental scoped workload HTTP client.
package hubclient

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Werner2005/herdr-hub/internal/workload"
)

const maxResponse = 4 << 20

type Client struct {
	base  string
	token string
	http  *http.Client
}

// New requires HTTPS and a distinct workload credential, never a Hub admin or
// workspace web token. Redirects are refused, including same-origin redirects:
// an API call must not accidentally reach a login page or another endpoint.
func New(base, token string, transport http.RoundTripper) (*Client, error) {
	u, err := url.Parse(base)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, fmt.Errorf("HERDR_HUB_URL must be an HTTPS origin without credentials, path, query or fragment")
	}
	if !strings.HasPrefix(token, "hhw_") || len(token) != 68 {
		return nil, fmt.Errorf("a scoped hhw_ workload token is required; administrator and web tokens are not accepted")
	}
	if _, err = hex.DecodeString(token[4:]); err != nil {
		return nil, fmt.Errorf("invalid workload token")
	}
	return &Client{
		base: strings.TrimSuffix(base, "/"), token: token,
		http: &http.Client{Transport: transport, Timeout: 90 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}},
	}, nil
}

// Do calls only the workload API. Error bodies are intentionally not echoed:
// reverse proxies may include credentials or user-controlled text in them.
func (c *Client) Do(ctx context.Context, method, endpoint string, input, output any) error {
	if endpoint != "" {
		for _, component := range strings.Split(strings.TrimPrefix(endpoint, "/"), "/") {
			if !workload.ValidName(component) {
				return fmt.Errorf("invalid workload API path")
			}
		}
		if !strings.HasPrefix(endpoint, "/") {
			return fmt.Errorf("invalid workload API path")
		}
	}
	var body io.Reader
	if input != nil {
		b, err := json.Marshal(input)
		if err != nil {
			return err
		}
		if len(b) > 65536 {
			return fmt.Errorf("request exceeds 64 KiB")
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+workload.APIPrefix+endpoint, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	if input != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		// Do not return transport errors that might contain a credential supplied
		// by an injected transport or proxy.
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("Hub request failed; check TLS, connectivity and server availability")
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("workload API or resource unavailable (HTTP 404); check the Hub version and parent-scoped resource name")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("Hub workload API returned HTTP %d", resp.StatusCode)
	}
	if output == nil || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse+1))
	if err != nil {
		return fmt.Errorf("could not read Hub response")
	}
	if len(data) > maxResponse {
		return fmt.Errorf("Hub response exceeds 4 MiB")
	}
	if err = json.Unmarshal(data, output); err != nil {
		return fmt.Errorf("invalid JSON response from Hub workload API")
	}
	return nil
}
