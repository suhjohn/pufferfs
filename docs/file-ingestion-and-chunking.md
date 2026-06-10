# File Ingestion and Chunking

This document describes how PufferFS turns source files into searchable text
chunks, page images, embeddings, and index rows.

## Entry Points

During sync, the Go sync pipeline classifies each changed file and chooses one
of two chunking paths:

- Local chunking for ordinary text/code files.
- Modal chunking for formats that need conversion, parsing, OCR, vision, or
  media processing.

The main routing code is:

- `internal/server/sync_pipeline.go`: calls local chunking or Modal chunking.
- `internal/server/local_chunker.go`: local text/code chunking.
- `modal/app.py`: Modal orchestration, document rendering, embedding, and API
  endpoints.
- `modal/chunkers.py`: format-specific extraction and chunking logic.

Every output chunk has:

- `id`: stable hash of `root_id:file_path` plus `chunk_index`.
- `root_id`
- `file_path`
- `absolute_path`, when available.
- `chunk_index`
- `content`: the text that is embedded and searched.
- `content_hash`
- `file_type`
- `page_number`, for page-based document chunks.
- `image_path`, for rendered document pages and standalone images.

## Format Detection

The Go server detects rich formats before choosing local or Modal chunking:

- Documents: `.pdf`, `.doc`, `.docx`, `.ppt`, `.pptx`
- Images: `.png`, `.jpg`, `.jpeg`, `.gif`, `.svg`, `.webp`, `.bmp`
- Email, calendar, contacts: `.eml`, `.msg`, `.vcf`, `.ics`
- Audio: `.mp3`, `.wav`
- Video: `.mp4`, `.mov`

Unknown files return `auto`. Modal then has a broader extension map for code,
configuration, Markdown, and plain text:

- Code/config: Python, JavaScript, TypeScript, Go, Rust, Java, C, C++, C#,
  Ruby, PHP, Swift, Kotlin, Scala, shell, Lua, Perl, R, SQL, HTML, CSS, SCSS,
  YAML, TOML, JSON, XML, Proto, GraphQL, Terraform, HCL, Dockerfile, Makefile.
- Text/docs: `.md`, `.rst`, `.txt`.
- Unknown extensions fall back to text.

Local chunking has its own code extension map. One important special case:
`.svg` is locally chunkable as text when the server handles it locally, even
though Modal classifies SVG as an image.

## Document Files

Supported document inputs:

- PDF: `.pdf`
- Word: `.doc`, `.docx`
- PowerPoint: `.ppt`, `.pptx`

Pipeline:

1. DOC/DOCX and PPT/PPTX files are converted to PDF with headless LibreOffice.
2. PDF pages are opened with PyMuPDF.
3. Each page is rendered to a JPEG image at 200 DPI.
4. Native PDF text is extracted only as fallback text.
5. The rendered page image is sent to Gemini vision to extract text or describe
   the page.
6. If no Gemini API key is configured, PufferFS falls back to native extracted
   text.
7. The rendered page JPEG is uploaded to S3-compatible storage at:

   ```text
   chunks/<root_id>/<file_path>.<page_number>.jpg
   ```

Chunking rule:

- One chunk per page.
- `chunk_index` equals the zero-based page number.
- `page_number` is set.
- `image_path` points to the rendered page image.

## Standalone Images

Supported image inputs:

- `.png`, `.jpg`, `.jpeg`, `.gif`, `.svg`, `.webp`, `.bmp`

Pipeline:

1. The original image is uploaded to S3-compatible storage at:

   ```text
   chunks/<root_id>/<file_path>
   ```

2. Gemini vision extracts text from the image or describes visual content.
3. If Gemini is unavailable or returns empty text, PufferFS stores a placeholder
   like `[Image: path/to/file.png]`.

Chunking rule:

- One chunk per image.
- `chunk_index` is `0`.
- `image_path` points to the uploaded original image.

## Audio and Video

Supported media inputs:

- Audio: `.mp3`, `.wav`
- Video: `.mp4`, `.mov`

Pipeline:

1. ffprobe reads the duration.
2. ffmpeg cuts the file into overlapping segments.
3. Each segment is uploaded to Gemini as audio/video.
4. Gemini returns a semantic description for retrieval. The prompt asks for
   useful search-index text, not a verbatim transcript.
5. The indexed content is prefixed with the segment time range.

Chunking rule:

- Window size: 6 minutes.
- Overlap: 1 minute.
- Effective stride: 5 minutes.
- Content format:

  ```text
  [00:00-06:00] <segment description>
  ```

If Gemini is unavailable or all segment descriptions are empty, PufferFS emits
one placeholder chunk such as `[Audio file: path/to/file.mp3]` or
`[Video file: path/to/file.mp4]`.

## Email, Calendar, and Contacts

Supported structured inputs:

- Email: `.eml`, `.msg`
- Contacts: `.vcf`
- Calendar/tasks: `.ics`

Email pipeline:

1. Extract headers: Subject, From, To, Cc, Date.
2. Extract body text.
3. Prefer `text/plain` parts.
4. If only HTML is available, strip HTML to text.
5. Ignore attachments.
6. Prefix each chunk with the selected headers.

Contact pipeline:

1. Split VCF content into `BEGIN:VCARD` / `END:VCARD` records.
2. Parse fields such as name, organization, title, email, phone, address, URL,
   note, and categories.
3. Convert each record into friendly text.

Calendar pipeline:

1. Split ICS content into `VEVENT` or `VTODO` records.
2. Parse fields such as title, start, end, due date, location, description,
   organizer, attendees, status, and URL.
3. Convert each record into friendly text.

Chunking rule:

- Email bodies are split into roughly 2000-character chunks with overlap,
  while repeating the header prelude in each chunk.
- VCF/ICS records are packed into chunks up to roughly 2000 characters.
- Oversized records are split with roughly 200 characters of overlap.

## Code, Markdown, and Plain Text

Modal code chunking:

- 300 lines per chunk.
- 50 lines of overlap.
- Used for code/config file types known to Modal.

Modal Markdown/text chunking:

- Split by Markdown headings matching `^#{1,6}\s`.
- Split oversized sections into 2000-character chunks.
- 200 characters of overlap.

Local Go chunking:

- Used when a file can be handled without Modal.
- Code chunks target 3000 characters with 1000 characters of overlap on line
  boundaries.
- Text/Markdown chunks target 2400 characters with 400 characters of overlap.
- Text boundaries prefer paragraph breaks, then line breaks, then sentence
  endings, then spaces.

The local and Modal chunk sizes are not identical. The local path optimizes for
avoiding Modal round trips for ordinary text/code, while Modal owns richer
format extraction.

## Embedding

After extraction and chunking, PufferFS embeds each chunk's `content` with:

```text
nomic-ai/nomic-embed-text-v1.5
```

Chunk contents are embedded with a `search_document:` prefix. Query text is
embedded separately with a `search_query:` prefix. Embeddings are normalized and
stored as vectors for Turbopuffer hybrid/vector search.

The embedding model version is part of the embedding cache key. Changing the
model should also change `PUFFERFS_EMBEDDING_MODEL_VERSION` or the default model
constant so old vectors are not reused.

## Search Index Rows

Index rows include the chunk text, vector, path metadata, content hashes,
file type, optional page/image metadata, and generation validity metadata.
Turbopuffer stores `content` with full-text search enabled and `vector` for ANN
search.

Queries can use:

- Full-text search.
- Vector search.
- Hybrid search.

Results include the original chunk metadata, so document/image results can
refer back to page numbers and stored page images when those fields exist.
