package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/pufferfs/pufferfs/internal/sourcecapture"
	"github.com/pufferfs/pufferfs/pkg/models"
	"golang.org/x/sys/unix"
)

// One directory per capture: immutable pack files, journal.json and a flock.
// Capture creation must fsync the pack files before publishing the journal.
// Source extents named local:<pack name> resolve through the retained mapping;
// already-remote extents are retained unchanged for verified append reuse.
type captureJournal struct {
	Format    int                             `json:"format"`
	ServerURL string                          `json:"server_url"`
	RootID    string                          `json:"root_id"`
	Request   models.CaptureVersionsRequest   `json:"request"`
	Packs     []journalPack                   `json:"packs"`
	State     map[string]models.FileState     `json:"state,omitempty"`
	Dirty     []string                        `json:"dirty,omitempty"`
	Accepted  *models.CaptureVersionsResponse `json:"accepted,omitempty"`
}

type journalPack struct {
	Name      string            `json:"name"`
	Size      int64             `json:"size"`
	Digest    string            `json:"digest"`
	ObjectKey string            `json:"object_key,omitempty"`
	Complete  bool              `json:"complete"`
	Multipart *journalMultipart `json:"multipart,omitempty"`
}

type journalMultipart struct {
	RequestID string                       `json:"request_id"`
	UploadID  string                       `json:"upload_id,omitempty"`
	PartSize  int64                        `json:"part_size,omitempty"`
	Parts     []models.SourceMultipartPart `json:"parts,omitempty"`
}

func saveCaptureJournal(dir string, journal captureJournal) error {
	return saveCaptureJSON(dir, "journal.json", journal)
}

func saveCaptureJSON(dir, name string, value any) error {
	if !filepath.IsLocal(name) || filepath.Base(name) != name {
		return errors.New("invalid capture metadata filename")
	}
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(data) > 4<<20 {
		return errors.New("capture journal exceeds 4 MiB")
	}
	file, err := os.CreateTemp(dir, ".journal-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	if _, err = file.Write(data); err != nil {
		return err
	}
	if err = file.Sync(); err != nil {
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	if err = os.Rename(file.Name(), filepath.Join(dir, name)); err != nil {
		return err
	}
	directory, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func resolvedCaptureRequest(journal captureJournal) (models.CaptureVersionsRequest, error) {
	if journal.Format != 1 || journal.RootID == "" || journal.ServerURL == "" || journal.Request.CaptureID == "" || len(journal.Request.Files) < 1 || len(journal.Request.Files) > 128 {
		return models.CaptureVersionsRequest{}, errors.New("invalid capture journal")
	}
	packs := make(map[string]journalPack, len(journal.Packs))
	for _, pack := range journal.Packs {
		if !filepath.IsLocal(pack.Name) || filepath.Base(pack.Name) != pack.Name || pack.Name == "journal.json" || strings.HasPrefix(pack.Name, ".") || pack.Size < 1 || pack.Size > 128<<20 {
			return models.CaptureVersionsRequest{}, errors.New("invalid journal pack")
		}
		if _, exists := packs["local:"+pack.Name]; exists {
			return models.CaptureVersionsRequest{}, errors.New("duplicate journal pack")
		}
		packs["local:"+pack.Name] = pack
	}
	request := models.CaptureVersionsRequest{CaptureID: journal.Request.CaptureID, Files: append([]models.CaptureFile(nil), journal.Request.Files...)}
	for i := range request.Files {
		file := &request.Files[i]
		if file.Deleted {
			if file.Source != nil {
				return request, errors.New("deleted capture contains source")
			}
			continue
		}
		if file.Source == nil {
			return request, errors.New("capture source missing")
		}
		source := *file.Source
		source.Extents = append([]models.SourceExtent(nil), source.Extents...)
		if err := sourcecapture.ValidateManifest(source); err != nil {
			return request, err
		}
		for n, extent := range source.Extents {
			if !strings.HasPrefix(extent.ObjectKey, "local:") {
				continue
			}
			pack, exists := packs[extent.ObjectKey]
			if !exists || extent.Offset > pack.Size || extent.Length > pack.Size-extent.Offset {
				return request, errors.New("invalid local pack extent")
			}
			if pack.ObjectKey == "" || !pack.Complete {
				return request, errors.New("capture pack not yet complete")
			}
			source.Extents[n].ObjectKey = pack.ObjectKey
		}
		file.Source = &source
	}
	return request, nil
}

func resumeCaptureJournal(ctx context.Context, client *apiClient, dir string) (models.CaptureVersionsResponse, error) {
	var empty models.CaptureVersionsResponse
	root, err := os.OpenRoot(dir)
	if err != nil {
		return empty, err
	}
	defer root.Close()
	lock, err := root.OpenFile(".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return empty, err
	}
	defer lock.Close() // Closing releases the kernel lock, including after a crash.
	if err = unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return empty, fmt.Errorf("capture is already being submitted: %w", err)
	}
	file, err := root.Open("journal.json")
	if err != nil {
		return empty, err
	}
	data, err := io.ReadAll(io.LimitReader(file, (4<<20)+1))
	file.Close()
	if err != nil {
		return empty, err
	}
	if len(data) > 4<<20 {
		return empty, errors.New("capture journal exceeds 4 MiB")
	}
	var journal captureJournal
	if err = json.Unmarshal(data, &journal); err != nil {
		return empty, err
	}
	if journal.ServerURL != client.baseURL || journal.Format != 1 || journal.RootID == "" {
		return empty, errors.New("capture journal server/format mismatch")
	}
	if journal.Accepted != nil {
		if journal.Accepted.CaptureID != journal.Request.CaptureID || len(journal.Accepted.Versions) != len(journal.Request.Files) {
			return empty, errors.New("journal acceptance does not match capture")
		}
		if _, err = resolvedCaptureRequest(journal); err != nil {
			return empty, err
		}
		return *journal.Accepted, nil
	}
	for attempt := 0; attempt < 2; attempt++ {
		err = uploadCapturePacks(ctx, client, dir, root, &journal)
		var accepted models.CaptureVersionsResponse
		if err == nil {
			var request models.CaptureVersionsRequest
			request, err = resolvedCaptureRequest(journal)
			if err == nil {
				accepted, err = client.registerCapturedVersions(ctx, journal.RootID, request)
			}
		}
		if err != nil {
			if attempt == 0 {
				reset, resetErr := resetRetiredCapturePacks(dir, &journal, err)
				if resetErr != nil {
					return empty, resetErr
				}
				if reset {
					continue
				}
			}
			return empty, err
		}
		journal.Accepted = &accepted
		if err = saveCaptureJournal(dir, journal); err != nil {
			return empty, err
		}
		return accepted, nil
	}
	return empty, errors.New("source retirement retry exhausted; retained capture is safe to retry")
}

func uploadCapturePacks(ctx context.Context, client *apiClient, dir string, root *os.Root, journal *captureJournal) error {
	for i := range journal.Packs {
		pack := &journal.Packs[i]
		if pack.Complete {
			continue
		}
		if !filepath.IsLocal(pack.Name) || filepath.Base(pack.Name) != pack.Name || strings.HasPrefix(pack.Name, ".") || pack.Name == "journal.json" || pack.Size < 1 || pack.Size > 128<<20 {
			return errors.New("invalid journal pack")
		}
		// Confirm an earlier PUT before uploading again after an ambiguous reply.
		if pack.ObjectKey != "" && pack.Multipart == nil {
			err := client.completeSourcePack(ctx, journal.RootID, pack.ObjectKey)
			if err == nil {
				pack.Complete = true
				if err = saveCaptureJournal(dir, *journal); err != nil {
					return err
				}
				continue
			}
			var apiErr *apiError
			if !errors.As(err, &apiErr) || apiErr.StatusCode != 409 || retiredPackKeys(err) != nil {
				return err
			}
		}
		source, err := root.Open(pack.Name)
		if err != nil {
			return err
		}
		err = uploadJournalPack(ctx, client, dir, journal, pack, source)
		source.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func uploadJournalPack(ctx context.Context, client *apiClient, dir string, journal *captureJournal, pack *journalPack, source *os.File) error {
	info, err := source.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() != pack.Size {
		return errors.New("captured pack size changed")
	}
	digest := sha256.New()
	if _, err = io.Copy(digest, io.NewSectionReader(source, 0, pack.Size)); err != nil {
		return err
	}
	if fmt.Sprintf("sha256:%x", digest.Sum(nil)) != pack.Digest {
		return errors.New("captured pack digest changed")
	}
	if pack.Multipart != nil || (pack.ObjectKey == "" && pack.Size >= 32<<20) {
		return uploadJournalMultipart(ctx, client, dir, journal, pack, source)
	}
	upload, err := client.initSourcePack(ctx, journal.RootID, pack.Size, pack.ObjectKey)
	if err != nil {
		return err
	}
	pack.ObjectKey = upload.ObjectKey
	// Persist identity before the PUT. Signed URLs and headers stay memory-only.
	if err = saveCaptureJournal(dir, *journal); err != nil {
		return err
	}
	if err = client.putSourcePack(ctx, upload, source, pack.Size); err != nil {
		return err
	}
	if err = client.completeSourcePack(ctx, journal.RootID, pack.ObjectKey); err != nil {
		return err
	}
	pack.Complete = true
	return saveCaptureJournal(dir, *journal)
}
