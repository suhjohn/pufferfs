package server

type indexedChunk struct {
	ID           string
	Content      string
	FilePath     string
	ChunkIndex   any
	ContentHash  string
	FileHash     string
	FileType     string
	RootID       string
	GenerationID string
	PageNumber   any
	ImagePath    any
	Vector       any
}

func indexedChunkFromModal(rootID, generationID, fileHash string, chunk map[string]any) indexedChunk {
	return indexedChunk{
		ID:           strVal(chunk, "id"),
		Content:      strVal(chunk, "content"),
		FilePath:     strVal(chunk, "file_path"),
		ChunkIndex:   chunk["chunk_index"],
		ContentHash:  strVal(chunk, "content_hash"),
		FileHash:     fileHash,
		FileType:     strVal(chunk, "file_type"),
		RootID:       rootID,
		GenerationID: generationID,
		PageNumber:   chunk["page_number"],
		ImagePath:    chunk["image_path"],
	}
}

func indexedChunkFromExisting(rootID, generationID, filePath, fileHash string, chunkIndex int, row map[string]any) indexedChunk {
	return indexedChunk{
		ID:           "",
		Content:      strVal(row, "content"),
		FilePath:     filePath,
		ChunkIndex:   chunkIndex,
		ContentHash:  strVal(row, "content_hash"),
		FileHash:     fileHash,
		FileType:     strVal(row, "file_type"),
		RootID:       rootID,
		GenerationID: generationID,
		PageNumber:   row["page_number"],
		ImagePath:    row["image_path"],
	}
}

func (c indexedChunk) mapRow() map[string]any {
	row := map[string]any{
		"id":            c.ID,
		"content":       c.Content,
		"file_path":     c.FilePath,
		"chunk_index":   c.ChunkIndex,
		"content_hash":  c.ContentHash,
		"file_hash":     c.FileHash,
		"file_type":     c.FileType,
		"root_id":       c.RootID,
		"generation_id": c.GenerationID,
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
