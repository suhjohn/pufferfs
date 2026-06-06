import { createFileRoute, Link } from "@tanstack/react-router";
import { PixelLogo } from "../components/PixelLogo";

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
    usage: "pufferfs root delete handbook --yes",
    detail:
      "Deletes PufferFS metadata, index rows, storage artifacts, and local PufferFS cache for a root. It does not delete source files on disk.",
    flags: ["--yes: skip confirmation prompt"],
    transcript: `$ pufferfs root delete handbook --yes
Deleted root handbook (root_8z7m)
Deleted Turbopuffer namespace: org_acme_root_8z7m
Deleted 1,246 storage objects`,
    note: "Deletion can return 409 if a sync is active. Stop background services before deleting a root.",
  },
  {
    name: "upgrade",
    usage: "pufferfs upgrade",
    detail:
      "Upgrades direct CLI installs from the server release manifest. Homebrew-managed installs should use brew upgrade.",
    flags: [
      "--manifest-url <url>: use a custom release manifest",
      "--version <version>: install a specific version",
      "--restart-services: restart installed services after upgrade",
      "--force: upgrade even if the install appears Homebrew-managed",
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
  "changes": [
    {
      "path": "policies/time-off.pdf",
      "status": "MODIFIED",
      "content_hash": "sha256:...",
      "size": 18422,
      "source_key": "files/root_8z7m/policies/time-off.pdf"
    }
  ],
  "state_ref": "bundles/root_8z7m/state.gz",
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
      ["changes", "array", "required", "Files added, modified, or deleted in this sync. Each item names a path plus content metadata or deletion status."],
      ["changes[].path", "string", "required", "Path relative to the root directory."],
      ["changes[].status", "string", "required", "File state such as ADDED, MODIFIED, or DELETED."],
      ["changes[].content_hash", "string", "required for uploaded content", "SHA-256 content identity used for diffing, proof, and dedupe."],
      ["changes[].size", "number", "required for uploaded content", "File size in bytes. Single-file uploads are limited to 512 MiB."],
      ["changes[].source_key", "string", "required for uploaded content", "Object-storage key where the uploaded file bytes can be read by the server pipeline."],
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

const SECURITY_ITEMS = [
  {
    title: "data boundary and storage",
    body: "PufferFS is not local-only search. Sync uploads source bytes and derived state to object storage, stores org/root/job metadata in Postgres, writes searchable content and vectors to Turbopuffer, and can send documents through Modal for extraction and embeddings. The local folder remains the source of truth, but synced content leaves the machine.",
  },
  {
    title: "data residency",
    body: "Residency is currently a deployment and provider configuration, not a per-customer product control. The bundled AWS deployment creates regional buckets and infrastructure in the configured AWS region, while Turbopuffer and Modal residency depend on their account and region configuration. PufferFS should only publish named supported regions after production deployment policy is fixed.",
  },
  {
    title: "encryption",
    body: "The bundled AWS deployment enables S3 server-side encryption with AES-256 for artifacts and encrypted EFS storage for NATS persistence. CloudFront redirects the web app to HTTPS, and the API can serve HTTPS behind an ALB with a TLS 1.2/1.3 policy when a validated certificate is configured. Encryption for Postgres, Turbopuffer, Modal, logs, and backups is provider/deployment dependent unless separately configured.",
  },
  {
    title: "authentication and sessions",
    body: "Tenant API keys are generated as pfs_ values and stored only as SHA-256 hashes. The raw key is returned once. Browser sessions use an HS256 JWT in an httpOnly pf_session cookie with SameSite=Lax. OAuth callbacks require signed state bound to a short-lived httpOnly state cookie. CLI browser login redirects the issued key only to a loopback callback. The platform admin key is a separate server-side credential for /admin/* and is compared by hash in constant time.",
  },
  {
    title: "tenant and root access",
    body: "Every normal API request resolves to an org, user, role, and optional scope list. Root lookups are org-scoped. Org roots are readable by org members, writable by editor+, and deletable by admin+. User roots are readable and writable by their owner or org admins. Unreadable roots return 404 rather than revealing that the root exists.",
  },
  {
    title: "scopes and least privilege",
    body: "New user-created API keys must include an explicit non-empty scope list. Scoped API keys must include the required action, an accepted alias, or *. The dashboard-created CLI key currently requests sync, query, and root:delete; query-only automation keys can be created with [\"query\"]. Legacy empty-scope keys are still treated as unrestricted for compatibility and should be rotated.",
  },
  {
    title: "customer data access",
    body: "The application code enforces tenant and root access before query and sync operations. There is not yet a formal staff-access workflow, customer approval gate, just-in-time privilege system, or customer-visible support access log. Treat direct access to Postgres, object storage, Turbopuffer, Modal outputs, and server logs as privileged operational access that must be controlled by the deployment operator.",
  },
  {
    title: "query isolation",
    body: "Queries are constrained to the root's visible committed generation. If the server cannot resolve that generation, it fails closed instead of returning unfiltered rows. In-flight or failed syncs are not exposed to normal queries. Deny-prefix ACLs are filtered out of results, and user-scoped roots add content-proof filtering for non-admin callers.",
  },
  {
    title: "secret-file handling",
    body: "Before building sync state, the CLI excludes common secret filenames: .env, .env.*, private keys, credentials.json, service-account*.json, .npmrc, .pypirc, .p12, and .pfx. It also honors .gitignore, .tpfsignore, and ~/.tpfs/ignore. This is filename-based protection, not a content scanner or DLP system.",
  },
  {
    title: "third-party processing",
    body: "Synced content can be handled by infrastructure and model providers used by the deployment: object storage, Postgres, Turbopuffer, Modal, embedding/OCR models, Stripe for billing events, and Google OAuth for identity. PufferFS should publish a formal subprocessors table only when the production vendor list, data categories, locations, and notification process are committed.",
  },
  {
    title: "deletion and retention",
    body: "Root deletion removes PufferFS metadata, Turbopuffer namespaces, object-storage artifacts under files/, bundles/, states/, chunks/, and syncs/, plus local PufferFS cache. It does not delete source files from the user's machine. Active sync jobs block root deletion with 409.",
  },
  {
    title: "vulnerability disclosure",
    body: "Report security issues to security@pufferfs.com. Good-faith testing is in scope when it avoids data destruction, service disruption, spam, social engineering, and access to other users' data. Include affected routes, reproduction steps, impact, and any logs or request IDs that help reproduce the issue.",
  },
  {
    title: "compliance status",
    body: "PufferFS should not currently claim SOC 2, ISO 27001, HIPAA, GDPR compliance, CCPA compliance, DPAs, BAAs, customer-managed encryption keys, private networking, audit logs, SAML/SSO enforcement, MFA enforcement, malware scanning, or content-level secret detection. Any customer-facing claim should be backed by an implemented control, policy, contract, or report.",
  },
  {
    title: "shared responsibility",
    body: "PufferFS enforces app-level auth, root isolation, API scopes, committed-generation query filtering, deny-prefix ACLs, filename-based secret exclusions, and root deletion. Operators remain responsible for HTTPS, Secure cookies, secrets management, CORS allowlists, provider encryption, backups, network isolation, key rotation, staff access, logging, incident response, and compliance evidence.",
  },
];

const SECURITY_TOBES = [
  "Add audit logs for login, API key creation/deletion, root creation/deletion, ACL changes, sync submissions, query access, and admin actions.",
  "Define retention periods for source copies, extracted chunks, sync artifacts, logs, deleted roots, and billing/customer records.",
  "Publish a formal subprocessors table with vendor, purpose, data category, location, and update-notification policy.",
  "Add a production backup and restore policy with recovery targets, restore testing, and customer deletion semantics.",
  "Add SAML/SSO and MFA enforcement for organizations that need centralized identity policy.",
  "Add customer-visible staff access controls: approval, time bounds, reason codes, and immutable logging.",
  "Add private networking options for enterprise deployments, such as VPC peering, PrivateLink, or BYOC.",
  "Add customer-managed encryption key support if enterprise customers require key control.",
  "Complete SOC 2 readiness before claiming SOC 2; publish report availability only after the audit is complete.",
  "Create DPA/GDPR/CCPA paperwork and a BAA path only after the data flows, subprocessors, and operational controls support those commitments.",
  "Add content-level secret detection or DLP integrations if PufferFS will index high-risk repositories or business documents by default.",
];

function Docs() {
  return (
    <main className="docs-shell">
      <nav className="landing-nav" aria-label="primary">
        <Link to="/" className="nav-brand">
          <PixelLogo size={24} />
          <span>pufferfs</span>
        </Link>
        <div className="nav-actions">
          <Link className="pill pill-muted" to="/">
            Home
          </Link>
          <Link className="pill pill-muted" to="/login">
            Sign in
          </Link>
        </div>
      </nav>

      <section className="docs-hero">
        <h1>docs</h1>
        <p className="muted">
          Setup, CLI commands, API reference, and security behavior for PufferFS.
        </p>
      </section>

      <div className="docs-layout">
        <aside className="docs-toc" aria-label="docs navigation">
          <a href="#setup">setup</a>
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
            <div className="code-window">
              <div className="code-titlebar">
                <span>terminal</span>
              </div>
              <pre className="code-pane solo">{`brew install --cask suhjohn/tap/pufferfs
pufferfs init

# non-interactive setup for CI or compute jobs
pufferfs init --api-key pfs_...

# one-off environment override
PUFFERFS_API_KEY=pfs_... pufferfs sync . --name workspace`}</pre>
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
            <h2>security</h2>
            <p>
              This section describes controls that exist in the current code,
              not a generic trust checklist. The model is closest to a
              developer infrastructure product: keep credentials scoped, treat
              the indexing plane as sensitive, and make access decisions at the
              API before anything is queried.
            </p>
            <div className="docs-security-list">
              {SECURITY_ITEMS.map((item) => (
                <article key={item.title}>
                  <h3>{item.title}</h3>
                  <p>{item.body}</p>
                </article>
              ))}
            </div>
            <div className="docs-security-roadmap">
              <h3>TO BE controls</h3>
              <p>
                These are the next security and compliance commitments PufferFS
                should implement before making enterprise-grade claims.
              </p>
              <ul>
                {SECURITY_TOBES.map((item) => (
                  <li key={item}>{item}</li>
                ))}
              </ul>
            </div>
          </section>
        </div>
      </div>
    </main>
  );
}
