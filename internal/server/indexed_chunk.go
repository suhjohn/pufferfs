package server

import "github.com/pufferfs/pufferfs/pkg/models"

type indexedChunk struct {
	ID                   string
	Content              string
	FilePath             string
	AbsolutePath         string
	ChunkIndex           any
	ContentHash          string
	FileHash             string
	FileType             string
	RootID               string
	GenerationID         string
	GenerationSeq        int64
	ValidToGeneration    string
	ValidToGenerationSeq int64
	PageNumber           any
	ImagePath            any
	Vector               any
}

func indexedChunkFromModal(rootID, generationID string, generationSeq int64, fileHash string, chunk map[string]any) indexedChunk {
	chunkIndex := chunkIndexNumber(chunk["chunk_index"])
	filePath := strVal(chunk, "file_path")
	return indexedChunk{
		ID:            models.MakeGenerationChunkID(rootID, generationID, filePath, chunkIndex),
		Content:       strVal(chunk, "content"),
		FilePath:      filePath,
		AbsolutePath:  strVal(chunk, "absolute_path"),
		ChunkIndex:    chunk["chunk_index"],
		ContentHash:   strVal(chunk, "content_hash"),
		FileHash:      fileHash,
		FileType:      strVal(chunk, "file_type"),
		RootID:        rootID,
		GenerationID:  generationID,
		GenerationSeq: generationSeq,
		PageNumber:    chunk["page_number"],
		ImagePath:     chunk["image_path"],
	}
}

func indexedChunkFromExisting(rootID, generationID string, generationSeq int64, filePath, absolutePath, fileHash string, chunkIndex int, row map[string]any) indexedChunk {
	return indexedChunk{
		ID:            models.MakeGenerationChunkID(rootID, generationID, filePath, chunkIndex),
		Content:       strVal(row, "content"),
		FilePath:      filePath,
		AbsolutePath:  absolutePath,
		ChunkIndex:    chunkIndex,
		ContentHash:   strVal(row, "content_hash"),
		FileHash:      fileHash,
		FileType:      strVal(row, "file_type"),
		RootID:        rootID,
		GenerationID:  generationID,
		GenerationSeq: generationSeq,
		PageNumber:    row["page_number"],
		ImagePath:     row["image_path"],
		Vector:        row["vector"],
	}
}

func (c indexedChunk) mapRow() map[string]any {
	row := map[string]any{
		"id":                        c.ID,
		"content":                   c.Content,
		"file_path":                 c.FilePath,
		"chunk_index":               c.ChunkIndex,
		"content_hash":              c.ContentHash,
		"file_hash":                 c.FileHash,
		"file_type":                 c.FileType,
		"root_id":                   c.RootID,
		"generation_id":             c.GenerationID,
		"valid_from_generation":     c.GenerationID,
		"valid_from_generation_seq": c.GenerationSeq,
		"valid_to_generation":       c.ValidToGeneration,
		"valid_to_generation_seq":   c.ValidToGenerationSeq,
	}
	if c.AbsolutePath != "" {
		row["absolute_path"] = c.AbsolutePath
	}
	if c.PageNumber != nil {
		row["page_number"] = c.PageNumber
	}
	if c.ImagePath != nil {
		row["image_path"] = c.ImagePath
	}
	if c.Vector != nil {
		row["vector"] = c.Vector
	}
	return row
}

func chunkIndexNumber(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	case jsonNumber:
		i, _ := n.Int64()
		return int(i)
	default:
		return 0
	}
}

type jsonNumber interface {
	Int64() (int64, error)
}
