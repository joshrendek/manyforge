package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/platform/config"
	"github.com/manyforge/manyforge/internal/ticketing"
)

const (
	historicalManifestName    = "manifest.json"
	maxHistoricalManifestJSON = int64(8 << 20)
)

type historicalManifest struct {
	Version   int                      `json:"version"`
	Snapshots []historicalSnapshotFile `json:"snapshots"`
}

type historicalSnapshotFile struct {
	File   string `json:"file"`
	SHA256 string `json:"sha256"`
}

type historicalCounts struct {
	Tickets         int   `json:"tickets"`
	Messages        int   `json:"messages"`
	InternalNotes   int   `json:"internal_notes"`
	Attachments     int   `json:"attachments"`
	AttachmentBytes int64 `json:"attachment_bytes"`
}

func (c *historicalCounts) add(snapshot ticketing.HistoricalTicket) {
	c.Tickets++
	c.Messages += len(snapshot.Messages)
	for _, message := range snapshot.Messages {
		if message.Direction == "note" {
			c.InternalNotes++
		}
		c.Attachments += len(message.Attachments)
		for _, file := range message.Attachments {
			c.AttachmentBytes += int64(len(file.Content))
		}
	}
}

type historicalBatchReport struct {
	Mode           string            `json:"mode"`
	ManifestSHA256 string            `json:"manifest_sha256"`
	Expected       historicalCounts  `json:"expected"`
	Reconciled     *historicalCounts `json:"reconciled,omitempty"`
	Created        int               `json:"created"`
	Replayed       int               `json:"replayed"`
	FailedSnapshot int               `json:"failed_snapshot,omitempty"`
	Complete       bool              `json:"complete"`
}

func importSupportTickets(ctx context.Context, args []string, output io.Writer) error {
	const usage = "usage: manyforge import-support-tickets --dry-run DIRECTORY or --apply --expected-sha256 DIGEST OWNER_PRINCIPAL_ID BUSINESS_ID DIRECTORY"
	flags := flag.NewFlagSet("import-support-tickets", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	dryRun := flags.Bool("dry-run", false, "")
	apply := flags.Bool("apply", false, "")
	expected := flags.String("expected-sha256", "", "")
	if flags.Parse(args) != nil || *dryRun == *apply {
		return errors.New(usage)
	}
	var principal, business uuid.UUID
	var dir string
	if *dryRun {
		if flags.NArg() != 1 || *expected != "" {
			return errors.New(usage)
		}
		dir = flags.Arg(0)
	} else {
		if flags.NArg() != 3 || !validHistoricalSHA256(*expected) {
			return errors.New(usage)
		}
		var principalErr, businessErr error
		principal, principalErr = uuid.Parse(flags.Arg(0))
		business, businessErr = uuid.Parse(flags.Arg(1))
		if principalErr != nil || businessErr != nil || principal == uuid.Nil || business == uuid.Nil {
			return errors.New(usage)
		}
		dir = flags.Arg(2)
	}

	root, err := os.OpenRoot(dir)
	if err != nil {
		return errors.New("cannot open historical bundle directory")
	}
	defer func() { _ = root.Close() }()
	manifest, report, err := preflightHistoricalBundle(ctx, root)
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(output)
	if *dryRun {
		report.Mode, report.Complete = "dry-run", true
		return encoder.Encode(report)
	}
	if !strings.EqualFold(*expected, report.ManifestSHA256) {
		return errors.New("historical manifest checksum differs from the approved dry run")
	}
	cfg, err := config.Load()
	if err != nil {
		return errors.New("cannot load historical import configuration")
	}
	if report.Expected.Attachments > 0 && cfg.BlobURL == "" {
		return ticketing.ErrAttachmentUnavailable
	}
	svc, closeService, err := openHistoricalImportService(ctx, cfg)
	if err != nil {
		return err
	}
	defer closeService()
	report.Mode = "apply"
	report.Reconciled = &historicalCounts{}
	err = applyHistoricalBundle(ctx, root, manifest, &report, svc, principal, business)
	return errors.Join(err, encoder.Encode(report))
}

func preflightHistoricalBundle(ctx context.Context, root *os.Root) (historicalManifest, historicalBatchReport, error) {
	var manifest historicalManifest
	var report historicalBatchReport
	file, err := openHistoricalBundleFile(root, historicalManifestName)
	if err != nil {
		return manifest, report, errors.New("cannot open historical manifest")
	}
	h := sha256.New()
	valid := decodeBoundedJSON(io.TeeReader(file, h), maxHistoricalManifestJSON, &manifest)
	_ = file.Close()
	if !valid || manifest.Version != 1 || len(manifest.Snapshots) == 0 {
		return manifest, report, errors.New("invalid historical manifest")
	}
	report.ManifestSHA256 = hex.EncodeToString(h.Sum(nil))
	files := make(map[string]bool, len(manifest.Snapshots))
	type sourceIdentity struct{ source, id string }
	identities := make(map[sourceIdentity]bool, len(manifest.Snapshots))
	for i, entry := range manifest.Snapshots {
		if err := ctx.Err(); err != nil {
			return manifest, report, err
		}
		if files[entry.File] {
			return manifest, report, fmt.Errorf("snapshot %d: duplicate manifest file", i+1)
		}
		snapshot, err := readHistoricalBundleSnapshot(root, entry)
		if err != nil {
			return manifest, report, fmt.Errorf("snapshot %d: %w", i+1, err)
		}
		identity := sourceIdentity{snapshot.Source, snapshot.ID}
		if identities[identity] {
			return manifest, report, fmt.Errorf("snapshot %d: duplicate source identity", i+1)
		}
		identities[identity], files[entry.File] = true, true
		report.Expected.add(snapshot)
	}
	return manifest, report, nil
}

func validHistoricalSHA256(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size
}

func openHistoricalBundleFile(root *os.Root, name string) (*os.File, error) {
	info, err := root.Lstat(name)
	if err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("snapshot must be a regular file")
	}
	return root.Open(name)
}

func readHistoricalBundleSnapshot(root *os.Root, entry historicalSnapshotFile) (ticketing.HistoricalTicket, error) {
	var snapshot ticketing.HistoricalTicket
	if entry.File == historicalManifestName || !strings.HasSuffix(entry.File, ".json") ||
		strings.ContainsAny(entry.File, "/\\") || !validHistoricalSHA256(entry.SHA256) {
		return snapshot, errors.New("invalid manifest entry")
	}
	file, err := openHistoricalBundleFile(root, entry.File)
	if err != nil {
		return snapshot, errors.New("cannot open historical snapshot")
	}
	defer func() { _ = file.Close() }()
	h := sha256.New()
	if !decodeBoundedJSON(io.TeeReader(file, h), maxHistoricalSnapshotJSON, &snapshot) {
		return snapshot, errors.New("snapshot must be one bounded historical ticket JSON object")
	}
	digest, _ := hex.DecodeString(entry.SHA256)
	if !bytes.Equal(digest, h.Sum(nil)) {
		return snapshot, errors.New("snapshot checksum mismatch")
	}
	return snapshot, ticketing.ValidateHistoricalTicket(snapshot)
}

func applyHistoricalBundle(ctx context.Context, root *os.Root, manifest historicalManifest, report *historicalBatchReport, svc *ticketing.PortalService, principal, business uuid.UUID) error {
	for i, entry := range manifest.Snapshots {
		report.FailedSnapshot = i + 1
		if err := ctx.Err(); err != nil {
			return err
		}
		snapshot, err := readHistoricalBundleSnapshot(root, entry)
		if err != nil {
			return fmt.Errorf("snapshot %d: %w", i+1, err)
		}
		itemCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		result, err := svc.ImportHistoricalTicket(itemCtx, principal, business, snapshot)
		if err == nil {
			if result.Created {
				report.Created++
			} else {
				report.Replayed++
			}
			err = svc.ReconcileHistoricalTicket(itemCtx, principal, business, result.TicketID, snapshot)
		}
		cancel()
		if err != nil {
			return fmt.Errorf("snapshot %d: %w", i+1, safeHistoricalImportError(err))
		}
		report.Reconciled.add(snapshot)
	}
	report.FailedSnapshot = 0
	report.Complete = true
	return nil
}
