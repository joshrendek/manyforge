package mailing

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/manyforge/manyforge/internal/platform/errs"
)

func TestParseSubscriberCSV(t *testing.T) {
	t.Run("normalizes valid rows and reports bounded invalid rows", func(t *testing.T) {
		var input strings.Builder
		input.WriteString("email,first_name,last_name,tags,attributes\n")
		input.WriteString(" Alice@Example.com , Alice , Smith ,VIP; News ,\"{\"\"plan\"\":\"\"pro\"\"}\"\n")
		for i := 0; i < 120; i++ {
			input.WriteString("not-an-email,,,,\n")
		}
		rows, rowErrors, invalid, err := parseSubscriberCSV(strings.NewReader(input.String()))
		if err != nil {
			t.Fatalf("ParseSubscriberCSV: %v", err)
		}
		if len(rows) != 1 || rows[0].email != "alice@example.com" {
			t.Fatalf("rows = %#v", rows)
		}
		if got := strings.Join(rows[0].tags, ","); got != "vip,news" {
			t.Fatalf("tags = %q", got)
		}
		if len(rowErrors) != maxRowErrors {
			t.Fatalf("errors = %d, want %d", len(rowErrors), maxRowErrors)
		}
		if invalid != 120 {
			t.Fatalf("invalid count = %d, want 120", invalid)
		}
	})

	t.Run("requires email header", func(t *testing.T) {
		_, _, _, err := parseSubscriberCSV(strings.NewReader("name\nAlice\n"))
		if !errors.Is(err, errs.ErrValidation) {
			t.Fatalf("error = %v, want validation", err)
		}
	})

	t.Run("rejects binary", func(t *testing.T) {
		_, _, _, err := parseSubscriberCSV(bytes.NewReader([]byte{0, 1, 2, 3, 0, 5}))
		if !errors.Is(err, errs.ErrValidation) {
			t.Fatalf("error = %v, want validation", err)
		}
	})

	t.Run("rejects files above five MiB", func(t *testing.T) {
		data := append([]byte("email\n"), bytes.Repeat([]byte("a"), maxCSVBytes)...)
		_, _, _, err := parseSubscriberCSV(bytes.NewReader(data))
		if !errors.Is(err, errs.ErrValidation) || !strings.Contains(err.Error(), "5 MiB") {
			t.Fatalf("error = %v", err)
		}
	})
}

func TestListKeyReadResponseCannotMarshalSecret(t *testing.T) {
	key := ListKey{ID: uuid.New(), Secret: "mls_should_not_escape", HasSecret: true}
	raw, err := json.Marshal(toListKeyResp(key))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "mls_should_not_escape") || strings.Contains(string(raw), `"secret":`) {
		t.Fatalf("read response contains secret field: %s", raw)
	}
}

func TestMailingValidation(t *testing.T) {
	if _, err := normalizeEmail("Display <a@example.com>"); !errors.Is(err, errs.ErrValidation) {
		t.Fatalf("display address error = %v", err)
	}
	if _, err := normalizeTags(make([]string, 51)); !errors.Is(err, errs.ErrValidation) {
		t.Fatalf("tag cap error = %v", err)
	}
	if got := slugify("  Product NEWS!  "); got != "product-news" {
		t.Fatalf("slug = %q", got)
	}
}

func TestRandomTokenShapeAndFailure(t *testing.T) {
	token, err := randomToken(bytes.NewReader(make([]byte, 32)), "mlk_")
	if err != nil || !strings.HasPrefix(token, "mlk_") || len(token) != 47 {
		t.Fatalf("token = %q, err = %v", token, err)
	}
	if _, err := randomToken(bytes.NewReader(nil), "mls_"); err == nil {
		t.Fatal("expected entropy failure")
	}
}

func TestNullableStringDistinguishesOmittedNullAndValue(t *testing.T) {
	var omitted, nullValue, value listBody
	if err := json.Unmarshal([]byte(`{}`), &omitted); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(`{"description":null}`), &nullValue); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(`{"description":"hello"}`), &value); err != nil {
		t.Fatal(err)
	}
	if omitted.Description.Set {
		t.Fatal("omitted field marked set")
	}
	if !nullValue.Description.Set || nullValue.Description.Value != nil {
		t.Fatalf("null = %+v", nullValue.Description)
	}
	if !value.Description.Set || value.Description.Value == nil || *value.Description.Value != "hello" {
		t.Fatalf("value = %+v", value.Description)
	}
}

func TestProfileRejectsCredentialsForWrongMode(t *testing.T) {
	svc := &Service{}
	_, err := svc.PutSendingProfile(t.Context(), uuid.New(), uuid.New(), SendingProfileInput{Mode: "resend", FromEmail: "sender@example.com", FromName: "Sender", SES: &SESCredentials{AccessKeyID: "id", SecretAccessKey: "secret"}})
	if !errors.Is(err, errs.ErrValidation) {
		t.Fatalf("error = %v, want validation", err)
	}
}
