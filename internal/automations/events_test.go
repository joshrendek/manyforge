package automations

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/platform/errs"
)

func TestPrepareEventValidation(t *testing.T) {
	email := " Person@Example.TEST "
	input, properties, err := prepareEvent(EventInput{Name: " purchased ", Email: &email})
	if err != nil {
		t.Fatal(err)
	}
	if input.Name != "purchased" || input.Email == nil || *input.Email != "person@example.test" || string(properties) != "{}" {
		t.Fatalf("prepared event=%+v properties=%s", input, properties)
	}

	subscriberID := uuid.New()
	tests := []EventInput{
		{Name: "", Email: &email},
		{Name: "x"},
		{Name: "x", Email: &email, SubscriberID: &subscriberID},
		{Name: "x", Email: stringPointer("display <person@example.test>")},
		{Name: "x", Email: &email, Properties: map[string]any{"large": strings.Repeat("x", maxEventPropertiesBytes)}},
	}
	for _, test := range tests {
		if _, _, err = prepareEvent(test); !errors.Is(err, errs.ErrValidation) {
			t.Errorf("prepareEvent(%+v) error=%v", test, err)
		}
	}
}

func TestDecodeEventInputRejectsUnknownAndTrailingJSON(t *testing.T) {
	for _, raw := range []string{
		`{"name":"x","email":"x@example.test","unknown":true}`,
		`{"name":"x","email":"x@example.test"}{}`,
	} {
		if _, err := decodeEventInput([]byte(raw)); !errors.Is(err, errs.ErrValidation) {
			t.Errorf("decodeEventInput(%q) error=%v", raw, err)
		}
	}
}
