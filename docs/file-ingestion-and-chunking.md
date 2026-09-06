# File ingestion and chunking

For deployed roles and queue ownership, start with
[architecture](architecture-and-functionality.md). Every format uses the same
capture → transform → index lifecycle; there are no session-specific handlers
or path/content recipe systems.

## Transformation contracts

| Input | Computation | Chunk location |
| --- | --- | --- |
| Plain text, code, JSONL/NDJSON | Stream UTF-8; group lines up to 6,000 bytes; split oversized lines at UTF-8 boundaries | Original byte and line offsets |
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
session recognition. Concatenating text chunks reproduces the original UTF-8
bytes, including whitespace and CRLF. Unknown extensions try this text path;
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

The format table in [extraction.py](../modal/extraction.py) is authoritative.
Extension support does not promise every codec/container variant. Encrypted,
corrupt, unsupported or undecodable files fail without manufacturing content.
Office macros are not executed; spreadsheet values do not imply recalculation.

## Durable outputs

Each extraction stores compressed ordered JSONL chunks in S3:
`chunk_index`, `content`, `content_hash`, and `location`.
The collector validates provider results before publication. Missing or invalid
results retry only the affected requests.

The index worker reads chunks, reuses compatible cached vectors or runs Nomic,
then persists replayable index mutation packs in S3. It applies those mutations
to Turbopuffer and advances the file catalog only after all batches succeed.
Postgres contains metadata and references, never vector bodies.

Original source packs, chunks and vectors have independent safe retention:
current published/captured versions and retry/append dependencies remain reachable.
Root deletion and obsolete artifacts are cleaned by the scheduled reconciler.

See [E2E evidence and limitations](../tests/e2e/README.md).
