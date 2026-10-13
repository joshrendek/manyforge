package inbox

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/manyforge/manyforge/internal/platform/blob"
	"github.com/manyforge/manyforge/internal/ticketing"
)

// fileStore opens a throwaway file:// blob store, so allowed parts are staged.
func fileStore(t *testing.T) blob.Store {
	t.Helper()
	store, err := blob.Open(context.Background(), "file://"+t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestSanitizeAttachmentFilenameStripsControlsAndBounds(t *testing.T) {
	if got := sanitizeAttachmentFilename("bad\x00na\x07me\x7f.exe"); got != "badname.exe" {
		t.Fatalf("sanitize = %q, want badname.exe (jsonb rejects \\u0000)", got)
	}
	long := strings.Repeat("é", 400)
	if got := sanitizeAttachmentFilename(long); len([]rune(got)) != maxAttachmentFilenameRunes {
		t.Fatalf("sanitize kept %d runes, want %d", len([]rune(got)), maxAttachmentFilenameRunes)
	}
	if got := sanitizeAttachmentFilename("\xff\xfeok.bin"); got != "ok.bin" {
		t.Fatalf("sanitize invalid UTF-8 = %q, want ok.bin", got)
	}
}

func TestSniffAttachmentsDropsDisallowedAndOversized(t *testing.T) {
	svc := &Service{blob: fileStore(t), cfg: Config{AttachmentMaxBytes: 64}}
	png := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a, 0, 0, 0, 0x0d, 'I', 'H', 'D', 'R'}
	elf := []byte{0x7f, 'E', 'L', 'F', 2, 1, 1, 0, 0, 0, 0, 0, 0, 0, 0, 0}
	big := append([]byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}, make([]byte, 100)...)
	staged, dropped := svc.sniffAttachments([]ParsedAttachment{
		{FileName: "ok.png", Content: png},
		{FileName: "run.exe", Content: elf},
		{FileName: "huge.png", Content: big},
		{FileName: "empty.txt", Content: nil},
	})
	if len(staged) != 1 || staged[0].filename != "ok.png" || staged[0].contentType != "image/png" || staged[0].size != int64(len(png)) {
		t.Fatalf("staged = %+v, want only ok.png", staged)
	}
	if len(dropped) != 2 ||
		dropped[0] != (ticketing.DroppedAttachment{Filename: "run.exe", DetectedType: "application/octet-stream", Size: int64(len(elf))}) ||
		dropped[1] != (ticketing.DroppedAttachment{Filename: "huge.png", DetectedType: "image/png", Size: int64(len(big))}) {
		t.Fatalf("dropped = %+v", dropped)
	}
}

func TestSniffAttachmentsBoundsDroppedList(t *testing.T) {
	svc := &Service{}
	elf := []byte{0x7f, 'E', 'L', 'F', 2, 1, 1, 0}
	parts := make([]ParsedAttachment, maxDroppedAttachments+10)
	for i := range parts {
		parts[i] = ParsedAttachment{FileName: "x.exe", Content: elf}
	}
	_, dropped := svc.sniffAttachments(parts)
	if len(dropped) != maxDroppedAttachments {
		t.Fatalf("dropped len = %d, want %d (CHECK caps the column at 50)", len(dropped), maxDroppedAttachments)
	}
}

// pngMessage is a multipart message with one allowlisted PNG attachment whose
// Content-Disposition filename parameter is given verbatim (e.g. RFC 2231).
func pngMessage(filenameParam string, png []byte) []byte {
	return []byte("From: a@example.com\r\nTo: b@example.com\r\nSubject: s\r\n" +
		"MIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=B\r\n\r\n" +
		"--B\r\nContent-Type: text/plain\r\n\r\nhi\r\n" +
		"--B\r\nContent-Type: image/png\r\nContent-Disposition: attachment; " + filenameParam +
		"\r\nContent-Transfer-Encoding: base64\r\n\r\n" +
		base64.StdEncoding.EncodeToString(png) + "\r\n--B--\r\n")
}

// SECREVIEW-M3: an ALLOWED attachment's filename is stored too, so it must be
// sanitized exactly like a dropped one — an RFC 2231 %00 would otherwise reach
// the jsonb metadata, fail the ingest, and poison the provider retry loop.
func TestSniffAttachmentsSanitizesAllowedFilename(t *testing.T) {
	png := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a, 0, 0, 0, 0x0d, 'I', 'H', 'D', 'R'}
	parsed, _ := Parse(pngMessage("filename*=UTF-8''bad%00name.png", png))
	if len(parsed.Attachments) != 1 || !strings.Contains(parsed.Attachments[0].FileName, "\x00") {
		t.Fatalf("precondition: parser must surface the NUL filename, got %+v", parsed.Attachments)
	}
	svc := &Service{blob: fileStore(t), cfg: Config{AttachmentMaxBytes: 1 << 20}}
	staged, dropped := svc.sniffAttachments(parsed.Attachments)
	if len(dropped) != 0 || len(staged) != 1 {
		t.Fatalf("staged=%+v dropped=%+v, want the PNG staged", staged, dropped)
	}
	if staged[0].filename != "badname.png" {
		t.Fatalf("staged filename = %q, want badname.png", staged[0].filename)
	}

	long := strings.Repeat("a", 1000) + ".png"
	staged, _ = svc.sniffAttachments([]ParsedAttachment{{FileName: long, Content: png}})
	if len(staged) != 1 || len([]rune(staged[0].filename)) != maxAttachmentFilenameRunes {
		t.Fatalf("staged filename not length-bounded: %+v", staged)
	}
}

// I2a: with no blob store (MANYFORGE_BLOB_URL unset) an allowed part cannot be
// stored, so it is recorded as dropped rather than discarded without a trace.
func TestSniffAttachmentsWithoutBlobStoreDropsAllowedParts(t *testing.T) {
	svc := &Service{cfg: Config{AttachmentMaxBytes: 64}}
	png := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a, 0, 0, 0, 0x0d, 'I', 'H', 'D', 'R'}
	staged, dropped := svc.sniffAttachments([]ParsedAttachment{{FileName: "shot.png", Content: png}})
	if len(staged) != 0 {
		t.Fatalf("staged = %+v, want none without a blob store", staged)
	}
	want := ticketing.DroppedAttachment{Filename: "shot.png", DetectedType: "image/png", Size: int64(len(png))}
	if len(dropped) != 1 || dropped[0] != want {
		t.Fatalf("dropped = %+v, want [%+v]", dropped, want)
	}
}
