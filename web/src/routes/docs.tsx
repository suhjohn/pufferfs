import { useEffect } from "react";
import { createFileRoute, Link } from "@tanstack/react-router";
import { PixelLogo } from "../components/PixelLogo";
import { ThemeToggle } from "../components/ThemeToggle";

export const Route = createFileRoute("/docs")({
  component: Docs,
});

const COMMANDS = [
  {
    name: "init",
    usage: "pufferfs init",
    detail:
      "Connects the CLI to an account and writes ~/.tpfs/config.toml. Browser login creates a normal user API key, not a platform admin key.",
    flags: [
      "--server-url <url>: use a non-default API server",
      "--api-key <key>: write a provided key without browser login",
      "--manual: write config without logging in",
      "--no-browser: print the login URL instead of opening it",
    ],
    transcript: `$ pufferfs init
Opening browser to connect your PufferFS account...
Config written to /Users/me/.tpfs/config.toml
PufferFS CLI connected as me@example.com.

$ pufferfs init --api-key pfs_...
Config written to /Users/me/.tpfs/config.toml
PufferFS CLI connected.`,
    note: "The second form is useful for CI, sandboxes, or secret-manager injection.",
  },
  {
    name: "sync",
    usage: "pufferfs sync ./handbook --name handbook",
    detail:
      "Scans the folder, computes a Merkle diff, uploads changed files, and blocks until the new generation is committed. With --dry-run, no API request is made.",
    flags: [
      "--name, -n <name>: assign or reuse a root alias",
      "--id <root-id>: attach to an existing root",
      "--scope org|user: choose root visibility when creating a root",
      "--dry-run: show what would change without uploading",
      "--json: print sync result as JSON",
      "--follow, -f: keep syncing on file changes",
    ],
    transcript: `$ pufferfs sync ./handbook --name handbook --dry-run
Building Merkle tree for /Users/me/Documents/handbook...
Merkle tree built in 0s (root hash: sha256:3e8d505d8db80...)
Merkle diff found 2 changed files (skipped unchanged subtrees)
Will upload:
  2 files
  95 B

Excluded:
  .DS_Store
  .git/
  .venv/
  __pycache__/
  node_modules/

$ pufferfs sync ./handbook --name handbook
Building Merkle tree for /Users/me/Documents/handbook...
Merkle diff found 1,284 changed files
Syncing 1,284 changes to root root_8z7m...
Sync job sync_2bd3 started; polling until committed...
Sync status: indexing (912/1,284 files)
Sync complete: 1,284 files processed, 14,602 chunks added`,
    note: "Normal sync is blocking from the user's point of view: the command returns after the server commits or fails the sync.",
  },
  {
    name: "query",
    usage: 'pufferfs query "paid time off" --root handbook --top-k 2',
    detail:
      "Queries the latest committed generation. In-flight and failed syncs are not visible to query results.",
    flags: [
      "--root <id-or-name>: root to search",
      "--top-k <n>: number of results to return",
      "--mode fts|vector|hybrid: retrieval mode",
      "--glob <pattern>: filter by file path glob",
      "--json: print raw response JSON",
    ],
    transcript: `$ pufferfs query "paid time off" --root handbook --top-k 2
1.
score: 0.8124
file: /Users/me/Documents/handbook/policies/time-off.pdf
page_number: 3
chunk_index: 12
file_type: pdf
content: Full-time employees accrue 20 days of paid time off per year, plus
10 company holidays. Unused PTO rolls over up to a 5-day cap.

2.
score: 0.7741
file: /Users/me/Documents/handbook/onboarding/benefits.docx
chunk_index: 4
file_type: docx
content: Parental leave provides 12 weeks paid for the primary caregiver and
6 weeks for secondary caregivers.`,
    note: "Use --json when another program or agent needs structured output instead of the human terminal view.",
  },
  {
    name: "service",
    usage: "pufferfs service install ./handbook --name handbook",
    detail:
      "Installs an OS-managed background sync service. The service runs sync --follow, debounces filesystem changes, retries transient failures, and writes logs through launchd or systemd.",
    flags: [
      "--name, -n <name>: root alias",
      "--id <root-id>: attach to an existing root",
      "--service-name <name>: override generated service name",
      "--debounce <duration>: quiet period before syncing changes",
      "--max-backoff <duration>: maximum retry backoff",
      "--max-same-failures <n>: stop after repeated identical failures",
    ],
    transcript: `$ pufferfs service install ./handbook --name handbook
Installed launchd service: com.pufferfs.handbook

$ pufferfs service start handbook
Service handbook started - re-syncing on every change

$ pufferfs service logs handbook
watching /Users/me/Documents/handbook (debounce 2s)
change: policies/time-off.pdf (modified)    -> 11 chunks updated
change: contracts/acme-msa.pdf (added)      -> 24 chunks added`,
    note: "Use service status, restart, stop, logs, and uninstall to manage the installed watcher.",
  },
  {
    name: "root delete",
    usage: "pufferfs root delete",
    detail:
      "Deletes the current directory's root, or a specific root by name or ID. Deletion removes PufferFS metadata, index rows, storage artifacts, and local PufferFS cache, but it does not delete source files on disk or historical sync job records.",
    flags: ["--yes: skip confirmation prompt"],
    transcript: `$ cd /Users/me/Documents/handbook
$ pufferfs root delete --yes
Deleted root handbook (root_8z7m)
Deleted Turbopuffer namespace: org_acme_root_8z7m
Deleted 1,246 storage objects

$ pufferfs root delete handbook --yes
Deleted root handbook (root_8z7m)`,
    note: "With no root argument, the CLI detects the root containing the current working directory. Without --yes, confirmation requires the root ID. Deletion can return 409 if a sync is active, so stop background services before deleting a root.",
  },
  {
    name: "upgrade",
    usage: "pufferfs upgrade",
    detail:
      "Upgrades CLI installs from the public release manifest.",
    flags: [
      "--manifest-url <url>: use a custom release manifest",
      "--version <version>: install a specific version",
      "--restart-services: restart installed services after upgrade",
      "--force: install even if the current version is already newer or equal",
    ],
    transcript: `$ pufferfs upgrade
Downloading pufferfs 0.4.0 for darwin/arm64...
Upgraded pufferfs to 0.4.0.
Restarted installed pufferfs services.`,
    note: "The CLI also checks for a newer compatible release at most once per day unless PUFFERFS_NO_UPDATE_CHECK is set.",
  },
];

const ENDPOINTS = [
  {
    operationId: "createApiKey",
    method: "POST",
    path: "/auth/api-keys",
    summary: "Create an API key for the current user.",
    auth: "Session cookie, JWT, or existing key with api_keys:write/admin/write.",
    requestBody: `{
  "name": "CI query key",
  "scopes": ["query"]
}`,
    requestFields: [
      ["name", "string", "required", "Human-readable label for the key. Used for display and audit/debugging; it is not the secret value."],
      ["scopes", "string[]", "required", "Explicit least-privilege scopes granted to the key. Use [\"query\"] for read-only search automation; include broader scopes only for sync or root management. Empty scope lists are rejected for newly created user keys."],
    ],
    responses: [
      ["201", "Created. Returns the raw key once.", `{
  "key": "pfs_..."
}`],
      ["403", "Caller lacks key-write scope.", `{
  "error": "api key write scope required"
}`],
    ],
  },
  {
    operationId: "listApiKeys",
    method: "GET",
    path: "/auth/api-keys",
    summary: "List API keys for the current user.",
    auth: "Session cookie, JWT, or key with api_keys:read/api_keys:write/admin/read/write.",
    requestBody: "No request body.",
    requestFields: [],
    responses: [
      ["200", "Key metadata. Raw key values are never returned after creation.", `[
  {
    "id": "key_123",
    "name": "CI query key",
    "scopes": ["query"],
    "created_at": "2026-06-08T05:00:00Z"
  }
]`],
    ],
  },
  {
    operationId: "deleteApiKey",
    method: "DELETE",
    path: "/auth/api-keys/{id}",
    summary: "Revoke an API key.",
    auth: "Session cookie, JWT, or key with api_keys:write/admin/write.",
    requestBody: "No request body.",
    requestFields: [],
    responses: [
      ["200", "The key was revoked.", `{
  "status": "deleted"
}`],
      ["403", "Caller lacks key-write scope.", `{
  "error": "api key write scope required"
}`],
    ],
  },
  {
    operationId: "listOrgMembers",
    method: "GET",
    path: "/org/members",
    summary: "List members in the current organization.",
    auth: "Session cookie, JWT, or user API key.",
    requestBody: "No request body.",
    requestFields: [],
    responses: [
      ["200", "Organization members and roles.", `[
  {
    "user_id": "user_123",
    "email": "teammate@example.com",
    "name": "Ada Lovelace",
    "avatar_url": "",
    "role": "editor",
    "joined_at": "2026-06-08T05:00:00Z"
  }
]`],
    ],
  },
  {
    operationId: "createOrgInvite",
    method: "POST",
    path: "/org/invites",
    summary: "Invite a teammate by email.",
    auth: "Session cookie, JWT, or key with org:admin/admin/write. Caller must be owner or admin.",
    requestBody: `{
  "email": "teammate@example.com",
  "role": "editor"
}`,
    requestFields: [
      ["email", "string", "required", "Email address that can accept the invite on next OAuth sign-in."],
      ["role", "\"owner\" | \"admin\" | \"editor\" | \"viewer\"", "required", "Starting role. Owners can invite any role; admins can invite only editor or viewer."],
    ],
    responses: [
      ["201", "Pending invite.", `{
  "id": "invite_123",
  "email": "teammate@example.com",
  "role": "editor",
  "invited_by_user_id": "user_owner",
  "created_at": "2026-06-08T05:00:00Z",
  "email_sent": true
}`],
      ["403", "Caller cannot manage this role.", `{
  "error": "cannot invite that role"
}`],
    ],
  },
  {
    operationId: "listOrgInvites",
    method: "GET",
    path: "/org/invites",
    summary: "List pending organization invites.",
    auth: "Session cookie, JWT, or user API key.",
    requestBody: "No request body.",
    requestFields: [],
    responses: [
      ["200", "Pending invites.", `[
  {
    "id": "invite_123",
    "email": "teammate@example.com",
    "role": "editor",
    "invited_by_user_id": "user_owner",
    "created_at": "2026-06-08T05:00:00Z"
  }
]`],
    ],
  },
  {
    operationId: "updateOrgMemberRole",
    method: "PUT",
    path: "/org/members/{userId}",
    summary: "Change a member's organization role.",
    auth: "Session cookie, JWT, or key with org:admin/admin/write. Caller must be owner or admin.",
    requestBody: `{
  "role": "viewer"
}`,
    requestFields: [
      ["role", "\"owner\" | \"admin\" | \"editor\" | \"viewer\"", "required", "New member role. Users cannot change their own role. Admins can manage only editor/viewer members and can assign only editor/viewer."],
    ],
    responses: [
      ["200", "Updated member.", `{
  "user_id": "user_123",
  "email": "teammate@example.com",
  "name": "Ada Lovelace",
  "avatar_url": "",
  "role": "viewer",
  "joined_at": "2026-06-08T05:00:00Z"
}`],
      ["403", "Role change is not allowed.", `{
  "error": "cannot change that member role"
}`],
    ],
  },
  {
    operationId: "deleteOrgInvite",
    method: "DELETE",
    path: "/org/invites/{id}",
    summary: "Revoke a pending organization invite.",
    auth: "Session cookie, JWT, or key with org:admin/admin/write. Caller must be able to manage the invited role.",
    requestBody: "No request body.",
    requestFields: [],
    responses: [
      ["200", "The invite was revoked.", `{
  "status": "deleted"
}`],
      ["403", "Caller cannot revoke this invite.", `{
  "error": "cannot revoke that invite"
}`],
    ],
  },
  {
    operationId: "createRoot",
    method: "POST",
    path: "/roots",
    summary: "Create an org or user-scoped root.",
    auth: "Bearer API key or session with sync/root:create/write and sufficient org role.",
    requestBody: `{
  "name": "handbook",
  "source_path": "/Users/me/Documents/handbook",
  "scope": "org"
}`,
    requestFields: [
      ["name", "string", "required", "Stable root name used by CLI commands and users, for example --root handbook."],
      ["source_path", "string", "required", "Original filesystem path on the syncing machine. PufferFS stores this for context; it does not read from this path on the server."],
      ["scope", "\"org\" | \"user\"", "required", "Visibility boundary. org roots are shared with the organization according to role; user roots belong to the creating user."],
    ],
    responses: [
      ["201", "Root metadata.", `{
  "id": "root_8z7m",
  "name": "handbook",
  "source_path": "/Users/me/Documents/handbook",
  "scope": "org",
  "owner_user_id": "",
  "visible_generation_id": "",
  "visible_generation_seq": 0
}`],
      ["403", "Caller cannot create this root scope.", `{
  "error": "editor role required for org root"
}`],
    ],
  },
  {
    operationId: "syncRoot",
    method: "POST",
    path: "/roots/{id}/sync?async=true",
    summary: "Submit a generation for indexing.",
    auth: "Bearer API key or session with sync/write and write access to the root.",
    requestBody: `{
  "protocol_version": 1,
  "base_generation_id": "gen_prev",
  "base_generation_seq": 7,
  "generation_id": "gen_new",
  "changes": [
    {
      "path": "policies/time-off.pdf",
      "status": "MODIFIED",
      "content_hash": "sha256:...",
      "size": 18422,
      "source_key": "syncs/gen_new/sources/files/policies/time-off.pdf"
    }
  ],
  "state_ref": "syncs/gen_new/state/state.json.gz",
  "content_proof": {
    "root_hash": "sha256:...",
    "file_hashes": {},
    "dir_hashes": {}
  }
}`,
    requestFields: [
      ["protocol_version", "number", "required", "Client/server sync wire version. Must match the server's supported SyncProtocolVersion."],
      ["base_generation_id", "string", "required", "Generation the client diffed from. Use the root's current visible generation before applying local changes."],
      ["base_generation_seq", "number", "required", "Monotonic sequence for the base generation. The server rejects stale bases with 409."],
      ["generation_id", "string", "recommended", "Generation returned by sync/init. Source upload objects under syncs/<generation_id>/ are temporary and removed on terminal sync outcomes."],
      ["changes", "array", "required", "Files added, modified, or deleted in this sync. Each item names a path plus content metadata or deletion status."],
      ["changes[].path", "string", "required", "Path relative to the root directory."],
      ["changes[].status", "string", "required", "File state such as ADDED, MODIFIED, or DELETED."],
      ["changes[].content_hash", "string", "required for uploaded content", "SHA-256 content identity used for diffing, proof, and dedupe."],
      ["changes[].size", "number", "required for uploaded content", "File size in bytes. Single-file uploads are limited to 512 MiB."],
      ["changes[].source_key", "string", "required for uploaded content", "Object-storage key where the uploaded file bytes can be read by the server pipeline. New clients should use syncs/<generation_id>/sources/... keys."],
      ["state_ref", "string", "required", "Object-storage reference for the serialized client sync state bundle."],
      ["content_proof.root_hash", "string", "required", "Merkle root hash for the submitted filesystem state."],
      ["content_proof.file_hashes", "object", "required", "Per-file proof data keyed by relative path when needed for filtering and validation."],
      ["content_proof.dir_hashes", "object", "required", "Per-directory proof data keyed by relative path."],
    ],
    responses: [
      ["202", "Accepted. Poll status until completed or failed.", `{
  "root_id": "root_8z7m",
  "sync_job_id": "sync_2bd3",
  "generation_id": "gen_8",
  "generation_seq": 8,
  "status": "running"
}`],
      ["409", "Client synced against a stale generation.", `{
  "error": "stale sync base generation",
  "client_base_generation_seq": 7,
  "current_generation_seq": 9
}`],
    ],
  },
  {
    operationId: "getSyncStatus",
    method: "GET",
    path: "/roots/{id}/sync/status?job_id=sync_2bd3",
    summary: "Read the status of a sync job.",
    auth: "Bearer API key or session with query/sync/read/write and read access to the root.",
    requestBody: "No request body.",
    requestFields: [],
    responses: [
      ["200", "Current job state.", `{
  "id": "sync_2bd3",
  "root_id": "root_8z7m",
  "status": "completed",
  "total_files": 1284,
  "processed": 1284,
  "errors": [],
  "started_at": "2026-06-05T22:00:00Z",
  "finished_at": "2026-06-05T22:04:12Z"
}`],
    ],
  },
  {
    operationId: "queryRoot",
    method: "POST",
    path: "/query",
    summary: "Search a committed root generation.",
    auth: "Bearer API key or session with query/read and read access to the root.",
    requestBody: `{
  "root_id": "root_8z7m",
  "query": "how much paid time off do we get",
  "mode": "hybrid",
  "glob": "*.pdf",
  "top_k": 5
}`,
    requestFields: [
      ["root_id", "string", "required", "Root to query. The caller must be able to read this root; unreadable roots return 404."],
      ["query", "string", "required", "Natural-language or keyword query text."],
      ["mode", "\"hybrid\" | \"vector\" | \"text\"", "optional", "Search strategy. hybrid combines vector and full-text signals and is the normal default."],
      ["glob", "string", "optional", "File path filter applied within the root, for example *.pdf or policies/**."],
      ["top_k", "number", "optional", "Maximum number of chunks to return. Use a small value for agent context and latency."],
    ],
    responses: [
      ["200", "Ranked query results.", `{
  "query": "how much paid time off do we get",
  "mode": "hybrid",
  "results": [
    {
      "file_path": "policies/time-off.pdf",
      "absolute_path": "/Users/me/Documents/handbook/policies/time-off.pdf",
      "chunk_index": 3,
      "content": "Full-time employees accrue 20 days of paid time off...",
      "file_type": "pdf",
      "page_number": 3,
      "score": 0.8124
    }
  ]
}`],
      ["404", "Root missing or unreadable.", `{
  "error": "root not found"
}`],
    ],
  },
];

const IGNORE_RULE_SOURCES = [
  ["Always ignored", "All projects", ".git"],
  ["Built-in defaults", "All projects", "node_modules, .venv, dist, build, caches, compiled artifacts"],
  ["Secret-file patterns", "All projects", ".env, private keys, credentials.json, package-manager auth files"],
  [".gitignore", "Directory where the file lives, recursive", "Gitignore syntax"],
  [".tpfsignore", "Directory where the file lives, recursive", "Gitignore syntax"],
  ["~/.tpfs/.tpfsignore", "All projects for the current user", "Gitignore syntax"],
];

const API_KEY_SCOPES = [
  ["query", "Search committed roots."],
  ["sync", "Create or update sync jobs for roots the caller can write."],
  ["root:delete", "Delete roots the caller is allowed to manage."],
  ["api_keys:read", "List the caller's API key metadata."],
  ["api_keys:write", "Create or revoke the caller's API keys."],
  ["org:admin", "Invite members, revoke invites, and manage eligible member roles."],
  ["read / write / admin / *", "Compatibility aliases for broader trusted automation."],
];

const ROLE_RULES = [
  ["owner", "Can invite any role, assign any role, and manage every member except their own role/removal. The organization must keep at least one owner."],
  ["admin", "Can invite, change, or remove editor and viewer members only. Admins cannot grant owner/admin or manage owner/admin members."],
  ["editor", "Can create and update org roots, depending on root ACLs and API key scopes."],
  ["viewer", "Can read and query accessible org roots."],
];

const SECURITY_SECTIONS = [
  {
    title: "Data handling",
    body: [
      "PufferFS indexes files users choose to sync so teams and agents can search them. Synced files, extracted text, search metadata, and account metadata are processed only to provide the product.",
      "Local folders remain under user control. PufferFS does not delete or modify source files during normal sync, query, or root deletion workflows.",
    ],
  },
  {
    title: "Encryption",
    body: [
      "PufferFS uses HTTPS for data in transit and encrypted cloud storage for customer content and operational data at rest.",
      "Credentials and session data are protected with standard controls such as one-time key display, hashed key storage, secure cookies, and scoped access.",
    ],
  },
  {
    title: "Authentication and access control",
    body: [
      "Access is tied to organization membership, pending invites, user role, root ownership, and API key scope. Users can create least-privilege API keys for automation and rotate or revoke them when needed.",
      "Search and sync requests are checked against the caller's permissions before customer content is returned or updated.",
    ],
  },
  {
    title: "Tenant isolation",
    body: [
      "Customer content is scoped to the organization and root it belongs to. PufferFS does not expose unreadable roots in normal search or sync responses.",
      "Queries run against completed syncs, so incomplete or failed sync jobs are not served as search results.",
    ],
  },
  {
    title: "Sensitive file controls",
    body: [
      "The CLI excludes common secret filenames by default and respects project and user ignore rules, including .gitignore and PufferFS ignore files.",
      "Teams can use folder access controls and ignore rules to keep sensitive paths out of synced roots.",
    ],
  },
  {
    title: "Operations",
    body: [
      "Operational access to production systems is limited to authorized personnel and should be used only for support, reliability, security, and compliance purposes.",
      "Infrastructure and application events are logged to support monitoring, debugging, abuse prevention, and incident response.",
    ],
  },
  {
    title: "Deletion",
    body: [
      "When a root is deleted, PufferFS removes associated indexed content, metadata, stored artifacts, and local PufferFS cache for that root.",
      "Deletion does not remove original source files from the user's machine or records that must be retained for legal, billing, security, or operational reasons.",
    ],
  },
  {
    title: "Vulnerability disclosure",
    body: [
      "Report suspected vulnerabilities to security@pufferfs.com with reproduction steps, impact, and relevant logs or request IDs.",
      "Good-faith security research is welcome when it avoids data destruction, service disruption, spam, social engineering, and access to other users' data.",
    ],
  },
];

const SECURITY_SUBPROCESSORS = [
  {
    name: "Persistence",
    purpose:
      "Stores synced content and the indexes/metadata needed to answer search. Deployments use S3-compatible object storage, PostgreSQL, and Turbopuffer.",
    data: "Uploaded source files, packed bundles, rendered page/OCR images, root state snapshots, sync artifacts, org/user/root metadata, org invites, API-key hashes and key metadata, ACLs, sync jobs/generations, content proofs, extracted text chunks, search metadata, and embedding vectors.",
  },
  {
    name: "Processing",
    purpose:
      "Parses files, extracts text/OCR/images, and computes chunk/query embeddings. Deployments use Modal endpoints when configured.",
    data: "Source files and derived processing artifacts needed to extract text/images, compute embeddings, and create index artifacts.",
  },
];

function Docs() {
  useEffect(() => {
    const scrollToHash = () => {
      const id = decodeURIComponent(window.location.hash.slice(1));
      if (id) document.getElementById(id)?.scrollIntoView();
    };

    scrollToHash();
    window.addEventListener("hashchange", scrollToHash);
    return () => window.removeEventListener("hashchange", scrollToHash);
  }, []);

  return (
    <main className="docs-shell">
      <nav className="landing-nav" aria-label="primary">
        <Link to="/" className="nav-brand">
          <span>pufferfs</span>
        </Link>
        <div className="nav-actions">
          <Link to="/login">console</Link>
          <Link to="/docs">docs</Link>
          <a href="https://github.com/suhjohn/pufferfs">github</a>
          <ThemeToggle />
        </div>
      </nav>

      <section className="docs-hero">
        <div className="hero-mark" aria-hidden="true">
          <PixelLogo size={44} />
          <span>pufferfs</span>
        </div>
        <h1>docs</h1>
        <p className="muted">
          Setup, CLI commands, API reference, and security behavior for PufferFS.
        </p>
      </section>

      <div className="docs-layout">
        <aside className="docs-toc" aria-label="docs navigation">
          <a href="#setup">setup</a>
          <a href="#console">console</a>
          <a href="#ignore-rules">ignore rules</a>
          <a href="#commands">commands</a>
          <a href="#apis">APIs</a>
          <a href="#security">security</a>
        </aside>

        <div className="docs-content">
          <section id="setup" className="docs-section">
            <h2>setup</h2>
            <p>
              Install the CLI, run init, and finish login in the browser.
              PufferFS issues a regular user-scoped CLI key and writes local
              config automatically.
            </p>
            <p>
              The full backend stack needs Postgres, object storage,
              Turbopuffer, Modal endpoints, and optionally NATS workers. For
              local CLI testing without that stack, use <code>--dry-run</code>{" "}
              to inspect filesystem changes before uploading.
            </p>
            <h3>macOS / Linux</h3>
            <div className="code-window">
              <div className="code-titlebar">
                <span>terminal</span>
              </div>
              <pre className="code-pane solo">{`curl -fsSL https://pufferfs.com/install.sh | sh
pufferfs init`}</pre>
            </div>

            <h3>Linux / CI / Docker</h3>
            <div className="code-window">
              <div className="code-titlebar">
                <span>terminal</span>
              </div>
              <pre className="code-pane solo">{`# download a specific version
curl -fsSL https://pufferfs.com/install.sh | PUFFERFS_VERSION=0.2.1 sh

# non-interactive setup for CI or compute jobs
pufferfs init --api-key pfs_...

# one-off environment override
PUFFERFS_API_KEY=pfs_... pufferfs sync . --name workspace`}</pre>
            </div>

            <h3>Go install (development)</h3>
            <div className="code-window">
              <div className="code-titlebar">
                <span>terminal</span>
              </div>
              <pre className="code-pane solo">{`go install github.com/pufferfs/pufferfs/cmd/pufferfs@latest`}</pre>
            </div>
          </section>

          <section id="console" className="docs-section">
            <h2>console</h2>
            <p>
              The console is for account-level operations: viewing synced
              roots, creating and revoking API keys, inviting organization
              members, and changing roles when your role allows it.
            </p>
            <div className="docs-command-list">
              <article className="docs-command">
                <h3>API keys</h3>
                <p>
                  API keys are shown only once, immediately after creation. The
                  server stores a hash and later displays only key metadata:
                  name, scopes, creation time, and revoke controls.
                </p>
                <div className="docs-security-table-wrap">
                  <table className="docs-security-table">
                    <thead>
                      <tr>
                        <th>Scope</th>
                        <th>Allows</th>
                      </tr>
                    </thead>
                    <tbody>
                      {API_KEY_SCOPES.map(([scope, allows]) => (
                        <tr key={scope}>
                          <td>{scope}</td>
                          <td>{allows}</td>
                        </tr>
                      ))}
                    </tbody>
                  </table>
                </div>
              </article>

              <article className="docs-command">
                <h3>organization roles</h3>
                <p>
                  Invites are email-based. When the invited person signs in with
                  that email, PufferFS adds them to the organization with the
                  invited role and clears the pending invite.
                </p>
                <div className="docs-security-table-wrap">
                  <table className="docs-security-table">
                    <thead>
                      <tr>
                        <th>Role</th>
                        <th>Behavior</th>
                      </tr>
                    </thead>
                    <tbody>
                      {ROLE_RULES.map(([role, behavior]) => (
                        <tr key={role}>
                          <td>{role}</td>
                          <td>{behavior}</td>
                        </tr>
                      ))}
                    </tbody>
                  </table>
                </div>
              </article>
            </div>
          </section>

          <section id="ignore-rules" className="docs-section">
            <h2>ignore rules</h2>
            <p>
              PufferFS excludes ignored paths before building the Merkle tree,
              diff, upload set, and search index. Ignored files are not sent to
              the API.
            </p>
            <p>
              This is local CLI behavior: the API server does not read ignore
              files itself. Direct API clients should filter paths before
              submitting a sync request.
            </p>
            <div className="docs-command-list">
              <article className="docs-command">
                <h3>user-defined ignore files</h3>
                <p>
                  Add a <code>.tpfsignore</code> file anywhere in a synced
                  folder for project-specific rules. Add{" "}
                  <code>~/.tpfs/.tpfsignore</code> for global user rules that apply
                  to every project on the current machine. Both use gitignore
                  syntax.
                </p>
                <div className="docs-command-detail">
                  <strong>example .tpfsignore</strong>
                  <pre>{`# ignore generated data in this project
*.csv
scratch/
generated/client/

# scoped .tpfsignore files can be placed in subdirectories too`}</pre>
                </div>
                <div className="docs-command-detail">
                  <strong>verify before uploading</strong>
                  <pre>{`pufferfs sync --dry-run .`}</pre>
                </div>
              </article>

              <article className="docs-command">
                <h3>rule sources</h3>
                <p>
                  A file is excluded if any ignore source matches it. The CLI
                  also respects existing <code>.gitignore</code> files.
                </p>
                <div className="docs-security-table-wrap">
                  <table className="docs-security-table">
                    <thead>
                      <tr>
                        <th>Source</th>
                        <th>Scope</th>
                        <th>Format / examples</th>
                      </tr>
                    </thead>
                    <tbody>
                      {IGNORE_RULE_SOURCES.map(([source, scope, format]) => (
                        <tr key={source}>
                          <td>{source}</td>
                          <td>{scope}</td>
                          <td>{format}</td>
                        </tr>
                      ))}
                    </tbody>
                  </table>
                </div>
              </article>

              <article className="docs-command">
                <h3>follow behavior</h3>
                <p>
                  <code>pufferfs sync --follow</code> uses the same matcher as
                  one-shot sync. Ignored directories are not watched, which
                  reduces filesystem noise from dependency installs, build
                  outputs, caches, and local scratch folders.
                </p>
              </article>
            </div>
          </section>

          <section id="commands" className="docs-section">
            <h2>commands</h2>
            <p>
              Commands are organized around roots: a local folder plus its
              committed server-side index. The transcripts below show what a
              user sees in the terminal. Sync examples use the actual CLI output
              format; connected examples assume a running PufferFS API.
            </p>
            <div className="docs-command-list">
              {COMMANDS.map((command) => (
                <article className="docs-command" key={command.name}>
                  <h3>{command.name}</h3>
                  <code>{command.usage}</code>
                  <p>{command.detail}</p>
                  <div className="docs-command-detail">
                    <strong>flags</strong>
                    <ul>
                      {command.flags.map((flag) => (
                        <li key={flag}>{flag}</li>
                      ))}
                    </ul>
                  </div>
                  <div className="docs-command-detail">
                    <strong>what you see</strong>
                    <pre>{command.transcript}</pre>
                  </div>
                  <p>{command.note}</p>
                </article>
              ))}
            </div>
          </section>

          <section id="apis" className="docs-section">
            <h2>APIs</h2>
            <p>
              The CLI uses the same HTTP API that agents and services can call
              directly. Normal routes accept{" "}
              <code>Authorization: Bearer pfs_...</code>. Platform admin routes
              use a separate server-configured admin key and are not available
              to regular user keys.
            </p>
            <p>
              Sync is asynchronous at the API layer when <code>async=true</code>{" "}
              is set. Query reads only the latest committed generation, so
              partially indexed data is not visible. Upload limits are 512 MiB
              per single file and 1024 MiB per bundle. The default sync job
              timeout is 30 minutes.
            </p>
            <div className="docs-openapi-list">
              {ENDPOINTS.map((endpoint) => (
                <article className="docs-openapi-operation" key={endpoint.operationId}>
                  <div className="docs-openapi-heading">
                    <code>{endpoint.method}</code>
                    <code>{endpoint.path}</code>
                  </div>
                  <h3>{endpoint.summary}</h3>
                  <dl className="docs-openapi-meta">
                    <div>
                      <dt>operationId</dt>
                      <dd>{endpoint.operationId}</dd>
                    </div>
                    <div>
                      <dt>auth</dt>
                      <dd>{endpoint.auth}</dd>
                    </div>
                  </dl>
                  <div className="docs-openapi-examples">
                    <section className="docs-example-block">
                      <div className="docs-example-heading">
                        <strong>Request body</strong>
                        {endpoint.requestBody === "No request body." ? null : (
                          <code>application/json</code>
                        )}
                      </div>
                      {endpoint.requestBody === "No request body." ? (
                        <p className="docs-empty-body">No request body.</p>
                      ) : (
                        <>
                          <div className="docs-field-list">
                            {endpoint.requestFields.map(
                              ([name, type, requirement, description]) => (
                                <div className="docs-field-row" key={name}>
                                  <div>
                                    <code>{name}</code>
                                    <span>{type}</span>
                                    <em>{requirement}</em>
                                  </div>
                                  <p>{description}</p>
                                </div>
                              ),
                            )}
                          </div>
                          <pre>{endpoint.requestBody}</pre>
                        </>
                      )}
                    </section>
                    <section className="docs-example-block">
                      <div className="docs-example-heading">
                        <strong>Responses</strong>
                        <code>application/json</code>
                      </div>
                      {endpoint.responses.map(([status, description, body]) => (
                        <div className="docs-response" key={status}>
                          <div className="docs-response-heading">
                            <code>{status}</code>
                            <span>{description}</span>
                          </div>
                          <pre>{body}</pre>
                        </div>
                      ))}
                    </section>
                  </div>
                </article>
              ))}
            </div>
          </section>

          <section id="security" className="docs-section">
            <h2>Security & Compliance</h2>
            <p>
              PufferFS is built to help teams make synced files searchable
              while keeping access controlled, auditable, and bounded to the
              data users choose to sync.
            </p>
            <div className="docs-security-list">
              {SECURITY_SECTIONS.map((section) => (
                <article key={section.title}>
                  <h3>{section.title}</h3>
                  {section.body.map((paragraph) => (
                    <p key={paragraph}>{paragraph}</p>
                  ))}
                </article>
              ))}
            </div>
            <div className="docs-security-subprocessors">
              <h3>Third-party services</h3>
              <p>
                PufferFS uses third-party providers for persistence and
                processing. Provider use depends on deployment configuration and
                customer use of the product.
              </p>
              <div className="docs-security-table-wrap">
                <table className="docs-security-table">
                  <thead>
                    <tr>
                      <th>Service area</th>
                      <th>Purpose</th>
                      <th>Data</th>
                    </tr>
                  </thead>
                  <tbody>
                    {SECURITY_SUBPROCESSORS.map((processor) => (
                      <tr key={processor.name}>
                        <td>{processor.name}</td>
                        <td>{processor.purpose}</td>
                        <td>{processor.data}</td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            </div>
          </section>
        </div>
      </div>
    </main>
  );
}
