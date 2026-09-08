// Copyright (c) 2026 ManyForge contributors. SPDX-License-Identifier: MIT
package manyforge

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// TokenProvider returns the current access token for one authenticated request.
type TokenProvider func(context.Context) (string, error)
type ClientOption func(*clientConfig) error
type clientConfig struct {
	accessToken               string
	provider                  TokenProvider
	session                   *Session
	modes                     int
	httpClient                *http.Client
	timeout                   time.Duration
	key, secret, sourceOrigin string
}

func WithAccessToken(token string) ClientOption {
	return func(c *clientConfig) error {
		c.accessToken = token
		c.modes++
		if strings.TrimSpace(token) == "" {
			return errors.New("access token must not be empty")
		}
		return nil
	}
}
func WithTokenProvider(provider TokenProvider) ClientOption {
	return func(c *clientConfig) error {
		c.provider = provider
		c.modes++
		if provider == nil {
			return errors.New("token provider must not be nil")
		}
		return nil
	}
}
func WithSession(session *Session) ClientOption {
	return func(c *clientConfig) error {
		c.session = session
		c.modes++
		if session == nil {
			return errors.New("session must not be nil")
		}
		return nil
	}
}

// WithHTTPClient shares the caller's pool without owning or modifying it. Custom
// RoundTrippers must not replay ambiguously processed requests. Public clients
// accept only *http.Transport, since arbitrary interceptors may add credentials.
func WithHTTPClient(client *http.Client) ClientOption {
	return func(c *clientConfig) error {
		if client == nil {
			return errors.New("HTTP client must not be nil")
		}
		c.httpClient = client
		return nil
	}
}
func WithClientTimeout(timeout time.Duration) ClientOption {
	return func(c *clientConfig) error {
		if timeout <= 0 {
			return errors.New("timeout must be positive")
		}
		c.timeout = timeout
		return nil
	}
}
func WithPublishableKey(key string) ClientOption {
	return func(c *clientConfig) error {
		if strings.TrimSpace(key) == "" {
			return errors.New("publishable key must not be empty")
		}
		c.key = key
		return nil
	}
}
func WithSigningSecret(secret string) ClientOption {
	return func(c *clientConfig) error {
		if strings.TrimSpace(secret) == "" {
			return errors.New("signing secret must not be empty")
		}
		c.secret = secret
		return nil
	}
}
func WithSourceOrigin(origin string) ClientOption {
	return func(c *clientConfig) error {
		value, err := instanceOrigin(origin)
		if err != nil {
			return errors.New("source origin must be an absolute HTTP(S) origin")
		}
		c.sourceOrigin = value
		return nil
	}
}

func instanceOrigin(value string) (string, error) {
	u, err := url.Parse(value)
	if err != nil || u == nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(value, "#") || (u.Path != "" && u.Path != "/") || u.RawPath != "" {
		return "", errors.New("base URL must be an absolute HTTP(S) instance root without credentials, path, query or fragment")
	}
	if strings.ContainsAny(u.Hostname(), "* ,\t\r\n") {
		return "", errors.New("origin host must be exact")
	}
	if strings.HasSuffix(u.Host, ":") {
		return "", errors.New("origin port must not be empty")
	}
	if port := u.Port(); port != "" {
		number, err := strconv.Atoi(port)
		if err != nil || number < 1 || number > 65535 {
			return "", errors.New("origin port is out of range")
		}
	}
	u.Host = strings.ToLower(u.Host)
	return strings.TrimSuffix(u.String(), "/"), nil
}

func newHTTPTransport(baseURL string, public, signed, analytics bool, options []ClientOption) (*httpTransport, error) {
	base, err := instanceOrigin(baseURL)
	if err != nil {
		return nil, err
	}
	config := clientConfig{timeout: 30 * time.Second}
	for _, option := range options {
		if option == nil {
			return nil, errors.New("nil client option")
		}
		if err := option(&config); err != nil {
			return nil, err
		}
	}
	if config.modes > 1 {
		return nil, errors.New("access token, token provider and session are mutually exclusive")
	}
	if public {
		if config.modes != 0 || config.key == "" {
			return nil, errors.New("public clients require a publishable key and reject management credentials")
		}
	} else if config.key != "" || config.secret != "" || config.sourceOrigin != "" {
		return nil, errors.New("management clients reject public credentials and source origin")
	}
	if signed && config.secret == "" {
		return nil, errors.New("signed clients require a signing secret")
	}
	if !signed && config.secret != "" {
		return nil, errors.New("unsigned clients reject signing secrets")
	}
	if analytics && config.sourceOrigin == "" {
		return nil, errors.New("server analytics requires source origin")
	}
	if !analytics && config.sourceOrigin != "" {
		return nil, errors.New("source origin is only supported by analytics")
	}
	var client http.Client
	var owned *http.Transport
	if config.httpClient != nil {
		client = *config.httpClient
		if client.Transport == nil {
			client.Transport = http.DefaultTransport
		}
		if public {
			if transport, ok := client.Transport.(*http.Transport); !ok || transport == nil {
				return nil, errors.New("public clients reject credential-capable custom RoundTrippers")
			}
		}
	} else {
		defaultTransport, ok := http.DefaultTransport.(*http.Transport)
		if !ok || defaultTransport == nil {
			return nil, errors.New("default HTTP transport must be a native *http.Transport; inject a client explicitly")
		}
		owned = defaultTransport.Clone()
		client.Transport = owned
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	client.Timeout = 0 // Request contexts cover both client defaults and per-call overrides.
	if public {
		client.Jar = nil
	}
	return &httpTransport{base: base, client: &client, owned: owned, config: config, public: public}, nil
}

type Client struct {
	*RootResources
	transport *httpTransport
}

func NewClient(baseURL string, options ...ClientOption) (*Client, error) {
	transport, err := newHTTPTransport(baseURL, false, false, false, options)
	if err != nil {
		return nil, err
	}
	return &Client{RootResources: NewRootResources(transport, ""), transport: transport}, nil
}
func (c *Client) Business(id string) *BusinessResources { return NewBusinessResources(c.transport, id) }

// Close closes only SDK-owned idle connections, never a caller-owned pool.
func (c *Client) Close() error { c.transport.close(); return nil }

type FeedbackClient struct {
	*FeedbackResources
	transport *httpTransport
}
type SignedFeedbackClient struct {
	*FeedbackResources
	transport *httpTransport
}
type TelemetryClient struct {
	*TelemetryResources
	transport *httpTransport
}
type SignedTelemetryClient struct {
	*TelemetryResources
	transport *httpTransport
}
type MailingClient struct {
	*MailingResources
	transport *httpTransport
}
type MailingServerClient struct {
	*MailingServerResources
	transport *httpTransport
}
type AnalyticsClient struct {
	*AnalyticsResources
	transport *httpTransport
}

func NewFeedbackClient(baseURL string, options ...ClientOption) (*FeedbackClient, error) {
	t, e := newHTTPTransport(baseURL, true, false, false, options)
	if e != nil {
		return nil, e
	}
	return &FeedbackClient{NewFeedbackResources(t, t.config.key), t}, nil
}
func NewSignedFeedbackClient(baseURL string, options ...ClientOption) (*SignedFeedbackClient, error) {
	t, e := newHTTPTransport(baseURL, true, true, false, options)
	if e != nil {
		return nil, e
	}
	return &SignedFeedbackClient{NewFeedbackResources(t, t.config.key), t}, nil
}
func NewTelemetryClient(baseURL string, options ...ClientOption) (*TelemetryClient, error) {
	t, e := newHTTPTransport(baseURL, true, false, false, options)
	if e != nil {
		return nil, e
	}
	return &TelemetryClient{NewTelemetryResources(t, t.config.key), t}, nil
}
func NewSignedTelemetryClient(baseURL string, options ...ClientOption) (*SignedTelemetryClient, error) {
	t, e := newHTTPTransport(baseURL, true, true, false, options)
	if e != nil {
		return nil, e
	}
	return &SignedTelemetryClient{NewTelemetryResources(t, t.config.key), t}, nil
}
func NewMailingClient(baseURL string, options ...ClientOption) (*MailingClient, error) {
	t, e := newHTTPTransport(baseURL, true, false, false, options)
	if e != nil {
		return nil, e
	}
	return &MailingClient{NewMailingResources(t, t.config.key), t}, nil
}
func NewMailingServerClient(baseURL string, options ...ClientOption) (*MailingServerClient, error) {
	t, e := newHTTPTransport(baseURL, true, true, false, options)
	if e != nil {
		return nil, e
	}
	return &MailingServerClient{NewMailingServerResources(t, t.config.key), t}, nil
}
func NewAnalyticsClient(baseURL string, options ...ClientOption) (*AnalyticsClient, error) {
	t, e := newHTTPTransport(baseURL, true, false, true, options)
	if e != nil {
		return nil, e
	}
	return &AnalyticsClient{NewAnalyticsResources(t, t.config.key), t}, nil
}
func (c *FeedbackClient) Close() error        { c.transport.close(); return nil }
func (c *SignedFeedbackClient) Close() error  { c.transport.close(); return nil }
func (c *TelemetryClient) Close() error       { c.transport.close(); return nil }
func (c *SignedTelemetryClient) Close() error { c.transport.close(); return nil }
func (c *MailingClient) Close() error         { c.transport.close(); return nil }
func (c *MailingServerClient) Close() error   { c.transport.close(); return nil }
func (c *AnalyticsClient) Close() error       { c.transport.close(); return nil }
