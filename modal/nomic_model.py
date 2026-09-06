"""Pinned Nomic model shared by independent query and bulk deployments."""

MODEL = "nomic-ai/nomic-embed-text-v1.5"
MODEL_REVISION = "e9b6763023c676ca8431644204f50c2b100d9aab"
CODE_REVISION = "7710840340a098cfb869c4f65e87cf2b1b70caca"
DIMENSIONS = 768


def cache_model():
    from sentence_transformers import SentenceTransformer

    SentenceTransformer(MODEL, revision=MODEL_REVISION, trust_remote_code=True,
                        model_kwargs={"code_revision": CODE_REVISION})


def load_model(device=None):
    import torch
    from sentence_transformers import SentenceTransformer

    device = device or ("cuda" if torch.cuda.is_available() else "cpu")
    model = SentenceTransformer(MODEL, revision=MODEL_REVISION, trust_remote_code=True,
                                device=device, model_kwargs={"code_revision": CODE_REVISION})
    if device == "cuda":
        model.half()
    return model, device


def encode_texts(model, device, texts, prefix, batch_size=64):
    if not texts:
        return []
    return model.encode([prefix + text for text in texts], normalize_embeddings=True,
                        show_progress_bar=False, batch_size=batch_size, device=device).tolist()
