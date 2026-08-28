package token

import (
	"bytes"
	"encoding/base64"
	"errors"
	"testing"

	"github.com/google/uuid"
)

func testCodec(t *testing.T) *Codec {
	t.Helper()
	c, err := New(bytes.Repeat([]byte{0x42}, 32))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestUnsubscribeRoundTrip(t *testing.T) {
	c := testCodec(t)
	subscriberID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	campaignID := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	raw := c.EncodeUnsubscribe(subscriberID, campaignID)
	gotSubscriber, gotCampaign, err := c.DecodeUnsubscribe(raw)
	if err != nil {
		t.Fatal(err)
	}
	if gotSubscriber != subscriberID || gotCampaign != campaignID {
		t.Fatalf("decoded (%s, %s), want (%s, %s)", gotSubscriber, gotCampaign, subscriberID, campaignID)
	}
}

func TestStatelessTokensRejectTamperTruncationAndWrongPurpose(t *testing.T) {
	c := testCodec(t)
	deliveryID := uuid.MustParse("33333333-3333-3333-3333-333333333333")
	raw := c.EncodeOpen(deliveryID)

	signed, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		t.Fatal(err)
	}
	tampered := append([]byte(nil), signed...)
	tampered[5] ^= 0x80
	if _, err := c.DecodeOpen(base64.RawURLEncoding.EncodeToString(tampered)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("tampered token error = %v", err)
	}
	if _, err := c.DecodeOpen(base64.RawURLEncoding.EncodeToString(signed[:len(signed)-1])); !errors.Is(err, ErrInvalid) {
		t.Fatalf("truncated token error = %v", err)
	}

	// Re-sign the exact same payload with a different purpose key. Equal framing and payload
	// lengths make this a direct pin of HKDF purpose separation rather than a length rejection.
	wrongPurpose := c.encode(purposeClick, deliveryID[:])
	if _, err := c.DecodeOpen(wrongPurpose); !errors.Is(err, ErrInvalid) {
		t.Fatalf("wrong-purpose token error = %v", err)
	}
}

func TestClickRoundTripAndBounds(t *testing.T) {
	c := testCodec(t)
	id := uuid.MustParse("44444444-4444-4444-4444-444444444444")
	raw, err := c.EncodeClick(id, "https://example.test/a?b=c")
	if err != nil {
		t.Fatal(err)
	}
	gotID, gotURL, err := c.DecodeClick(raw)
	if err != nil {
		t.Fatal(err)
	}
	if gotID != id || gotURL != "https://example.test/a?b=c" {
		t.Fatalf("decoded (%s, %q)", gotID, gotURL)
	}
	if _, err := c.EncodeClick(id, ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty URL error = %v", err)
	}
}

func TestConfirmationStoresOnlyHashAndRejectsMalformed(t *testing.T) {
	c := testCodec(t).WithRand(bytes.NewReader(bytes.Repeat([]byte{0x7a}, confirmSize)))
	raw, stored, err := c.NewConfirmation()
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(stored, []byte(raw)) {
		t.Fatal("stored digest contains raw token")
	}
	lookup, err := HashConfirmation(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(lookup, stored) {
		t.Fatalf("lookup hash %x, want %x", lookup, stored)
	}
	for _, malformed := range []string{"", raw[:len(raw)-1], raw + "!"} {
		if _, err := HashConfirmation(malformed); !errors.Is(err, ErrInvalid) {
			t.Fatalf("HashConfirmation(%q) error = %v", malformed, err)
		}
	}
}

func TestNewRejectsNon32ByteMasterKey(t *testing.T) {
	if _, err := New(make([]byte, 31)); err == nil {
		t.Fatal("New accepted a 31-byte key")
	}
}
