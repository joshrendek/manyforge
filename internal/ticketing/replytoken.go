package ticketing

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"strings"

	"github.com/google/uuid"
)

// ReplyToken is the unforgeable thread-routing token (research R4). It is carried
// in the outbound Reply-To as a VERP/plus-address (support+<token>@domain) and in
// the [#<token>] subject tag, and lets an inbound reply that lost its threading
// headers still attach to the right ticket — without a guessable ticket id leaking
// into mail headers (Constitution: never use a raw resource id as an auth token).
//
// Format: base64url(ticketID[16]) "." base64url(HMAC_SHA256(serverKey, ticketID[16])).
// The id is recoverable from the token, but only a holder of serverKey can forge a
// valid signature, so a requester cannot inject into another ticket by guessing.

// SignReplyToken issues the reply token for a ticket.
func SignReplyToken(ticketID uuid.UUID, serverKey []byte) string {
	id := ticketID[:]
	enc := base64.RawURLEncoding
	return enc.EncodeToString(id) + "." + enc.EncodeToString(hmacSum(serverKey, id))
}

// VerifyReplyToken returns the ticket id encoded in token iff its HMAC verifies
// under serverKey. The signature comparison is constant-time; a forged, tampered,
// or malformed token returns ok=false and must be ignored by threading.
func VerifyReplyToken(token string, serverKey []byte) (uuid.UUID, bool) {
	enc := base64.RawURLEncoding
	dot := strings.IndexByte(token, '.')
	if dot <= 0 || dot == len(token)-1 {
		return uuid.Nil, false
	}
	id, err := enc.DecodeString(token[:dot])
	if err != nil || len(id) != 16 {
		return uuid.Nil, false
	}
	sig, err := enc.DecodeString(token[dot+1:])
	if err != nil {
		return uuid.Nil, false
	}
	if subtle.ConstantTimeCompare(sig, hmacSum(serverKey, id)) != 1 {
		return uuid.Nil, false
	}
	tid, err := uuid.FromBytes(id)
	if err != nil {
		return uuid.Nil, false
	}
	return tid, true
}

func hmacSum(key, msg []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(msg)
	return h.Sum(nil)
}
