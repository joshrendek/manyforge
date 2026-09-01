package snsverify

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net/http"
	"strings"
	"testing"
	"time"
)

type doerFunc func(*http.Request) (*http.Response, error)

func (f doerFunc) Do(r *http.Request) (*http.Response, error) { return f(r) }

func TestVerifyNotificationVersionsAndCertificateCache(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	key, certPEM, roots := signingCertificate(t, now)
	fetches := 0
	v := &Verifier{
		Roots: roots, Now: func() time.Time { return now },
		Client: doerFunc(func(r *http.Request) (*http.Response, error) {
			fetches++
			return response(http.StatusOK, certPEM), nil
		}),
	}
	certURL := "https://sns.us-east-1.amazonaws.com/SimpleNotificationService-test.pem"
	subject := "Launch"
	for _, version := range []string{"1", "2"} {
		msg := Message{
			Type: "Notification", MessageID: "msg-" + version,
			TopicARN: "arn:aws:sns:us-east-1:123456789012:mailing",
			Subject:  &subject, Message: `{"notificationType":"Delivery"}`,
			Timestamp: "2026-08-30T12:00:00Z", SignatureVersion: version,
			SigningCertURL: certURL,
		}
		msg.Signature = signMessage(t, key, msg)
		raw, err := json.Marshal(msg)
		if err != nil {
			t.Fatal(err)
		}
		got, err := v.Verify(context.Background(), raw)
		if err != nil || got.MessageID != msg.MessageID {
			t.Fatalf("Verify(version=%s) = %#v, %v", version, got, err)
		}
	}
	if fetches != 1 {
		t.Fatalf("certificate fetches = %d, want one cached fetch", fetches)
	}
}

func TestVerifySubscriptionAndConfirmHostPin(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	key, certPEM, roots := signingCertificate(t, now)
	confirmed := false
	v := &Verifier{
		Roots: roots, Now: func() time.Time { return now },
		Client: doerFunc(func(r *http.Request) (*http.Response, error) {
			if strings.HasSuffix(r.URL.Path, ".pem") {
				return response(http.StatusOK, certPEM), nil
			}
			confirmed = true
			return response(http.StatusOK, []byte("ok")), nil
		}),
	}
	msg := Message{
		Type: "SubscriptionConfirmation", MessageID: "confirm-1",
		TopicARN: "arn:aws:sns:us-west-2:123456789012:mailing",
		Message:  "Please confirm", Timestamp: "2026-08-30T12:00:00Z",
		Token: "token", SubscribeURL: "https://sns.us-west-2.amazonaws.com/?Action=ConfirmSubscription",
		SignatureVersion: "2",
		SigningCertURL:   "https://sns.us-west-2.amazonaws.com/SimpleNotificationService-test.pem",
	}
	msg.Signature = signMessage(t, key, msg)
	raw, _ := json.Marshal(msg)
	got, err := v.Verify(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := v.Confirm(context.Background(), got.SubscribeURL); err != nil || !confirmed {
		t.Fatalf("Confirm = %v, called=%t", err, confirmed)
	}
	for _, bad := range []string{
		"http://sns.us-west-2.amazonaws.com/", "https://sns.us-west-2.amazonaws.com.evil.test/",
		"https://sns.us-west-2.amazonaws.com:443/", "https://127.0.0.1/",
	} {
		if err := v.Confirm(context.Background(), bad); err == nil {
			t.Errorf("Confirm(%q) unexpectedly succeeded", bad)
		}
	}
}

func TestVerifyRejectsTamperingAndInvalidCertificateURLs(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	key, certPEM, roots := signingCertificate(t, now)
	v := &Verifier{
		Roots: roots, Now: func() time.Time { return now },
		Client: doerFunc(func(*http.Request) (*http.Response, error) {
			return response(http.StatusOK, certPEM), nil
		}),
	}
	base := Message{
		Type: "Notification", MessageID: "message-1", Message: "original",
		TopicARN: "arn:aws:sns:us-east-2:123:topic", Timestamp: "2026-08-30T12:00:00Z",
		SignatureVersion: "2",
		SigningCertURL:   "https://sns.us-east-2.amazonaws.com/SimpleNotificationService-test.pem",
	}
	base.Signature = signMessage(t, key, base)
	tampered := base
	tampered.Message = "changed"
	raw, _ := json.Marshal(tampered)
	if _, err := v.Verify(context.Background(), raw); err == nil {
		t.Fatal("tampered message unexpectedly verified")
	}
	for _, certURL := range []string{
		"http://sns.us-east-2.amazonaws.com/SimpleNotificationService-test.pem",
		"https://sns.us-east-2.amazonaws.com/other.pem",
		"https://sns.us-east-2.amazonaws.com.evil.test/SimpleNotificationService-test.pem",
	} {
		msg := base
		msg.SigningCertURL = certURL
		msg.Signature = signMessage(t, key, msg)
		raw, _ := json.Marshal(msg)
		if _, err := v.Verify(context.Background(), raw); err == nil {
			t.Errorf("certificate URL %q unexpectedly accepted", certURL)
		}
	}
}

func signingCertificate(t *testing.T, now time.Time) (*rsa.PrivateKey, []byte, *x509.CertPool) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "SNS Test Root"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(48 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		BasicConstraintsValid: true, IsCA: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(cert)
	return key, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), roots
}

func signMessage(t *testing.T, key *rsa.PrivateKey, msg Message) string {
	t.Helper()
	canonical, err := canonicalString(msg)
	if err != nil {
		t.Fatal(err)
	}
	var digest []byte
	var hash crypto.Hash
	if msg.SignatureVersion == "1" {
		sum := sha1.Sum([]byte(canonical))
		digest, hash = sum[:], crypto.SHA1
	} else {
		sum := sha256.Sum256([]byte(canonical))
		digest, hash = sum[:], crypto.SHA256
	}
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, hash, digest)
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(sig)
}

func response(status int, body []byte) *http.Response {
	return &http.Response{
		StatusCode: status, Body: io.NopCloser(strings.NewReader(string(body))),
		Header: make(http.Header),
	}
}
