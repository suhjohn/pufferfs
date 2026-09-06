package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	appconfig "github.com/pufferfs/pufferfs/internal/config"
	"github.com/pufferfs/pufferfs/pkg/models"
	"github.com/spf13/cobra"
)

type captureAuditFile struct {
	Path               string                       `json:"path"`
	Historical         bool                         `json:"historical"`
	LocalPresent       bool                         `json:"local_present"`
	LocalMatchesLegacy *bool                        `json:"local_matches_legacy,omitempty"`
	VersionID          string                       `json:"version_id,omitempty"`
	Status             string                       `json:"status"`
	Processing         *models.FileProcessingStatus `json:"processing,omitempty"`
}

type captureAuditReport struct {
	RootID                string             `json:"root_id"`
	SourcePath            string             `json:"source_path"`
	LegacyGenerationID    string             `json:"legacy_generation_id"`
	LegacyGenerationSeq   int64              `json:"legacy_generation_seq"`
	CheckedAt             time.Time          `json:"checked_at"`
	Status                string             `json:"status"`
	LegacyFiles           int                `json:"legacy_files"`
	CatalogFiles          int                `json:"catalog_files"`
	SourceStorageVerified bool               `json:"source_storage_verified"`
	States                map[string]int     `json:"states"`
	Files                 []captureAuditFile `json:"files"`
}

func syncAuditCmd() *cobra.Command {
	var source string
	var jsonOut bool
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:          "audit ROOT_ID --source PATH",
		Short:        "Audit historical recapture and per-file publication without writing",
		SilenceUsage: true, // An incomplete audit must leave --json output parseable.
		Long: `Compare the legacy inventory and captured catalog with local bytes.

This reads every known path, including historical paths now ignored by sync.
It does not scan new, untracked local files, resume pending captures, upload,
enqueue, infer deletions, or verify the continued existence of S3 objects.
Catalog coverage is not a production cutover approval. Run on the source host
with a credential allowed to read the legacy state and sync the entire root.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if source == "" || timeout <= 0 {
				return errors.New("--source and a positive --timeout are required")
			}
			cfg, err := appconfig.Load()
			if err != nil {
				return err
			}
			if cfg.Server.URL == "" {
				return errors.New("server URL not configured; run 'pufferfs init' first")
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
			defer cancel()
			result, err := readCaptureAudit(ctx, newAPIClient(cfg), args[0], source)
			if err != nil {
				return err
			}
			if jsonOut {
				err = writePrettyJSON(cmd.OutOrStdout(), result)
			} else {
				_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s: %d legacy files, %d captured paths; %d indexed, %d deleted.\n",
					result.Status, result.LegacyFiles, result.CatalogFiles, result.States["complete"], result.States["deleted"])
				for _, file := range result.Files {
					if err == nil && file.Status != "complete" && file.Status != "deleted" {
						_, err = fmt.Fprintf(cmd.OutOrStdout(), "  %s  %q\n", file.Status, file.Path)
					}
				}
				if err == nil {
					_, err = fmt.Fprintln(cmd.OutOrStdout(), "Read-only metadata/local-byte audit; S3 contents and search results are NOT verified.")
				}
			}
			if err != nil {
				return err
			}
			if result.Status != "catalog_covered" {
				return errors.New("migration coverage is incomplete; restore missing originals, capture intended local changes, and wait for publication before auditing again")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&source, "source", "", "Local source directory to inspect (required)")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Print every audited path and the summary as JSON")
	cmd.Flags().DurationVar(&timeout, "timeout", 15*time.Minute, "Maximum audit duration")
	_ = cmd.MarkFlagRequired("source")
	return cmd
}

// One-time migration inventory, not a new background pipeline. Memory scales
// with legacy/catalog metadata; each local file is hashed using a 64 KiB buffer.
// Catalog pages are live reads, not an atomic whole-root snapshot.
func readCaptureAudit(ctx context.Context, client *apiClient, rootID, source string) (*captureAuditReport, error) {
	baseURL := "/roots/" + url.PathEscape(rootID)
	get := func(endpoint string, out any) error {
		body, err := client.requestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return err
		}
		return json.Unmarshal(body, out)
	}
	var before models.RootMetadata
	if err := get(baseURL, &before); err != nil {
		return nil, err
	}
	if rootID == "" || before.ID != rootID {
		return nil, errors.New("audit root identity mismatch")
	}
	var legacy map[string]models.FileState
	if err := get(baseURL+"/state", &legacy); err != nil {
		return nil, fmt.Errorf("legacy inventory unavailable: %w", err)
	}
	if legacy == nil {
		return nil, errors.New("legacy inventory is missing; an explicit empty object is required for an empty root")
	}
	files := make(map[string]*models.CapturedFileHead, len(legacy))
	for name := range legacy {
		files[name] = nil
	}
	catalogCount := 0
	if err := client.walkCapturedFiles(ctx, rootID, true, func(file models.CapturedFileHead) error {
		if files[file.Path] != nil {
			return errors.New("duplicate path in captured catalog")
		}
		files[file.Path] = &file
		catalogCount++
		return nil
	}); err != nil {
		return nil, err
	}
	canonical, err := canonicalLocalPath(source)
	if err != nil {
		return nil, err
	}
	localRoot, err := os.OpenRoot(canonical)
	if err != nil {
		return nil, err
	}
	defer localRoot.Close()
	paths := make([]string, 0, len(files))
	for name := range files {
		// Validate even absent paths before passing any server-supplied name to IO.
		if name == "." || !filepath.IsLocal(name) || path.Clean(name) != name || strings.ContainsAny(name, "\\\x00") {
			return nil, fmt.Errorf("invalid inventory path %q", name)
		}
		paths = append(paths, name)
	}
	sort.Strings(paths)
	result := &captureAuditReport{RootID: rootID, SourcePath: canonical,
		LegacyGenerationID: before.VisibleGenerationID, LegacyGenerationSeq: before.VisibleGenerationSeq,
		LegacyFiles: len(legacy), CatalogFiles: catalogCount, Status: "incomplete",
		States: make(map[string]int), Files: make([]captureAuditFile, 0, len(paths))}
	for _, name := range paths {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		state, stable, err := hashCapturedFile(ctx, localRoot, name)
		var local *models.FileState
		switch {
		case errors.Is(err, os.ErrNotExist):
			// Absence is evidence to report, never authorization for a tombstone.
		case err != nil:
			return nil, fmt.Errorf("audit %q: %w", name, err)
		case !stable:
			return nil, fmt.Errorf("audit %q: not a stable regular file; retry after resolving local changes", name)
		default:
			local = &state
		}
		old, historical := legacy[name]
		file := captureAuditFile{Path: name, Historical: historical, LocalPresent: local != nil,
			Status: captureAuditStatus(local, files[name])}
		if historical && local != nil {
			matches := old.ContentHash == local.ContentHash && old.Size == local.Size
			file.LocalMatchesLegacy = &matches
		}
		if remote := files[name]; remote != nil {
			file.VersionID, file.Processing = remote.VersionID, remote.Processing
		}
		result.Files = append(result.Files, file)
		result.States[file.Status]++
	}
	var after models.RootMetadata
	if err = get(baseURL, &after); err != nil {
		return nil, err
	}
	if before.ID != after.ID || before.OrgID != after.OrgID || before.VisibleGenerationID != after.VisibleGenerationID || before.VisibleGenerationSeq != after.VisibleGenerationSeq {
		return nil, errors.New("legacy inventory changed during audit; stop legacy writers and retry")
	}
	result.CheckedAt = time.Now().UTC()
	if len(paths) == 0 {
		result.Status = "empty"
	} else if result.States["complete"]+result.States["deleted"] == len(paths) {
		result.Status = "catalog_covered"
	}
	return result, nil
}

func captureAuditStatus(local *models.FileState, remote *models.CapturedFileHead) string {
	if remote == nil {
		if local == nil {
			return "missing_original"
		}
		return "needs_capture"
	}
	if !remote.Deleted && (remote.SourceManifestRef == "" || !validContentHash(remote.ContentHash) || remote.Size < 0) {
		return "missing_source_reference"
	}
	if local != nil && (remote.Deleted || local.ContentHash != remote.ContentHash || local.Size != remote.Size) {
		return "needs_capture"
	}
	p := remote.Processing
	if p == nil {
		return "missing"
	}
	switch p.Status {
	case "complete":
		if remote.IndexedVersionID != remote.VersionID || p.ExtractionID == "" || p.Stage != "index" {
			return "inconsistent"
		}
		if remote.Deleted {
			return "deleted"
		}
		return "complete"
	case "pending", "running", "waiting_provider", "failed", "superseded", "missing", "inconsistent":
		return p.Status
	default:
		return "inconsistent"
	}
}
