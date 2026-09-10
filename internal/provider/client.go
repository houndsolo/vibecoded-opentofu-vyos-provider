package provider

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

type Client struct {
	endpoint string
	apiKey   string
	http     *http.Client
}

func normalizeEndpoint(endpoint string) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u == nil || u.Hostname() == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return "", fmt.Errorf("endpoint must be an absolute http or https URL")
	}
	if u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return "", fmt.Errorf("endpoint must not contain credentials, a query, or a fragment")
	}
	host := u.Hostname()
	// DNS is case-insensitive, but an IPv6 zone identifies a local interface
	// whose name can be case-sensitive.
	if zone := strings.IndexByte(host, '%'); zone >= 0 {
		host = strings.ToLower(host[:zone]) + host[zone:]
	} else {
		host = strings.ToLower(host)
	}
	port := u.Port()
	if (u.Scheme == "https" && port == "443") || (u.Scheme == "http" && port == "80") {
		port = ""
	}
	u.Host = host
	if port != "" {
		u.Host = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") {
		u.Host = "[" + host + "]"
	}
	// Trim actual URL separators, never escaped slashes in a proxy route.
	// Clearing RawPath can silently select a different router behind a proxy.
	u.RawPath = strings.TrimRight(u.EscapedPath(), "/")
	u.Path, _ = url.PathUnescape(u.RawPath)
	return u.String(), nil
}

func sameEndpoint(a, b string) bool {
	left, leftErr := normalizeEndpoint(a)
	right, rightErr := normalizeEndpoint(b)
	return leftErr == nil && rightErr == nil && left == right
}

func newClient(endpoint, apiKey string, insecure bool, timeout time.Duration) (*Client, error) {
	endpoint, err := normalizeEndpoint(endpoint)
	if err != nil {
		return nil, err
	}
	if apiKey == "" {
		return nil, fmt.Errorf("set api_key or VYOS_API_KEY")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: insecure} // explicitly configured for lab certificates
	return &Client{endpoint: endpoint, apiKey: apiKey, http: &http.Client{
		Transport: transport, Timeout: timeout,
		// Never forward the form API key to a redirect target.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}, nil
}

func (c *Client) close() { c.http.CloseIdleConnections() }

func (c *Client) request(ctx context.Context, route string, payload any) (json.RawMessage, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encode VyOS request: %w", err)
	}
	form := url.Values{"key": {c.apiKey}, "data": {string(data)}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint+route, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("cannot construct VyOS request")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.http.Do(req)
	if err != nil {
		// Transport errors can include URLs. Server error bodies can echo keys or
		// unrelated secrets. Neither is returned in a diagnostic or provider log.
		if ctx.Err() != nil {
			return nil, fmt.Errorf("VyOS %s request interrupted: %w", route, ctx.Err())
		}
		return nil, fmt.Errorf("VyOS %s request failed; check reachability, TLS and request_timeout (a commit outcome may be unknown)", route)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("VyOS %s returned HTTP %d; see router API logs", route, resp.StatusCode)
	}
	const maxResponse = 32 << 20
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse+1))
	if err != nil || len(body) > maxResponse {
		return nil, fmt.Errorf("cannot read complete VyOS %s response (limit 32 MiB)", route)
	}
	var envelope struct {
		Success *bool           `json:"success"`
		Data    json.RawMessage `json:"data"`
		Error   json.RawMessage `json:"error"`
	}
	if json.Unmarshal(body, &envelope) != nil || envelope.Success == nil {
		return nil, fmt.Errorf("invalid VyOS %s response", route)
	}
	if !*envelope.Success {
		return nil, fmt.Errorf("VyOS %s operation failed; see router API logs", route)
	}
	return envelope.Data, nil
}

func (c *Client) Snapshot(ctx context.Context) (*Snapshot, error) {
	raw, err := c.request(ctx, "/retrieve", struct {
		Op     string   `json:"op"`
		Path   []string `json:"path"`
		Format string   `json:"configFormat"`
	}{"showConfig", []string{}, "json_ast"})
	if err != nil {
		return nil, err
	}
	// AST errors must not include raw values from the configuration.
	s, err := DecodeSnapshot(raw)
	if err != nil {
		return nil, fmt.Errorf("cannot safely decode the full VyOS json_ast configuration; this API format is required")
	}
	active, err := c.request(ctx, "/show", Command{Op: "show", Path: []string{"configuration", "commands"}})
	if err != nil {
		return nil, err
	}
	var commands string
	if err := json.Unmarshal(active, &commands); err != nil {
		return nil, fmt.Errorf("cannot decode the active configuration export")
	}
	if err := s.VerifyActive(commands); err != nil {
		return nil, err
	}
	return s, nil
}

func (c *Client) Configure(ctx context.Context, operations []Command) error {
	if len(operations) == 0 {
		return nil
	}
	_, err := c.request(ctx, "/configure", operations)
	return err
}

func (c *Client) Save(ctx context.Context) error {
	_, err := c.request(ctx, "/config-file", map[string]string{"op": "save"})
	return err
}

// All resources and provider aliases in this process serialize a router's
// read/plan/commit/verify/save sequence. Separate processes need an external lock.
var routerLocks sync.Map

func lockRouter(ctx context.Context, endpoint string) (func(), error) {
	value, _ := routerLocks.LoadOrStore(endpoint, make(chan struct{}, 1))
	lock := value.(chan struct{})
	select {
	case lock <- struct{}{}:
		return func() { <-lock }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
