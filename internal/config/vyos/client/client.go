// Package client talks to the REST API of a VyOS router.
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
	"strings"
	"time"

	"github.com/hauke-cloud/router-api/internal/config/vyos/command"
)

// ErrNothingToConfirm is returned by Confirm when the router has no pending
// commit-confirm: the change the caller meant to keep has already been
// reverted, or was never made.
var ErrNothingToConfirm = errors.New("no commit-confirm is pending on the router")

// maxResponseBytes bounds what is read from a router. A full configuration
// with certificates in it is tens of kilobytes.
const maxResponseBytes = 16 << 20

// APIError is an answer from the router that says the request failed.
type APIError struct {
	// StatusCode of the HTTP response.
	StatusCode int
	// Message the router gave. It can quote configuration, so treat it as
	// sensitive as the configuration itself.
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("router answered HTTP %d: %s", e.StatusCode, e.Message)
}

// Options configure a Client.
type Options struct {
	// URL of the API, https://address[:port].
	URL string
	// Key is the API key.
	Key string
	// CACertPEM is the certificate the router has to present, or the CA that
	// signed it. Nothing else is trusted: the system's certificate store is
	// not consulted.
	CACertPEM []byte
	// ServerName the certificate was issued for. Routers are addressed by IP
	// and that IP is not known when the certificate is made, so the name
	// checked is this one rather than the address dialled.
	ServerName string
	// Timeout of a single request. Defaults to two minutes: a commit of a
	// large change takes a while.
	Timeout time.Duration
}

// Client is a client for one router.
type Client struct {
	baseURL string
	key     string
	http    *http.Client
}

// Info is what a router says about itself without being asked for a key.
type Info struct {
	Version  string `json:"version"`
	Hostname string `json:"hostname"`
}

// New returns a client for the router described by opts.
func New(opts *Options) (*Client, error) {
	parsed, err := url.Parse(opts.URL)
	if err != nil {
		return nil, fmt.Errorf("parse URL: %w", err)
	}
	if parsed.Scheme != "https" || parsed.Host == "" {
		return nil, fmt.Errorf("URL %q has to be https://host[:port]", opts.URL)
	}
	if opts.Key == "" {
		return nil, errors.New("an API key is required")
	}
	if opts.ServerName == "" {
		return nil, errors.New("a server name is required")
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(opts.CACertPEM) {
		return nil, errors.New("no certificate found in the CA PEM")
	}
	timeout := opts.Timeout
	if timeout == 0 {
		timeout = 2 * time.Minute
	}

	return &Client{
		baseURL: strings.TrimRight(opts.URL, "/"),
		key:     opts.Key,
		http: &http.Client{
			Timeout: timeout,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{
					RootCAs:    pool,
					ServerName: opts.ServerName,
					MinVersion: tls.VersionTLS12,
				},
				// One router, few requests, far apart.
				MaxIdleConns:        2,
				IdleConnTimeout:     30 * time.Second,
				TLSHandshakeTimeout: 10 * time.Second,
			},
		},
	}, nil
}

// Close releases the client's idle connections.
func (c *Client) Close() {
	c.http.CloseIdleConnections()
}

// Info asks the router for its version. It needs no key, which makes it the
// cheapest proof that the router is up and is the router we think it is.
func (c *Client) Info(ctx context.Context) (Info, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/info", http.NoBody)
	if err != nil {
		return Info{}, err
	}
	var info Info
	if err := c.do(req, &info); err != nil {
		return Info{}, err
	}
	return info, nil
}

// Commands returns the running configuration.
//
// The result contains every secret on the router. Do not log it.
func (c *Client) Commands(ctx context.Context) ([]command.Path, error) {
	out, err := c.Show(ctx, "configuration", "commands")
	if err != nil {
		return nil, err
	}
	paths, err := command.Parse(out)
	if err != nil {
		return nil, fmt.Errorf("parse the router's configuration: %w", err)
	}
	return paths, nil
}

// Show runs an operational "show" command and returns its output.
func (c *Client) Show(ctx context.Context, path ...string) (string, error) {
	var out string
	if err := c.post(ctx, "/show", map[string]any{"op": "show", "path": path}, &out); err != nil {
		return "", err
	}
	return stripNoise(out), nil
}

// Configure applies changes as one commit. With confirmMinutes > 0 the router
// reverts the commit by itself unless Confirm is called within that time.
//
// A failed commit is not a no-op: components that applied before the failing
// one stay applied. Read the configuration back before deciding what to do.
func (c *Client) Configure(ctx context.Context, changes []command.Change, confirmMinutes int) error {
	if len(changes) == 0 {
		return nil
	}
	type operation struct {
		Op   command.Op `json:"op"`
		Path []string   `json:"path"`
	}
	commands := make([]operation, len(changes))
	for i, change := range changes {
		commands[i] = operation{Op: change.Op, Path: change.Path}
	}
	body := map[string]any{"commands": commands}
	if confirmMinutes > 0 {
		body["confirm_time"] = confirmMinutes
	}
	return c.post(ctx, "/configure", body, nil)
}

// Confirm keeps the change made by the last Configure. It returns
// ErrNothingToConfirm if the router is not waiting for a confirmation.
func (c *Client) Confirm(ctx context.Context) error {
	var out string
	if err := c.post(ctx, "/config-file", map[string]any{"op": "confirm"}, &out); err != nil {
		return err
	}
	if strings.Contains(out, "No confirm pending") {
		return ErrNothingToConfirm
	}
	return nil
}

// Save writes the running configuration to disk. Without it a reboot brings
// back the configuration from before.
func (c *Client) Save(ctx context.Context) error {
	return c.post(ctx, "/config-file", map[string]any{"op": "save"}, nil)
}

func (c *Client) post(ctx context.Context, endpoint string, body map[string]any, out any) error {
	body["key"] = c.key
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+endpoint, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	return c.do(req, out)
}

func (c *Client) do(req *http.Request, out any) error {
	resp, err := c.http.Do(req)
	if err != nil {
		// url.Error carries the URL but never the body, so the key cannot
		// end up in a log through this error.
		return fmt.Errorf("%s %s: %w", req.Method, req.URL.Path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return fmt.Errorf("%s %s: read response: %w", req.Method, req.URL.Path, err)
	}

	var envelope struct {
		Success bool            `json:"success"`
		Data    json.RawMessage `json:"data"`
		Error   *string         `json:"error"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return &APIError{StatusCode: resp.StatusCode, Message: "response is not JSON"}
	}
	if resp.StatusCode != http.StatusOK || !envelope.Success {
		message := "request failed"
		if envelope.Error != nil {
			message = strings.TrimSpace(stripNoise(*envelope.Error))
		}
		return &APIError{StatusCode: resp.StatusCode, Message: message}
	}
	if out == nil || len(envelope.Data) == 0 {
		return nil
	}
	if err := json.Unmarshal(envelope.Data, out); err != nil {
		return fmt.Errorf("%s %s: unexpected data in response: %w", req.Method, req.URL.Path, err)
	}
	return nil
}

// stripNoise removes the "sudo: unable to resolve host" lines a router puts in
// front of command output after its host name changed.
func stripNoise(out string) string {
	if !strings.Contains(out, "sudo: unable to resolve host") {
		return out
	}
	lines := strings.Split(out, "\n")
	kept := lines[:0]
	for _, line := range lines {
		if !strings.HasPrefix(line, "sudo: unable to resolve host") {
			kept = append(kept, line)
		}
	}
	return strings.Join(kept, "\n")
}
