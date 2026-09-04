package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
)

// streamJSONL uploads one JSONL object while write produces it. The storage
// uploader owns multipart buffering, so neither side materializes the object.
func (p *syncPipeline) streamJSONL(ctx context.Context, dir, name string, write func(*json.Encoder) error) (string, error) {
	key := fmt.Sprintf("syncs/%s/%s/%s.jsonl", p.generation.ID, dir, safeObjectName(name))
	reader, writer := io.Pipe()
	uploaded := make(chan error, 1)
	go func() {
		err := p.server.s3.UploadStream(ctx, key, reader, "application/x-ndjson")
		_ = reader.CloseWithError(err)
		uploaded <- err
	}()
	writeErr := write(json.NewEncoder(writer))
	_ = writer.CloseWithError(writeErr)
	uploadErr := <-uploaded
	if writeErr != nil {
		return "", writeErr
	}
	if uploadErr != nil {
		return "", fmt.Errorf("uploading %s: %w", key, uploadErr)
	}
	return key, nil
}

func eachJSONL[T any](ctx context.Context, store objectStore, key string, visit func(T) error) error {
	reader, err := store.Open(ctx, key, 0, 0)
	if err != nil {
		return fmt.Errorf("opening %s: %w", key, err)
	}
	defer reader.Close()
	dec := json.NewDecoder(reader)
	for {
		var value T
		if err := dec.Decode(&value); err != nil {
			if err == io.EOF {
				return nil
			}
			return fmt.Errorf("decoding %s: %w", key, err)
		}
		if err := visit(value); err != nil {
			return err
		}
	}
}
