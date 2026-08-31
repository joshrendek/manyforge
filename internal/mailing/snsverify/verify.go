// Package snsverify verifies Amazon SNS HTTP messages before mailing webhook
// handlers trust their contents. Certificate and confirmation URLs are pinned to
// SNS hosts and fetched through the repository's SSRF-screened HTTP client.
package snsverify

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha1" // #nosec G505 -- SNS SignatureVersion 1 requires SHA-1 compatibility.
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/manyforge/manyforge/internal/platform/netsafe"
)

const (
	maxCertificateBytes = 64 << 10
	certificateCacheTTL = 24 * time.Hour
)

var (
	snsHostPattern  = regexp.MustCompile(`^sns\.[a-z0-9-]+\.amazonaws\.com(\.cn)?$`)
	certPathPattern = regexp.MustCompile(`^/SimpleNotificationService-[A-Za-z0-9_-]{1,128}\.pem$`)
)

// Message is the signed outer SNS envelope. Message contains the provider
// payload as an exact JSON string and is decoded only after this envelope passes.
type Message struct {
	Type             string  `json:"Type"`
	MessageID        string  `json:"MessageId"`
	TopicARN         string  `json:"TopicArn"`
	Subject          *string `json:"Subject,omitempty"`
	Message          string  `json:"Message"`
	Timestamp        string  `json:"Timestamp"`
	Token            string  `json:"Token,omitempty"`
	SubscribeURL     string  `json:"SubscribeURL,omitempty"`
	SignatureVersion string  `json:"SignatureVersion"`
	Signature        string  `json:"Signature"`
	SigningCertURL   string  `json:"SigningCertURL"`
}

type httpDoer interface {
	Do(*http.Request) (*http.Response, error)
}

type cachedKey struct {
	key       *rsa.PublicKey
	expiresAt time.Time
}

// Verifier validates SNS envelopes and confirms signed subscriptions. Client,
// Roots, and Now are injectable for deterministic tests.
type Verifier struct {
	Client httpDoer
	Roots  *x509.CertPool
	Now    func() time.Time

	mu    sync.Mutex
	cache map[string]cachedKey
}

// New returns a verifier using a guarded outbound client.
func New() *Verifier {
	client := netsafe.NewClient(10 * time.Second)
	client.CheckRedirect = func(req *http.Request, _ []*http.Request) error {
		return validateSNSURL(req.URL.String(), false)
	}
	return &Verifier{Client: client, Now: time.Now, cache: make(map[string]cachedKey)}
}

// Verify parses raw as an SNS envelope and verifies its RSA signature.
func (v *Verifier) Verify(ctx context.Context, raw []byte) (Message, error) {
	var msg Message
	if err := json.Unmarshal(raw, &msg); err != nil {
		return Message{}, errors.New("sns: invalid envelope")
	}
	canonical, err := canonicalString(msg)
	if err != nil {
		return Message{}, err
	}
	sig, err := base64.StdEncoding.DecodeString(msg.Signature)
	if err != nil || len(sig) == 0 || len(sig) > 1024 {
		return Message{}, errors.New("sns: invalid signature encoding")
	}
	var digest []byte
	var hash crypto.Hash
	switch msg.SignatureVersion {
	case "1":
		sum := sha1.Sum([]byte(canonical)) // #nosec G401 -- required by SNS version 1.
		digest, hash = sum[:], crypto.SHA1
	case "2":
		sum := sha256.Sum256([]byte(canonical))
		digest, hash = sum[:], crypto.SHA256
	default:
		return Message{}, errors.New("sns: unsupported signature version")
	}
	key, err := v.signingKey(ctx, msg.SigningCertURL)
	if err != nil {
		return Message{}, err
	}
	if err := rsa.VerifyPKCS1v15(key, hash, digest, sig); err != nil {
		return Message{}, errors.New("sns: signature verification failed")
	}
	return msg, nil
}

// Confirm follows a signed SubscriptionConfirmation URL after applying the same
// HTTPS and SNS-host constraints as certificate retrieval.
func (v *Verifier) Confirm(ctx context.Context, rawURL string) error {
	if err := validateSNSURL(rawURL, false); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return errors.New("sns: invalid confirmation URL")
	}
	resp, err := v.client().Do(req)
	if err != nil {
		// net/http errors include the full request URL. SubscribeURL contains the
		// confirmation token, so never return or log that error verbatim.
		return errors.New("sns: confirm subscription request failed")
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("sns: confirm subscription: status %d", resp.StatusCode)
	}
	return nil
}

func (v *Verifier) signingKey(ctx context.Context, rawURL string) (*rsa.PublicKey, error) {
	if err := validateSNSURL(rawURL, true); err != nil {
		return nil, err
	}
	now := v.now()
	v.mu.Lock()
	entry, ok := v.cache[rawURL]
	v.mu.Unlock()
	if ok && now.Before(entry.expiresAt) {
		return entry.key, nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, errors.New("sns: invalid certificate URL")
	}
	resp, err := v.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("sns: fetch signing certificate: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("sns: fetch signing certificate: status %d", resp.StatusCode)
	}
	pemBytes, err := io.ReadAll(io.LimitReader(resp.Body, maxCertificateBytes+1))
	if err != nil || len(pemBytes) > maxCertificateBytes {
		return nil, errors.New("sns: invalid signing certificate response")
	}
	certs, err := parseCertificates(pemBytes)
	if err != nil {
		return nil, err
	}
	leaf := certs[0]
	intermediates := x509.NewCertPool()
	for _, cert := range certs[1:] {
		intermediates.AddCert(cert)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{
		Roots: v.Roots, Intermediates: intermediates,
		KeyUsages:   []x509.ExtKeyUsage{x509.ExtKeyUsageAny},
		CurrentTime: now,
	}); err != nil {
		return nil, errors.New("sns: signing certificate is not trusted")
	}
	key, ok := leaf.PublicKey.(*rsa.PublicKey)
	if !ok || key.N.BitLen() < 2048 {
		return nil, errors.New("sns: signing certificate has invalid RSA key")
	}
	expires := now.Add(certificateCacheTTL)
	if leaf.NotAfter.Before(expires) {
		expires = leaf.NotAfter
	}
	v.mu.Lock()
	if v.cache == nil {
		v.cache = make(map[string]cachedKey)
	}
	v.cache[rawURL] = cachedKey{key: key, expiresAt: expires}
	v.mu.Unlock()
	return key, nil
}

func (v *Verifier) client() httpDoer {
	if v.Client != nil {
		return v.Client
	}
	return New().Client
}

func (v *Verifier) now() time.Time {
	if v.Now != nil {
		return v.Now().UTC()
	}
	return time.Now().UTC()
}

func parseCertificates(raw []byte) ([]*x509.Certificate, error) {
	var certs []*x509.Certificate
	for len(raw) > 0 {
		block, rest := pem.Decode(raw)
		if block == nil {
			break
		}
		raw = rest
		if block.Type != "CERTIFICATE" {
			continue
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, errors.New("sns: invalid signing certificate")
		}
		certs = append(certs, cert)
	}
	if len(certs) == 0 {
		return nil, errors.New("sns: invalid signing certificate")
	}
	return certs, nil
}

func validateSNSURL(raw string, certificate bool) error {
	if len(raw) == 0 || len(raw) > 8192 || (certificate && len(raw) > 1024) {
		return errors.New("sns: URL length is invalid")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.RawFragment != "" || u.Fragment != "" {
		return errors.New("sns: URL must be HTTPS")
	}
	if u.Host == "" || u.Host != u.Hostname() || !snsHostPattern.MatchString(strings.ToLower(u.Hostname())) {
		return errors.New("sns: URL host is not allowed")
	}
	if certificate && (!certPathPattern.MatchString(u.EscapedPath()) || u.RawQuery != "") {
		return errors.New("sns: certificate URL path is not allowed")
	}
	return nil
}

func canonicalString(msg Message) (string, error) {
	var fields [][2]string
	switch msg.Type {
	case "Notification":
		fields = append(fields, [2]string{"Message", msg.Message}, [2]string{"MessageId", msg.MessageID})
		if msg.Subject != nil {
			fields = append(fields, [2]string{"Subject", *msg.Subject})
		}
		fields = append(fields,
			[2]string{"Timestamp", msg.Timestamp},
			[2]string{"TopicArn", msg.TopicARN},
			[2]string{"Type", msg.Type},
		)
	case "SubscriptionConfirmation", "UnsubscribeConfirmation":
		fields = [][2]string{
			{"Message", msg.Message}, {"MessageId", msg.MessageID},
			{"SubscribeURL", msg.SubscribeURL}, {"Timestamp", msg.Timestamp},
			{"Token", msg.Token}, {"TopicArn", msg.TopicARN}, {"Type", msg.Type},
		}
	default:
		return "", errors.New("sns: unsupported message type")
	}
	for _, field := range fields {
		if field[1] == "" || strings.ContainsRune(field[0], '\n') {
			return "", errors.New("sns: signed envelope is missing a required field")
		}
	}
	var b strings.Builder
	for _, field := range fields {
		b.WriteString(field[0])
		b.WriteByte('\n')
		b.WriteString(field[1])
		b.WriteByte('\n')
	}
	return b.String(), nil
}
