package models

// SourceExtent locates immutable bytes inside a source pack. A manifest can
// reuse extents from a previous version without copying the original bytes.
type SourceExtent struct {
	ObjectKey string `json:"object_key"`
	Offset    int64  `json:"offset"`
	Length    int64  `json:"length"`
}

// SourceManifest describes one exact captured byte sequence, independently of
// its current filesystem path and the physical pack boundaries.
type SourceManifest struct {
	Format      int            `json:"format"`
	ContentHash string         `json:"content_hash"`
	Size        int64          `json:"size"`
	Extents     []SourceExtent `json:"extents"`
}

type SourcePackInitRequest struct {
	Size      int64  `json:"size"`
	ObjectKey string `json:"object_key,omitempty"` // Renew authorization for a retained upload identity.
}

type SourcePackInitResponse struct {
	ObjectKey string              `json:"object_key"`
	URL       string              `json:"url"`
	Headers   map[string][]string `json:"headers"`
}

type SourcePackCompleteRequest struct {
	ObjectKey string `json:"object_key"`
}

type SourcePackCompleteResponse struct {
	ObjectKey string `json:"object_key"`
	Status    string `json:"status"`
}

type SourceMultipartInitRequest struct {
	RequestID string `json:"request_id"` // Client-persisted UUID, unchanged on retry.
	Size      int64  `json:"size"`
}

// SourceMultipartExpired is returned only after S3 confirms that neither the
// persisted multipart session nor its completed object exists.
const SourceMultipartExpired = "source_multipart_expired"

type SourceMultipartInitResponse struct {
	ObjectKey string `json:"object_key"`
	UploadID  string `json:"upload_id"`
	PartSize  int64  `json:"part_size"`
	PartCount int    `json:"part_count"`
	Complete  bool   `json:"complete"`
}

type SourceMultipartPartRequest struct {
	ObjectKey  string `json:"object_key"`
	PartNumber int32  `json:"part_number"`
}

type SourceMultipartPartResponse struct {
	URL     string              `json:"url"`
	Headers map[string][]string `json:"headers"`
	Size    int64               `json:"size"`
}

type SourceMultipartPart struct {
	PartNumber int32  `json:"part_number"`
	ETag       string `json:"etag"`
}

type SourceMultipartCompleteRequest struct {
	ObjectKey string                `json:"object_key"`
	Parts     []SourceMultipartPart `json:"parts"`
}

type CaptureFile struct {
	Path              string          `json:"path"`
	PreviousVersionID string          `json:"previous_version_id"`
	Deleted           bool            `json:"deleted"`
	Source            *SourceManifest `json:"source,omitempty"`
}

// CaptureID and the complete payload must be retained unchanged for retries.
type CaptureVersionsRequest struct {
	CaptureID string        `json:"capture_id"`
	Files     []CaptureFile `json:"files"`
}

type RegisteredFileVersion struct {
	FileID       string `json:"file_id"`
	VersionID    string `json:"version_id"`
	Sequence     int64  `json:"sequence"`
	ExtractionID string `json:"extraction_id"`
	WorkID       string `json:"work_id"`
	Stage        string `json:"stage"`
}

type CaptureVersionsResponse struct {
	CaptureID string                  `json:"capture_id"`
	Versions  []RegisteredFileVersion `json:"versions"`
}

// CapturedFileHead is catalog metadata, not a claim that indexing is complete.
type CapturedFileHead struct {
	FileID            string                `json:"file_id"`
	Path              string                `json:"path"`
	VersionID         string                `json:"version_id"`
	Sequence          int64                 `json:"sequence"`
	IndexedVersionID  string                `json:"indexed_version_id"`
	ContentHash       string                `json:"content_hash"`
	Size              int64                 `json:"size"`
	Deleted           bool                  `json:"deleted"`
	SourceManifestRef string                `json:"source_manifest_ref"`
	ProofCurrent      bool                  `json:"proof_current"`
	Processing        *FileProcessingStatus `json:"processing,omitempty"`
}

// Metadata for the latest registered extraction of the current captured version.
// Complete means that exact extraction is published, not merely transformed.
type FileProcessingStatus struct {
	ExtractionID        string `json:"extraction_id"`
	Revision            string `json:"revision"`
	Stage               string `json:"stage"`
	Status              string `json:"status"`
	AttemptCount        int    `json:"attempt_count"`
	AcknowledgedBatches int    `json:"acknowledged_batches"`
	MutationBatchCount  *int   `json:"mutation_batch_count,omitempty"`
}

type CapturedFilesResponse struct {
	Files      []CapturedFileHead `json:"files"`
	NextCursor string             `json:"next_cursor,omitempty"`
}

type CapturedFileProof struct {
	Path        string `json:"path"`
	VersionID   string `json:"version_id"`
	ContentHash string `json:"content_hash"`
}

type CapturedProofsRequest struct {
	Files []CapturedFileProof `json:"files"`
}
