package ticketing

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/platform/db/dbgen"
)

func TestToMessageProjectsDroppedAttachments(t *testing.T) {
	m := dbgen.TicketMessage{
		ID: uuid.New(), TicketID: uuid.New(),
		DroppedAttachments: []byte(`[{"filename":"a.exe","detected_type":"application/octet-stream","size":12}]`),
	}
	got, err := toMessage(m, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := DroppedAttachment{Filename: "a.exe", DetectedType: "application/octet-stream", Size: 12}
	if len(got.DroppedAttachments) != 1 || got.DroppedAttachments[0] != want {
		t.Fatalf("DroppedAttachments = %+v, want [%+v]", got.DroppedAttachments, want)
	}
	raw, err := json.Marshal(toMessageResp(got))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"dropped_attachments":[{"filename":"a.exe","detected_type":"application/octet-stream","size":12}]`) {
		t.Fatalf("response JSON = %s", raw)
	}
}

func TestToMessageDroppedAttachmentsAlwaysAnArray(t *testing.T) {
	for _, stored := range [][]byte{nil, []byte(`[]`), []byte(`null`)} {
		msg, err := toMessage(dbgen.TicketMessage{ID: uuid.New(), DroppedAttachments: stored}, nil)
		if err != nil {
			t.Fatalf("stored %q: %v", stored, err)
		}
		raw, err := json.Marshal(toMessageResp(msg))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(raw), `"dropped_attachments":[]`) {
			t.Fatalf("stored %q -> JSON %s, want dropped_attachments: []", stored, raw)
		}
	}
}

func TestToMessageRejectsCorruptDroppedAttachments(t *testing.T) {
	if _, err := toMessage(dbgen.TicketMessage{ID: uuid.New(), DroppedAttachments: []byte(`not json`)}, nil); err == nil {
		t.Fatal("corrupt dropped_attachments must surface an error, not be swallowed")
	}
}

// manyforge-22zq: the drop reason round-trips to the API, and a row stored before
// the field existed omits it (the client treats a missing reason as unknown).
func TestToMessageProjectsDropReason(t *testing.T) {
	m := dbgen.TicketMessage{
		ID: uuid.New(),
		DroppedAttachments: []byte(`[{"filename":"a.png","detected_type":"image/png","size":3,"reason":"limit_exceeded"},` +
			`{"filename":"old.exe","detected_type":"application/octet-stream","size":4}]`),
	}
	got, err := toMessage(m, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.DroppedAttachments) != 2 || got.DroppedAttachments[0].Reason != DropReasonLimitExceeded || got.DroppedAttachments[1].Reason != "" {
		t.Fatalf("DroppedAttachments = %+v", got.DroppedAttachments)
	}
	raw, err := json.Marshal(toMessageResp(got))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"size":3,"reason":"limit_exceeded"}`) ||
		!strings.Contains(string(raw), `{"filename":"old.exe","detected_type":"application/octet-stream","size":4}`) {
		t.Fatalf("response JSON = %s", raw)
	}
}
