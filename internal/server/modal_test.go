package server

import "testing"

// TestEmbeddingModelVersion guards the embedding-cache key source: the cache is
// keyed by (org_id, model_version, content_hash), so the model version must be
// non-empty and overridable. A model upgrade (env override) must produce a
// different key so vectors from the old model are never reused.
func TestEmbeddingModelVersion(t *testing.T) {
	t.Setenv("PUFFERFS_EMBEDDING_MODEL_VERSION", "")
	if got := embeddingModelVersion(); got != defaultEmbeddingModelVersion {
		t.Fatalf("default model version = %q, want %q", got, defaultEmbeddingModelVersion)
	}

	t.Setenv("PUFFERFS_EMBEDDING_MODEL_VERSION", "  nomic-embed-text-v2  ")
	if got := embeddingModelVersion(); got != "nomic-embed-text-v2" {
		t.Fatalf("override model version = %q, want trimmed %q", got, "nomic-embed-text-v2")
	}

	if defaultEmbeddingModelVersion == "" {
		t.Fatal("defaultEmbeddingModelVersion must be non-empty so cached vectors are model-scoped")
	}

	c := &ModalClient{}
	if got := c.EmbeddingModelVersion(); got != defaultEmbeddingModelVersion {
		t.Fatalf("zero-value client model version = %q, want default %q", got, defaultEmbeddingModelVersion)
	}
}
