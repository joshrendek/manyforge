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
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"sync/atomic"
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
		got, err := v.Verify(context.Background(), raw, msg.TopicARN)
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
	got, err := v.Verify(context.Background(), raw, msg.TopicARN)
	if err != nil {
		t.Fatal(err)
	}
	if err := v.Confirm(context.Background(), got.SubscribeURL, msg.TopicARN); err != nil || !confirmed {
		t.Fatalf("Confirm = %v, called=%t", err, confirmed)
	}
	for _, bad := range []string{
		"http://sns.us-west-2.amazonaws.com/", "https://sns.us-west-2.amazonaws.com.evil.test/",
		"https://sns.us-west-2.amazonaws.com:443/", "https://127.0.0.1/",
	} {
		if err := v.Confirm(context.Background(), bad, msg.TopicARN); err == nil {
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
	if _, err := v.Verify(context.Background(), raw, base.TopicARN); err == nil {
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
		if _, err := v.Verify(context.Background(), raw, base.TopicARN); err == nil {
			t.Errorf("certificate URL %q unexpectedly accepted", certURL)
		}
	}
}

// MF-MAIL-WEBHOOK-002 rejects a topic mismatch before any attacker-selected
// certificate retrieval.
func TestMFMailWebhook002TopicMismatchDoesNotFetchCertificate(t *testing.T) {
	fetches := 0
	v := &Verifier{
		Client: doerFunc(func(*http.Request) (*http.Response, error) {
			fetches++
			return response(http.StatusInternalServerError, nil), nil
		}),
	}
	msg := Message{
		Type: "Notification", MessageID: "forged", Message: "{}",
		TopicARN:  "arn:aws:sns:us-east-1:123456789012:attacker-topic",
		Timestamp: "2026-08-30T12:00:00Z", SignatureVersion: "2",
		SigningCertURL: "https://sns.us-east-1.amazonaws.com/SimpleNotificationService-forged.pem",
		Signature:      base64.StdEncoding.EncodeToString([]byte("not-an-rsa-signature")),
	}
	raw, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	expected := "arn:aws:sns:us-east-1:123456789012:expected-topic"
	if _, err = v.Verify(context.Background(), raw, expected); err == nil {
		t.Fatal("topic-mismatched envelope unexpectedly verified")
	}
	if fetches != 0 {
		t.Fatalf("certificate fetches = %d, want zero before topic binding", fetches)
	}
}

func TestCertificateFetchBudgetBoundsUniqueForgedPaths(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	fetches := 0
	v := &Verifier{
		Now: func() time.Time { return now },
		Client: doerFunc(func(*http.Request) (*http.Response, error) {
			fetches++
			return response(http.StatusBadGateway, nil), nil
		}),
	}
	topic := "arn:aws:sns:us-east-1:123456789012:expected-topic"
	for i := range 20 {
		msg := Message{
			Type: "Notification", MessageID: "forged", Message: "{}", TopicARN: topic,
			Timestamp: "2026-08-30T12:00:00Z", SignatureVersion: "2",
			SigningCertURL: fmt.Sprintf("https://sns.us-east-1.amazonaws.com/SimpleNotificationService-forged-%d.pem", i),
			Signature: base64.StdEncoding.EncodeToString([]byte("not-an-rsa-signature")),
		}
		raw, err := json.Marshal(msg)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = v.Verify(context.Background(), raw, topic); err == nil {
			t.Fatal("forged envelope unexpectedly verified")
		}
	}
	if fetches != maxCertificateFetches {
		t.Fatalf("certificate fetches = %d, want fixed budget %d", fetches, maxCertificateFetches)
	}
}

// MF-MAIL-SNS-BUDGET-005 partitions miss and negative-cache budgets by the
// configured topic so one profile cannot starve another profile's first fetch.
func TestMFMailSNSBudget005PartitionsCertificateMissesByExpectedTopic(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	key, certPEM, roots := signingCertificate(t, now)
	sharedURL := "https://sns.us-east-1.amazonaws.com/SimpleNotificationService-shared.pem"
	fetches := 0
	sharedFetches := 0
	v := &Verifier{
		Roots: roots, Now: func() time.Time { return now },
		Client: doerFunc(func(r *http.Request) (*http.Response, error) {
			fetches++
			if r.URL.String() == sharedURL {
				sharedFetches++
				if sharedFetches > 1 {
					return response(http.StatusOK, certPEM), nil
				}
			}
			return response(http.StatusBadGateway, nil), nil
		}),
	}
	topicA := "arn:aws:sns:us-east-1:111111111111:tenant-a"
	for i := range maxCertificateFetches {
		certURL := sharedURL
		if i > 0 {
			certURL = fmt.Sprintf(
				"https://sns.us-east-1.amazonaws.com/SimpleNotificationService-tenant-a-%d.pem", i)
		}
		msg := Message{
			Type: "Notification", MessageID: fmt.Sprintf("forged-%d", i),
			Message: "{}", TopicARN: topicA, Timestamp: "2026-08-30T12:00:00Z",
			SignatureVersion: "2", SigningCertURL: certURL,
			Signature: base64.StdEncoding.EncodeToString([]byte("not-an-rsa-signature")),
		}
		raw, err := json.Marshal(msg)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = v.Verify(context.Background(), raw, topicA); err == nil {
			t.Fatal("tenant A forged envelope unexpectedly verified")
		}
	}

	topicB := "arn:aws:sns:us-east-1:222222222222:tenant-b"
	msgB := Message{
		Type: "Notification", MessageID: "authentic-b", Message: "{}",
		TopicARN: topicB, Timestamp: "2026-08-30T12:00:00Z",
		SignatureVersion: "2", SigningCertURL: sharedURL,
	}
	msgB.Signature = signMessage(t, key, msgB)
	rawB, err := json.Marshal(msgB)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = v.Verify(context.Background(), rawB, topicB); err != nil {
		t.Fatalf("tenant B first authentic certificate fetch was starved: %v", err)
	}
	if fetches != maxCertificateFetches+1 || sharedFetches != 2 {
		t.Fatalf("partitioned fetches=%d shared=%d, want %d/2",
			fetches, sharedFetches, maxCertificateFetches+1)
	}
}

func TestCertificateFetchesUseFairGlobalInflightCeiling(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	key, certPEM, roots := signingCertificate(t, now)
	var active atomic.Int32
	var maxActive atomic.Int32
	started := make(chan struct{}, maxConcurrentCertificateFetches)
	release := make(chan struct{})
	v := &Verifier{
		Roots: roots, Now: func() time.Time { return now },
		Client: doerFunc(func(r *http.Request) (*http.Response, error) {
			current := active.Add(1)
			defer active.Add(-1)
			for {
				observed := maxActive.Load()
				if current <= observed || maxActive.CompareAndSwap(observed, current) {
					break
				}
			}
			if strings.HasSuffix(r.URL.Path, "-authentic.pem") {
				return response(http.StatusOK, certPEM), nil
			}
			started <- struct{}{}
			select {
			case <-release:
				return response(http.StatusBadGateway, nil), nil
			case <-r.Context().Done():
				return nil, r.Context().Err()
			}
		}),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	forgedDone := make(chan error, maxConcurrentCertificateFetches)
	for i := range maxConcurrentCertificateFetches {
		topic := fmt.Sprintf("arn:aws:sns:us-east-1:%012d:forged", i+1)
		msg := Message{
			Type: "Notification", MessageID: fmt.Sprintf("forged-%d", i),
			Message: "{}", TopicARN: topic, Timestamp: "2026-08-30T12:00:00Z",
			SignatureVersion: "2",
			SigningCertURL: fmt.Sprintf(
				"https://sns.us-east-1.amazonaws.com/SimpleNotificationService-forged-%d.pem", i),
			Signature: base64.StdEncoding.EncodeToString([]byte("not-an-rsa-signature")),
		}
		raw, err := json.Marshal(msg)
		if err != nil {
			t.Fatal(err)
		}
		go func() {
			_, verifyErr := v.Verify(ctx, raw, topic)
			forgedDone <- verifyErr
		}()
		<-started
	}

	topicB := "arn:aws:sns:us-east-1:999999999999:authentic"
	msgB := Message{
		Type: "Notification", MessageID: "authentic", Message: "{}",
		TopicARN: topicB, Timestamp: "2026-08-30T12:00:00Z",
		SignatureVersion: "2",
		SigningCertURL: "https://sns.us-east-1.amazonaws.com/SimpleNotificationService-authentic.pem",
	}
	msgB.Signature = signMessage(t, key, msgB)
	rawB, err := json.Marshal(msgB)
	if err != nil {
		t.Fatal(err)
	}
	authenticDone := make(chan error, 1)
	go func() {
		_, verifyErr := v.Verify(ctx, rawB, topicB)
		authenticDone <- verifyErr
	}()

	release <- struct{}{}
	if err = <-authenticDone; err != nil {
		t.Fatalf("authentic topic did not receive the next fair fetch slot: %v", err)
	}
	for range maxConcurrentCertificateFetches - 1 {
		release <- struct{}{}
	}
	for range maxConcurrentCertificateFetches {
		if err = <-forgedDone; err == nil {
			t.Fatal("forged envelope unexpectedly verified")
		}
	}
	if got := maxActive.Load(); got != maxConcurrentCertificateFetches {
		t.Fatalf("maximum concurrent certificate fetches = %d, want %d",
			got, maxConcurrentCertificateFetches)
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
