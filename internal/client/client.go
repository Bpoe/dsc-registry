// Package client implements the remote DSC Registry administration transport.
package client

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Bpoe/dsc-registry/internal/desired"
)

const maxResponseBytes = 32 << 20

var canonicalUUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
var errorCode = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// Error exposes machine-readable HTTP failure details without server messages.
type Error struct {
	Status int
	Code   string
}

func (e *Error) Error() string {
	return fmt.Sprintf("registry request failed (HTTP %d)", e.Status)
}

type Client struct {
	base *url.URL
	key  string
	http *http.Client
}

func New(baseURL, key string, timeout time.Duration, tlsCA string, insecure bool) (*Client, error) {
	base, err := url.Parse(baseURL)
	if err != nil || base == nil || (base.Scheme != "http" && base.Scheme != "https") ||
		base.Hostname() == "" || base.User != nil || base.RawQuery != "" || base.ForceQuery ||
		base.Fragment != "" || base.Opaque != "" {
		return nil, errors.New("invalid registry server URL")
	}
	if timeout <= 0 {
		return nil, errors.New("request timeout must be positive")
	}
	if strings.ContainsAny(key, "\r\n") {
		return nil, errors.New("invalid API key")
	}
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: insecure} // explicitly requested by the operator
	if tlsCA != "" {
		pem, err := os.ReadFile(tlsCA)
		if err != nil {
			return nil, errors.New("cannot read TLS CA file")
		}
		pool, err := x509.SystemCertPool()
		if err != nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, errors.New("TLS CA file contains no certificates")
		}
		tlsConfig.RootCAs = pool
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = tlsConfig
	return &Client{base: base, key: key, http: &http.Client{
		Timeout: timeout, Transport: transport,
		// Reject even same-origin redirects: 301/302/303 may rewrite mutations,
		// and a redirect must never move a credential to another destination.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}, nil
}

// Request accepts a route whose individual variable segments are already escaped.
// A server URL's path is a reverse-proxy prefix, not a route to resolve against.
func (c *Client) Request(ctx context.Context, method, path string, query url.Values, body any) (json.RawMessage, int, error) {
	route, err := url.Parse(path)
	if err != nil || route.IsAbs() || route.Host != "" || route.RawQuery != "" ||
		route.ForceQuery || route.Fragment != "" || !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") {
		return nil, 0, errors.New("invalid registry request path")
	}
	target := *c.base
	escaped := strings.TrimRight(c.base.EscapedPath(), "/") + route.EscapedPath()
	target.Path, err = url.PathUnescape(escaped)
	if err != nil {
		return nil, 0, errors.New("invalid registry request path")
	}
	target.RawPath = escaped
	target.RawQuery = query.Encode()
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil || desired.ValidateJSON(data) != nil {
			return nil, 0, errors.New("invalid request JSON")
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, target.String(), reader)
	if err != nil {
		return nil, 0, errors.New("cannot create registry request")
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.key != "" {
		req.Header.Set("Authorization", "ApiKey "+c.key)
	}
	response, err := c.http.Do(req)
	if err != nil {
		// net/http's *url.Error includes the full URL. Never return it, even
		// wrapped, because names, queries and proxy URLs can contain secrets.
		return nil, 0, transportError(err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			return nil, response.StatusCode, transportError(err)
		}
		return nil, response.StatusCode, errors.New("cannot read registry response")
	}
	if len(data) > maxResponseBytes {
		return nil, response.StatusCode, errors.New("registry response exceeds 32 MiB limit")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		failure := &Error{Status: response.StatusCode}
		var envelope struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		if desired.ValidateJSON(data) == nil && json.Unmarshal(data, &envelope) == nil && errorCode.MatchString(envelope.Error.Code) {
			failure.Code = envelope.Error.Code
		}
		return nil, response.StatusCode, failure
	}
	if method == http.MethodHead || response.StatusCode == http.StatusNoContent {
		return nil, response.StatusCode, nil
	}
	if path == "/health/ready" || path == "/health/live" {
		return json.RawMessage(data), response.StatusCode, nil
	}
	if _, err := object(data); err != nil {
		return nil, response.StatusCode, err
	}
	// Keep the exact original report bytes, not a round trip through a map.
	return json.RawMessage(data), response.StatusCode, nil
}

func transportError(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("registry request timed out: %w", context.DeadlineExceeded)
	}
	if errors.Is(err, context.Canceled) {
		return fmt.Errorf("registry request canceled: %w", context.Canceled)
	}
	return errors.New("registry connection failed")
}

func object(data []byte) (map[string]any, error) {
	if desired.ValidateJSON(data) != nil {
		return nil, errors.New("invalid registry JSON response")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value map[string]any
	if err := decoder.Decode(&value); err != nil || value == nil {
		return nil, errors.New("registry response must be a JSON object")
	}
	return value, nil
}

func (c *Client) ResolveMachine(ctx context.Context, name string) (map[string]any, error) {
	path := "/v1/machines"
	query := url.Values{}
	if canonicalUUID.MatchString(name) {
		path += "/" + url.PathEscape(name)
	} else {
		query.Set("name", name)
	}
	data, _, err := c.Request(ctx, http.MethodGet, path, query, nil)
	if err != nil {
		return nil, err
	}
	return object(data)
}

func (c *Client) List(ctx context.Context, path, collection string, limit int, cursor string, all bool) (map[string]any, error) {
	if limit < 1 || limit > 1000 {
		return nil, errors.New("page limit must be between 1 and 1000")
	}
	var result map[string]any
	items := make([]any, 0)
	seen := map[string]bool{}
	if cursor != "" {
		seen[cursor] = true
	}
	for {
		query := url.Values{"limit": {strconv.Itoa(limit)}}
		if cursor != "" {
			query.Set("cursor", cursor)
		}
		data, _, err := c.Request(ctx, http.MethodGet, path, query, nil)
		if err != nil {
			return nil, err
		}
		page, err := object(data)
		if err != nil {
			return nil, err
		}
		values, ok := page[collection].([]any)
		if !ok {
			return nil, errors.New("invalid registry collection response")
		}
		next := ""
		if value, exists := page["nextCursor"]; exists {
			next, ok = value.(string)
			if !ok {
				return nil, errors.New("invalid registry pagination cursor")
			}
		}
		if !all {
			return page, nil
		}
		if result == nil {
			result = page
		} else {
			for key, value := range page {
				if key != collection && key != "nextCursor" {
					result[key] = value
				}
			}
		}
		items = append(items, values...)
		if next == "" {
			result[collection] = items
			delete(result, "nextCursor")
			return result, nil
		}
		if seen[next] {
			return nil, errors.New("registry pagination cursor repeated")
		}
		seen[next] = true
		cursor = next
	}
}
