package server

import (
	"strings"
	"testing"

	"github.com/pufferfs/pufferfs/pkg/models"
)

func TestNormalizeSyncRequestValidatesSourceRefs(t *testing.T) {
	tests := []struct {
		name    string
		change  models.FileChange
		wantErr string
	}{
		{
			name: "empty source uses legacy file key",
			change: models.FileChange{
				Path:   "docs/a.txt",
				Status: models.StatusAdded,
			},
		},
		{
			name: "exact file source for path",
			change: models.FileChange{
				Path:         "docs/a.txt",
				Status:       models.StatusModified,
				SourceKey:    "files/root-1/docs/a.txt",
				SourceLength: 0,
			},
		},
		{
			name: "bundle source for same root",
			change: models.FileChange{
				Path:         "docs/a.txt",
				Status:       models.StatusAdded,
				SourceKey:    "bundles/root-1/123-000001",
				SourceOffset: 5,
				SourceLength: 10,
			},
		},
		{
			name: "file source must match path",
			change: models.FileChange{
				Path:         "docs/a.txt",
				Status:       models.StatusAdded,
				SourceKey:    "files/root-1/docs/b.txt",
				SourceLength: 10,
			},
			wantErr: "source_key must reference this root",
		},
		{
			name: "source cannot reference another root",
			change: models.FileChange{
				Path:         "docs/a.txt",
				Status:       models.StatusAdded,
				SourceKey:    "bundles/root-2/123-000001",
				SourceLength: 10,
			},
			wantErr: "source_key must reference this root",
		},
		{
			name: "bundle source needs byte length",
			change: models.FileChange{
				Path:      "docs/a.txt",
				Status:    models.StatusAdded,
				SourceKey: "bundles/root-1/123-000001",
			},
			wantErr: "source_length must be positive",
		},
		{
			name: "removed files cannot carry source",
			change: models.FileChange{
				Path:      "docs/a.txt",
				Status:    models.StatusRemoved,
				SourceKey: "files/root-1/docs/a.txt",
			},
			wantErr: "source fields are only valid",
		},
		{
			name: "negative range is rejected",
			change: models.FileChange{
				Path:         "docs/a.txt",
				Status:       models.StatusAdded,
				SourceKey:    "files/root-1/docs/a.txt",
				SourceOffset: -1,
			},
			wantErr: "source_offset must be non-negative",
		},
		{
			name: "move requires old path",
			change: models.FileChange{
				Path:   "docs/new.txt",
				Status: models.StatusMoved,
			},
			wantErr: "old_path is required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := models.SyncRequest{
				RootID:  "root-1",
				Changes: []models.FileChange{tt.change},
			}
			err := normalizeSyncRequest(&req)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("normalizeSyncRequest: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("normalizeSyncRequest error = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}
