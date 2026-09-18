# File ingestion and chunking

For deployed roles and queue ownership, start with
[architecture](architecture-and-functionality.md). Every format uses the same
capture → transform → index lifecycle; there are no session-specific handlers
or path/content recipe systems.

## Transformation contracts

| Input | Computation | Chunk location |
| --- | --- | --- |
| Plain text, code, JSONL/NDJSON | Stream UTF-8; redact base64 data-URL payloads; group lines up to 6,000 bytes; split oversized lines at UTF-8 boundaries | Original byte ranges and line offsets |
| PDF | Render every page locally; Gemini 3.5 Flash-Lite Batch extracts Markdown from the image | Page number |
| Word and presentations | Headless LibreOffice → temporary PDF → the same image parsing | Page/slide number |
| Images | Decode/render individual frames/pages → Gemini Batch → Markdown | Frame/page number |
| Spreadsheets | Parse sheet cells into bounded text chunks, retaining cell addresses | Sheet and cell/row metadata |
| Email, calendar, contacts | Parse structured fields into text → bounded chunks | Format-specific metadata |
| Audio and video | FFmpeg decodes the first audio track into temporary 16 kHz mono WAV clips → Gemini Batch transcription | Global start/end seconds; request-scoped speakers |

Documents never bypass image parsing using a native text layer. Converted
PDFs, rendered images and media clips are temporary local files, never S3
artifacts. Original input bytes—including original images—remain in source packs.

JSONL is ordinary text: no JSON projection, parsing/re-serialization or special
session recognition. Explicit `data:...;base64,` payloads become
`[base64 image]` in extracted text, search results and indexed reads. The
data-URL header, surrounding text, whitespace and CRLF are preserved. Original
uploaded bytes, hashes, source packing and append reuse are unchanged.

Redaction happens before chunking with bounded buffering, even for payloads
spanning many S3 reads. JSON-escaped slashes, ASCII Unicode escapes and percent
escapes of base64 characters are supported. Whitespace or another non-base64
character ends the payload; bare base64, empty payloads and non-base64 data URLs
remain unchanged. Headers are bounded to 4 KiB. This is content recognition,
independent of filenames, users and JSON field names; it does not decode images
or call a vision provider. Spreadsheet cell text uses the same redaction rule.

Line numbers remain source line numbers. Byte ranges cover the original source
payload when a chunk intersects a replacement marker; `location.redacted=true`
identifies these text chunks. If a marker straddles a chunk boundary, the two
source ranges overlap that payload. Otherwise concatenated native-text chunks
retain the source text exactly. Existing artifacts are unchanged until a new
extraction (for example, an explicit `sync --force`).

Unknown extensions try this text path;
invalid UTF-8 and binary data fail explicitly.

Current media clips are 60 seconds. Persisted older extraction revisions retain
their original clip boundaries for retries. Video indexes its audio, not visual
frames. Silent video cannot produce a transcript. Gemini speaker diarization is
best-effort: labels are scoped to one request, and distinct voices may merge.

## Supported extensions

- Documents: PDF; DOC, DOCX, DOCM, DOT, DOTX, DOTM, RTF, ODT, OTT, FODT.
- Presentations: PPT, PPTX, PPTM, PPS, PPSX, PPSM, POT, POTX, POTM, ODP, OTP, FODP.
- Spreadsheets: XLS, XLSX, XLSM, XLSB, XLT, XLTX, XLTM, ODS, OTS, FODS, CSV, TSV.
- Images: PNG, JPG, JPEG, JFIF, WebP, GIF, BMP, TIFF, TIF, HEIC, HEIF, AVIF, SVG, APNG, JP2, JPX, J2K.
- Audio: MP3, WAV, M4A, M4B, AAC, FLAC, OGG, OGA, Opus, AIF, AIFF, WMA, AMR.
- Video: MP4, MOV, M4V, MKV, WebM, AVI, MPEG, MPG, WMV, FLV, 3GP, MTS, M2TS, MXF.
- Structured: EML, MSG, VCF, ICS.
- Text: UTF-8 text/code/configuration formats, JSON, JSONL, NDJSON, Markdown and logs.

The format table in [extraction.py](../workers/extraction.py) is authoritative.
Extension support does not promise every codec/container variant. Encrypted,
corrupt, unsupported or undecodable files fail without manufacturing content.
Office macros are not executed; spreadsheet values do not imply recalculation.

## Durable outputs

Each extraction stores compressed ordered JSONL chunks in S3:
`chunk_index`, `content`, `content_hash`, and `location`.
The collector validates provider results before publication. Missing or invalid
results retry only the affected requests.

The background worker reads canonical chunks and regenerates bounded text writes
on each attempt. No index mutation artifacts are stored. Turbopuffer generates Qwen3-Embedding-8B vectors for vector-enabled roots
as it applies those writes. The worker advances the file catalog only after all
batches succeed. Postgres contains metadata and references; vectors live only
in Turbopuffer.

Original source packs and derived artifacts have independent safe retention:
current published/captured versions and retry/append dependencies remain reachable.
Root deletion and obsolete artifacts are cleaned by the background maintenance loop.

See [E2E evidence and limitations](../tests/e2e/README.md).

## Base64 redaction verification

September 17, 2026: local E2E run `4ad05621c2a14af6abde38913fdd614b`
passed initial capture (10.87 s), reads after API/worker restart (2.44 s), and
append/deletion plus forced re-extraction (17.46 s). It checked an 11 MB
encoded payload, JSON/percent escapes, read/chunk boundaries, malformed headers,
CSV cells, exact retained source bytes, source locations, authorization, stored
native vectors and public FTS/vector/hybrid queries. External resources and
Compose volumes were cleaned up. These times describe synthetic E2E phases,
not full-corpus indexing throughput. The implementation is local and has not
been deployed or applied to existing production extractions.

The existing embedding-batch recovery regression also passed as run
`551194be8d45463c95889b3e467359ea`: a real index-worker SIGKILL after provider
acceptance, normal lease expiry, replay, exact reads on both API processes,
native vectors, API restarts and cleanup. Recovery took 299.12 seconds.
