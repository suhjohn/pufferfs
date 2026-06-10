# Multi-Root Query Plan

## Goal

Allow users to query across multiple roots they can access while preserving the
existing single-root behavior, ACL rules, visible-generation correctness, and
CLI ergonomics.

## API Request

Extend `models.QueryRequest`:

```go
type QueryRequest struct {
	Query    string   `json:"query"`
	Mode     string   `json:"mode"`
	RootID   string   `json:"root_id,omitempty"`
	RootIDs  []string `json:"root_ids,omitempty"`
	AllRoots bool     `json:"all_roots,omitempty"`
	Glob     string   `json:"glob,omitempty"`
	TopK     int      `json:"top_k"`
}
```

Validation:

- `query` is required.
- `mode` defaults to `hybrid`; allowed values are `hybrid`, `fts`, and `vector`.
- `top_k` defaults to `10`.
- Exactly one root selector is allowed:
  - `root_id`
  - non-empty `root_ids`
  - `all_roots: true`
- `root_ids` should be deduplicated server-side.
- If no selector is supplied, return `400`. The CLI can still auto-detect the
  cwd root and send `root_id`.

## API Response

Extend `models.QueryResult` and `models.QueryResponse`:

```go
type QueryResult struct {
	RootID       string  `json:"root_id,omitempty"`
	RootName     string  `json:"root_name,omitempty"`
	FilePath     string  `json:"file_path"`
	AbsolutePath string  `json:"absolute_path,omitempty"`
	ChunkIndex   int     `json:"chunk_index"`
	Content      string  `json:"content"`
	Score        float64 `json:"score"`
	FileType     string  `json:"file_type"`
	PageNumber   *int    `json:"page_number,omitempty"`
	ImagePath    *string `json:"image_path,omitempty"`
}

type QueryResponse struct {
	Results       []QueryResult `json:"results"`
	Query         string        `json:"query"`
	Mode          string        `json:"mode"`
	RootsSearched int           `json:"roots_searched,omitempty"`
}
```

For single-root queries, `root_id` and `root_name` can still be included. The
CLI should only display root information in human output when more than one root
was searched.

## Access Rules

For every selected root:

- The root must belong to the caller's org.
- The caller must pass `canReadRoot`.
- Results must be constrained to the root's visible committed generation.
- ACL deny filtering must be applied.
- For user-scoped roots, non-admin callers must pass content-proof filtering.
- Roots with no committed visible generation should return zero rows, not an
  error.
- Explicitly requested inaccessible roots should return `404`, matching the
  current single-root behavior.
- `all_roots` should select from `ListAccessibleRoots`, so inaccessible roots
  are naturally excluded.

## Query Execution

Server flow:

1. Resolve selected roots:
   - `root_id`: load one root.
   - `root_ids`: load each root, dedupe, and validate access.
   - `all_roots`: call `ListAccessibleRoots(orgID, userID, role)`.
2. For each root:
   - Load active Turbopuffer namespaces.
   - Load visible generation sequence.
   - Build root-specific filters:
     - optional glob
     - active generation filter
   - Execute query over that root's namespaces.
   - Apply ACL and content-proof post-filtering.
   - Attach `root_id` and `root_name` to each result.
3. Merge globally:
   - For `fts` and `vector`, sort all root results by score descending and
     truncate to `top_k`.
   - For `hybrid`, keep the current per-root namespace merge, then globally
     sort/truncate. Longer term, use global reciprocal rank fusion across all
     root namespaces.
4. Return the top `top_k` results.

For `vector` and `hybrid`, embed the query once and reuse the embedding across
all selected roots.

## CLI

Current behavior remains:

```sh
pufferfs query "renewal notice"
```

This still auto-detects the current root from cwd.

New flags:

```sh
pufferfs query "renewal notice" --root contracts --root handbook
pufferfs query "renewal notice" --all-roots
```

Flag behavior:

- Change `--root` from a single string to a string array.
- One `--root` keeps the current behavior.
- Multiple `--root` values resolve each name/ID and send `root_ids`.
- `--all-roots` sends `all_roots: true`.
- `--root` and `--all-roots` are mutually exclusive.
- If neither is supplied, auto-detect the cwd root and send `root_id`.

Human output:

- Single-root queries preserve the current output.
- Multi-root and all-root queries include `root: <name>` for each result.

JSON output:

- Print the raw response with `root_id`, `root_name`, and `roots_searched`.

## Docs

Update:

- `docs/api-reference.md`: `POST /query` request and response.
- `docs/developer-guide.md`: CLI examples.
- `docs/security-and-data-handling.md`: note that multi-root query reuses
  per-root visibility, ACL, and content-proof filtering.

## Analytics

Extend backend `query_submitted` properties:

- `roots_searched`
- `query_scope`: `single_root`, `selected_roots`, or `all_roots`
- existing sanitized fields such as `mode`, `top_k`, `has_glob`, and
  `result_count`

Do not send query text, glob pattern content, root names, file paths, or result
content to analytics.
