package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/ticketing"
)

func historicalBatchFixture(id string) ticketing.HistoricalTicket {
	instant := time.Date(2022, 2, 17, 13, 0, 0, 0, time.UTC)
	return ticketing.HistoricalTicket{
		Source: "fixture", ID: id, Subject: "Synthetic request", Status: "solved", Priority: "normal",
		SourceStatus: "Example state", CreatedAt: instant, UpdatedAt: instant,
		RequesterEmail: "fixture@example.com", RequesterName: "Example requester",
		Messages: []ticketing.HistoricalMessage{
			{ID: "example-message", Direction: "note", Body: "Synthetic note", CreatedAt: instant, UpdatedAt: instant, AuthorKind: "staff",
				Attachments: []ticketing.HistoricalAttachment{
					{AttachmentInput: ticketing.AttachmentInput{Filename: "example.txt", Content: []byte("file")}, ID: "example-file", CreatedAt: instant},
				},
			},
		},
	}
}

func writeHistoricalFixtureBundle(t *testing.T, snapshots ...ticketing.HistoricalTicket) (string, string) {
	t.Helper()
	dir := t.TempDir()
	manifest := historicalManifest{Version: 1}
	for i, snapshot := range snapshots {
		name := fmt.Sprintf("snapshot-%d.json", i+1)
		body, err := json.MarshalIndent(snapshot, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), body, 0600); err != nil {
			t.Fatal(err)
		}
		manifest.Snapshots = append(manifest.Snapshots, historicalSnapshotFile{File: name, SHA256: fmt.Sprintf("%x", sha256.Sum256(body))})
	}
	body, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, historicalManifestName), body, 0600); err != nil {
		t.Fatal(err)
	}
	return dir, fmt.Sprintf("%x", sha256.Sum256(body))
}

func TestHistoricalBatchDryRunAndApproval(t *testing.T) {
	t.Setenv("MANYFORGE_ENVIRONMENT", "fixture-invalid-environment")
	dir, digest := writeHistoricalFixtureBundle(t, historicalBatchFixture("example-1"), historicalBatchFixture("example-2"))
	var out bytes.Buffer
	ctx := context.Background()
	if err := importSupportTickets(ctx, []string{"--dry-run", dir}, &out); err != nil {
		t.Fatal(err)
	}
	var result historicalBatchReport
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	want := historicalCounts{Tickets: 2, Messages: 2, InternalNotes: 2, Attachments: 2, AttachmentBytes: 8}
	if !result.Complete || result.Mode != "dry-run" || result.Expected != want || result.ManifestSHA256 != digest || result.Reconciled != nil {
		t.Fatalf("dry run report: %+v", result)
	}
	if strings.Contains(out.String(), "fixture@example.com") || strings.Contains(out.String(), "example-1") {
		t.Fatal("dry run printed snapshot details")
	}
	out.Reset()
	err := importSupportTickets(ctx, []string{"--apply", "--expected-sha256", strings.Repeat("0", 64), uuid.NewString(), uuid.NewString(), dir}, &out)
	if err == nil || !strings.Contains(err.Error(), "approved dry run") || out.Len() != 0 {
		t.Fatalf("unexpected result for mismatched approval: %v", err)
	}
}

func TestHistoricalBatchRejectsDuplicateAndChangedSnapshots(t *testing.T) {
	ctx := context.Background()
	var out bytes.Buffer
	dir, _ := writeHistoricalFixtureBundle(t, historicalBatchFixture("example-1"), historicalBatchFixture("example-1"))
	err := importSupportTickets(ctx, []string{"--dry-run", dir}, &out)
	if err == nil || err.Error() != "snapshot 2: duplicate source identity" {
		t.Fatalf("duplicate identity: %v", err)
	}
	dir, _ = writeHistoricalFixtureBundle(t, historicalBatchFixture("example-1"))
	path := filepath.Join(dir, "snapshot-1.json")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(body, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	err = importSupportTickets(ctx, []string{"--dry-run", dir}, &out)
	if err == nil || err.Error() != "snapshot 1: snapshot checksum mismatch" {
		t.Fatalf("changed snapshot: %v", err)
	}
	if out.Len() != 0 {
		t.Fatal("invalid bundle produced a dry-run report")
	}
}
