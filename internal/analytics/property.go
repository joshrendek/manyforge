package analytics

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/manyforge/manyforge/internal/platform/errs"
)

const (
	// MaxPropertyRules bounds configuration, collector work, response size, and the later rollup's
	// maximum number of per-site dimensions.
	MaxPropertyRules = 20
	// MaxPropertyRulesPerEvent prevents one event from consuming the site's entire rule budget.
	MaxPropertyRulesPerEvent = 6
	maxPropertyLabelLen      = 64
)

// PropertyRuleInput is one exact custom-event property an operator chooses to retain and report.
type PropertyRuleInput struct {
	EventName   string `json:"event_name"`
	PropertyKey string `json:"property_key"`
	Label       string `json:"label"`
}

// UnmarshalJSON keeps replacement payloads closed-world. Silently accepting a misspelled field
// would appear to save a privacy rule while retaining nothing; accepting unrelated fields makes
// the management contract harder to audit.
func (p *PropertyRuleInput) UnmarshalJSON(data []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	if len(fields) != 3 || fields["event_name"] == nil || fields["property_key"] == nil ||
		fields["label"] == nil {
		return fmt.Errorf("analytics: property rule must contain exactly event_name, property_key, and label")
	}
	type plain PropertyRuleInput
	var value plain
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	*p = PropertyRuleInput(value)
	return nil
}

// PropertyRule is a persisted governed property. EnabledAt is stable while the same event/key
// remains configured, so later rollups can exclude historical raw JSON without retroactive use.
type PropertyRule struct {
	ID          uuid.UUID `json:"id"`
	EventName   string    `json:"event_name"`
	PropertyKey string    `json:"property_key"`
	Label       string    `json:"label"`
	EnabledAt   time.Time `json:"enabled_at"`
}

func propertyKeyOK(value string) bool {
	// Share the collector's bound so a rule can never name a key the public path would discard.
	if value == "" || len(value) > maxPropKeyLen {
		return false
	}
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '_', r == '-', r == '.', r == ':':
		default:
			return false
		}
	}
	return true
}

var prohibitedPropertyFragments = []string{
	"email", "phone", "password", "passwd", "secret", "token", "bearer", "cookie",
	"address", "street", "postalcode", "zipcode", "useragent", "firstname", "lastname",
	"fullname", "creditcard", "cardnumber",
}

var prohibitedPropertyExact = map[string]struct{}{
	"ip": {}, "ipaddress": {}, "ssn": {}, "dob": {}, "birthdate": {}, "dateofbirth": {},
	"name": {}, "userid": {}, "customerid": {}, "accountid": {}, "sessionid": {},
	"deviceid": {}, "fingerprint": {},
}

// propertyKeyProhibited rejects common direct identifiers, secrets, network identifiers, and
// persistent IDs. The compact comparison catches ordinary snake_case, kebab-case, and camelCase
// spellings without retaining or inspecting any property value.
func propertyKeyProhibited(value string) bool {
	compact := strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return unicode.ToLower(r)
		}
		return -1
	}, value)
	if _, blocked := prohibitedPropertyExact[compact]; blocked {
		return true
	}
	for _, fragment := range prohibitedPropertyFragments {
		if strings.Contains(compact, fragment) {
			return true
		}
	}
	return false
}

func normalizePropertyRules(values []PropertyRuleInput) ([]PropertyRuleInput, error) {
	if len(values) > MaxPropertyRules {
		return nil, fmt.Errorf("analytics: at most %d property rules are allowed: %w",
			MaxPropertyRules, errs.ErrValidation)
	}
	out := make([]PropertyRuleInput, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	perEvent := make(map[string]int)
	for _, value := range values {
		eventName := strings.TrimSpace(value.EventName)
		propertyKey := strings.TrimSpace(value.PropertyKey)
		label := strings.TrimSpace(strings.ToValidUTF8(value.Label, ""))
		if NormalizeEventName(eventName) != eventName || !propertyKeyOK(propertyKey) {
			return nil, fmt.Errorf("analytics: invalid event/property pair %q/%q: %w",
				eventName, propertyKey, errs.ErrValidation)
		}
		if propertyKeyProhibited(propertyKey) {
			return nil, fmt.Errorf("analytics: prohibited sensitive property key %q: %w",
				propertyKey, errs.ErrValidation)
		}
		if label == "" || len(label) > maxPropertyLabelLen || !utf8.ValidString(label) ||
			strings.IndexFunc(label, unicode.IsControl) >= 0 {
			return nil, fmt.Errorf("analytics: invalid property label for %q/%q: %w",
				eventName, propertyKey, errs.ErrValidation)
		}
		key := eventName + "\x00" + propertyKey
		if _, duplicate := seen[key]; duplicate {
			return nil, fmt.Errorf("analytics: duplicate property rule %q/%q: %w",
				eventName, propertyKey, errs.ErrValidation)
		}
		seen[key] = struct{}{}
		perEvent[eventName]++
		if perEvent[eventName] > MaxPropertyRulesPerEvent {
			return nil, fmt.Errorf("analytics: event %q exceeds %d property rules: %w",
				eventName, MaxPropertyRulesPerEvent, errs.ErrValidation)
		}
		out = append(out, PropertyRuleInput{
			EventName: eventName, PropertyKey: propertyKey, Label: label,
		})
	}
	return out, nil
}

// ListPropertyRules returns the active analytics site's configured rules in stable display order.
// Unknown, foreign, revoked, non-analytics, and unauthorized sites are indistinguishable.
func (s *Service) ListPropertyRules(
	ctx context.Context, principalID, businessID, clientID uuid.UUID,
) ([]PropertyRule, error) {
	var out []PropertyRule
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		var err error
		out, err = listPropertyRules(ctx, tx, businessID, clientID)
		return err
	})
	if err != nil {
		return nil, mapErr(err)
	}
	return out, nil
}

// ReplacePropertyRules atomically replaces the complete bounded configuration. Existing pairs
// preserve their IDs and activation timestamps; removing and later re-adding a pair starts a new
// activation boundary.
func (s *Service) ReplacePropertyRules(
	ctx context.Context, principalID, businessID, clientID uuid.UUID, values []PropertyRuleInput,
) ([]PropertyRule, error) {
	rules, err := normalizePropertyRules(values)
	if err != nil {
		return nil, err
	}
	payload, err := json.Marshal(rules)
	if err != nil {
		return nil, fmt.Errorf("analytics: encode property rules: %w", err)
	}
	var out []PropertyRule
	err = s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		var outcome string
		if err := tx.QueryRow(ctx,
			`SELECT analytics_replace_property_rules($1, $2, $3::jsonb)`,
			businessID, clientID, payload).Scan(&outcome); err != nil {
			return err
		}
		switch outcome {
		case "updated":
			var err error
			out, err = listPropertyRules(ctx, tx, businessID, clientID)
			return err
		case "not_found":
			return errs.ErrNotFound
		case "invalid":
			return errs.ErrValidation
		default:
			return fmt.Errorf("analytics: unexpected replace-property-rules outcome %q", outcome)
		}
	})
	if err != nil {
		return nil, mapErr(err)
	}
	return out, nil
}

func listPropertyRules(
	ctx context.Context, tx pgx.Tx, businessID, clientID uuid.UUID,
) ([]PropertyRule, error) {
	var exists bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS (
		    SELECT 1 FROM telemetry_client
		     WHERE id=$1 AND business_id=$2 AND kind='analytics'
		       AND status='active' AND revoked_at IS NULL
		)`, clientID, businessID).Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		return nil, errs.ErrNotFound
	}
	rows, err := tx.Query(ctx,
		`SELECT id, event_name, property_key, label, enabled_at
		   FROM analytics_property_rule
		  WHERE client_id=$1 AND business_id=$2
		  ORDER BY event_name, property_key`, clientID, businessID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PropertyRule{}
	for rows.Next() {
		var rule PropertyRule
		if err := rows.Scan(
			&rule.ID, &rule.EventName, &rule.PropertyKey, &rule.Label, &rule.EnabledAt,
		); err != nil {
			return nil, err
		}
		out = append(out, rule)
	}
	return out, rows.Err()
}
