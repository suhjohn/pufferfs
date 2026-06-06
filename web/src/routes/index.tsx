import { createFileRoute, Link } from "@tanstack/react-router";
import { PixelLogo } from "../components/PixelLogo";

// Landing page. SEO meta lives on the root route (so the SPA shell carries it);
// this route just renders the marketing body.
export const Route = createFileRoute("/")({
  component: Landing,
});

// Why PufferFS fits ephemeral agent environments: a tiny client that pushes the
// heavy work off the machine.
const SANDBOX_POINTS = [
  {
    title: "~7 mb binary",
    body: "A single static binary with no runtime or dependencies. Drop it into any VM, container, or CI box.",
  },
  {
    title: "remote indexing",
    body: "Hashing and diffing run locally; chunking, embeddings, and the search index run on the server. The heavy compute never touches your machine.",
  },
  {
    title: "sandbox-ready",
    body: "Light enough for ephemeral agent sandboxes where you can't install a full stack — sync, search, then throw the box away.",
  },
];

const ACCESS_POINTS = [
  {
    title: "shared org roots",
    body: "Make canonical team folders searchable for everyone in the organization, with roles controlling who can update or delete them.",
  },
  {
    title: "private user roots",
    body: "Give each user or agent its own indexed workspace inside the same tenant, visible only to that owner and org admins.",
  },
  {
    title: "scoped agent keys",
    body: "Issue least-privilege keys for compute jobs: query-only, sync-only, or broader access when a trusted workflow needs it.",
  },
];

function Landing() {
  return (
    <main className="site-shell">
      <nav className="landing-nav" aria-label="primary">
        <Link to="/" className="nav-brand">
          <PixelLogo size={24} />
          <span>pufferfs</span>
        </Link>
        <div className="nav-actions">
          <Link className="pill pill-muted" to="/docs">
            Docs
          </Link>
          <Link className="pill pill-muted" to="/login">
            Sign in
          </Link>
        </div>
      </nav>

      <section className="hero">
        <div className="hero-copy">
          <h1>point your agents at a folder, not a vector pipeline</h1>
          <p>
            sync once and pufferfs keeps a hybrid vector + full-text index fresh
            as your files change — no chunking, embeddings, or infra to wire up.
          </p>
          <div className="hero-actions">
            <Link className="pill pill-primary" to="/login">
              Create account
            </Link>
            <a className="pill pill-muted" href="#cli-setup">
              View CLI setup
            </a>
          </div>
        </div>

        <section className="product-stage" id="product" aria-label="PufferFS terminal workflow">
          <div className="code-graphic">
            <p className="workflow-caption">
              sync once, let the agent query the workspace
            </p>
            <div className="code-window workflow-window">
              <div className="code-titlebar">
                <span>terminal</span>
              </div>
              <div className="code-grid workflow-grid">
                <pre className="code-pane">{`$ cd ~/Documents/handbook
$ pufferfs sync . --name handbook
Created root: handbook (root_8z7m)
Building Merkle tree for /Users/me/Documents/handbook...
Merkle diff found 1,284 changed files
Sync job sync_2bd3 started; polling until committed...
Sync status: indexing (912/1,284 files)
Sync complete: 1,284 files processed, 14,602 chunks added`}</pre>
                <pre className="code-pane">{`$ pufferfs query "how much paid time off do we get" --root handbook --top-k 2
1.
score: 0.8124
file: /Users/me/Documents/handbook/policies/time-off.pdf
page: 3
content: Full-time employees accrue 20 days of paid time off per year, plus
10 company holidays. Unused PTO rolls over up to a 5-day cap.

2.
score: 0.7741
file: /Users/me/Documents/handbook/onboarding/benefits.docx
content: Parental leave provides 12 weeks paid for the primary caregiver and
6 weeks for secondary caregivers.`}</pre>
              </div>
            </div>

            <p className="workflow-caption">
              keep it fresh with a background service
            </p>
            <div className="code-window workflow-window">
              <div className="code-titlebar">
                <span>terminal</span>
              </div>
              <pre className="code-pane solo">{`$ pufferfs service install ~/Documents/handbook --name handbook
Installed launchd service: com.pufferfs.handbook
$ pufferfs service start handbook
Service handbook started — re-syncing on every change

# edit files as usual; the index stays current for your agents
$ pufferfs service logs handbook
watching /Users/me/Documents/handbook (debounce 2s)
change: policies/time-off.pdf (modified)    → 11 chunks updated
change: contracts/acme-msa.pdf (added)      → 24 chunks added
change: notes/standup-2026-06-05.md (added) → 3 chunks added`}</pre>
            </div>
          </div>
        </section>
      </section>

      <section
        className="use-cases"
        id="sandboxes"
        aria-label="built for any compute environment"
      >
        <h2>built for any compute environment</h2>
        <div className="use-case-grid">
          {SANDBOX_POINTS.map((point) => (
            <div className="use-case" key={point.title}>
              <h3>{point.title}</h3>
              <p>{point.body}</p>
            </div>
          ))}
        </div>
        <div className="access-block" aria-label="organization and user access">
          <h2>shared where it should be, private where it matters</h2>
          <div className="use-case-grid">
            {ACCESS_POINTS.map((point) => (
              <div className="use-case" key={point.title}>
                <h3>{point.title}</h3>
                <p>{point.body}</p>
              </div>
            ))}
          </div>
        </div>
      </section>

      <section className="cli-setup" id="cli-setup" aria-label="CLI setup">
        <h2>CLI setup connects your account</h2>
        <p className="muted">
          Install the binary, run init, and sign in in your browser. PufferFS
          issues a scoped CLI key and writes the local config automatically.
        </p>
        <div className="code-window cli-setup-window">
          <div className="code-titlebar">
            <span>terminal — macOS</span>
          </div>
          <pre className="code-pane solo">{`$ brew install --cask suhjohn/tap/pufferfs
$ pufferfs init
Opening browser to connect your PufferFS account...
Config written to ~/.tpfs/config.toml
PufferFS CLI connected.`}</pre>
        </div>
        <div className="code-window cli-setup-window">
          <div className="code-titlebar">
            <span>terminal — Linux / CI</span>
          </div>
          <pre className="code-pane solo">{`$ curl -fsSL https://pufferfs.com/install.sh | sh
$ pufferfs init
Opening browser to connect your PufferFS account...
Config written to ~/.tpfs/config.toml
PufferFS CLI connected.`}</pre>
        </div>
      </section>

      <section className="closing-cta" aria-label="get started">
        <h2>drop it into your next compute environment</h2>
        <p className="muted">
          install one binary, connect once, and your agents can search the box.
        </p>
        <div className="hero-actions">
          <Link className="pill pill-primary" to="/login">
            Create account
          </Link>
          <Link className="pill pill-muted" to="/docs">
            Read docs
          </Link>
          <a className="pill pill-muted" href="#cli-setup">
            View CLI setup
          </a>
        </div>
      </section>
    </main>
  );
}
