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
