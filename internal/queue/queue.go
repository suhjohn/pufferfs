package queue

const (
	StageTransform = "transform"
	StageIndex     = "index"
)

// JobMessage is the shared Go/Python reference-only SQS contract.
type JobMessage struct {
	JobID        string `json:"job_id"`
	WorkID       string `json:"work_id"`
	OrgID        string `json:"org_id"`
	RootID       string `json:"root_id"`
	FileID       string `json:"file_id"`
	VersionID    string `json:"version_id"`
	ExtractionID string `json:"extraction_id"`
	Stage        string `json:"stage"`
}

type ReceivedMessage struct {
	Job     JobMessage
	receipt sqsReceipt
}
