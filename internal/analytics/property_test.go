package analytics

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/manyforge/manyforge/internal/platform/errs"
)

func TestNormalizePropertyRules(t *testing.T) {
	got, err := normalizePropertyRules([]PropertyRuleInput{
		{EventName: " grow_start ", PropertyKey: " mode ", Label: " Game mode "},
		{EventName: "purchase", PropertyKey: "planName", Label: "Plan"},
	})
	if err != nil {
		t.Fatalf("normalizePropertyRules: %v", err)
	}
	if len(got) != 2 || got[0].EventName != "grow_start" || got[0].PropertyKey != "mode" ||
		got[0].Label != "Game mode" {
		t.Fatalf("normalized rules = %+v", got)
	}
}

func TestPropertyRuleInputJSONIsClosedWorld(t *testing.T) {
	for _, body := range []string{
		`{"event_name":"event","property_key":"mode"}`,
		`{"event_name":"event","property_key":"mode","label":"Mode","typo":"ignored"}`,
	} {
		var rule PropertyRuleInput
		if err := json.Unmarshal([]byte(body), &rule); err == nil {
			t.Fatalf("json.Unmarshal(%s) accepted a non-exact rule", body)
		}
	}
}

func TestNormalizePropertyRulesRejectsUnsafeOrUnboundedConfig(t *testing.T) {
	tooMany := make([]PropertyRuleInput, MaxPropertyRules+1)
	for i := range tooMany {
		tooMany[i] = PropertyRuleInput{
			EventName: "event", PropertyKey: fmt.Sprintf("safe%d", i), Label: "Safe",
		}
	}
	tooManyForEvent := make([]PropertyRuleInput, MaxPropertyRulesPerEvent+1)
	for i := range tooManyForEvent {
		tooManyForEvent[i] = PropertyRuleInput{
			EventName: "event", PropertyKey: fmt.Sprintf("safe%d", i), Label: "Safe",
		}
	}
	for name, values := range map[string][]PropertyRuleInput{
		"too many":           tooMany,
		"per event":          tooManyForEvent,
		"bad event":          {{EventName: "has space", PropertyKey: "mode", Label: "Mode"}},
		"reserved event":     {{EventName: "pageview", PropertyKey: "mode", Label: "Mode"}},
		"bad key":            {{EventName: "event", PropertyKey: "has space", Label: "Mode"}},
		"empty label":        {{EventName: "event", PropertyKey: "mode", Label: " "}},
		"control label":      {{EventName: "event", PropertyKey: "mode", Label: "Mode\nsecret"}},
		"duplicate":          {{EventName: "event", PropertyKey: "mode", Label: "A"}, {EventName: "event", PropertyKey: "mode", Label: "B"}},
		"email":              {{EventName: "event", PropertyKey: "customerEmail", Label: "Email"}},
		"persistent id":      {{EventName: "event", PropertyKey: "user_id", Label: "User"}},
		"network id":         {{EventName: "event", PropertyKey: "ip_address", Label: "IP"}},
		"authentication":     {{EventName: "event", PropertyKey: "session_token", Label: "Session"}},
		"device fingerprint": {{EventName: "event", PropertyKey: "fingerprint", Label: "Device"}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := normalizePropertyRules(values); !errors.Is(err, errs.ErrValidation) {
				t.Fatalf("normalizePropertyRules(%+v) = %v, want validation", values, err)
			}
		})
	}
}
