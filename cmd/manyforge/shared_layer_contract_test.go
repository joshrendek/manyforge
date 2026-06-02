//go:build contract

package main

import (
	"context"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/manyforge/manyforge/internal/inbox"
	"github.com/manyforge/manyforge/internal/platform/blob"
	"github.com/manyforge/manyforge/internal/platform/events"
	"github.com/manyforge/manyforge/internal/platform/notify"
)

// TestSharedLayerContracts (T068) pins the method-set + signatures of the
// cross-cutting seams spec-002 introduces as thin first cuts: the inbound source
// adapter (SL ingress), the blob store (SL-E attachments/object storage), the
// notifier (SL-D notifications), and the in-process event bus (SL-C events). The
// OpenAPI-drift gate (drift_002_test.go) pins the HTTP surface; this pins the Go
// seams the same way — a renamed method, a dropped param, or an extra method on
// any of these contracts fails CI, forcing a conscious contract change rather
// than a silent break in a downstream slice (003+ consumes all four).
func TestSharedLayerContracts(t *testing.T) {
	t.Run("inbox.InboundSource", func(t *testing.T) {
		it := reflect.TypeOf((*inbox.InboundSource)(nil)).Elem()
		assertMethodSet(t, it, map[string]interface{}{
			"Provider": (func() string)(nil),
		})
	})

	t.Run("blob.Store", func(t *testing.T) {
		it := reflect.TypeOf((*blob.Store)(nil)).Elem()
		assertMethodSet(t, it, map[string]interface{}{
			"Put":    (func(context.Context, string, []byte, string) error)(nil),
			"Get":    (func(context.Context, string) ([]byte, error))(nil),
			"Delete": (func(context.Context, string) error)(nil),
			"Close":  (func() error)(nil),
		})
	})

	t.Run("notify.Sender", func(t *testing.T) {
		it := reflect.TypeOf((*notify.Sender)(nil)).Elem()
		assertMethodSet(t, it, map[string]interface{}{
			"Send": (func(context.Context, notify.Mail) error)(nil),
		})
	})

	t.Run("events.Bus", func(t *testing.T) {
		// Concrete type, not an interface — pin the Subscribe entrypoint and the
		// Handler shape downstream subscribers must satisfy.
		gotSub := reflect.TypeOf((&events.Bus{}).Subscribe)
		if want := reflect.TypeOf((func(string, events.Handler))(nil)); !sameFunc(gotSub, want) {
			t.Errorf("events.Bus.Subscribe signature %s, want %s", gotSub, want)
		}
		gotHandler := reflect.TypeOf(events.Handler(nil))
		if want := reflect.TypeOf((func(context.Context, pgx.Tx, events.Event) error)(nil)); !sameFunc(gotHandler, want) {
			t.Errorf("events.Handler signature %s, want %s", gotHandler, want)
		}
		// The topic constants are part of the cross-slice contract (producers and
		// consumers in different packages agree by string value, not symbol).
		for topic, want := range map[string]string{
			"TopicBusinessCreated": "business.created",
			"TopicTicketReplied":   "ticket.replied",
			"TopicAttachmentPurge": "attachment.purge",
		} {
			got := map[string]string{
				"TopicBusinessCreated": events.TopicBusinessCreated,
				"TopicTicketReplied":   events.TopicTicketReplied,
				"TopicAttachmentPurge": events.TopicAttachmentPurge,
			}[topic]
			if got != want {
				t.Errorf("events.%s = %q, want %q", topic, got, want)
			}
		}
	})
}

// assertMethodSet checks that interface type `it` has EXACTLY the named methods,
// each with the signature of the corresponding sample func value. Both an extra
// method and a missing/renamed/re-typed one fail — pinning the contract exactly.
func assertMethodSet(t *testing.T, it reflect.Type, want map[string]interface{}) {
	t.Helper()
	if got := it.NumMethod(); got != len(want) {
		t.Errorf("%s: %d methods, want exactly %d (%v)", it, got, len(want), keysOf(want))
	}
	for name, sample := range want {
		m, ok := it.MethodByName(name)
		if !ok {
			t.Errorf("%s: missing method %s", it, name)
			continue
		}
		if wantT := reflect.TypeOf(sample); !sameFunc(m.Type, wantT) {
			t.Errorf("%s.%s: signature %s, want %s", it, name, m.Type, wantT)
		}
	}
}

// sameFunc structurally compares two func types (params + results, variadic-ness),
// so a named func type (e.g. events.Handler) matches its underlying signature.
func sameFunc(a, b reflect.Type) bool {
	if a == nil || b == nil || a.Kind() != reflect.Func || b.Kind() != reflect.Func {
		return false
	}
	if a.NumIn() != b.NumIn() || a.NumOut() != b.NumOut() || a.IsVariadic() != b.IsVariadic() {
		return false
	}
	for i := 0; i < a.NumIn(); i++ {
		if a.In(i) != b.In(i) {
			return false
		}
	}
	for i := 0; i < a.NumOut(); i++ {
		if a.Out(i) != b.Out(i) {
			return false
		}
	}
	return true
}

func keysOf(m map[string]interface{}) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}
