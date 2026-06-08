import { createFileRoute, Link } from "@tanstack/react-router";
import { PixelLogo } from "../components/PixelLogo";
import { ThemeToggle } from "../components/ThemeToggle";

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
          <span>pufferfs</span>
        </Link>
        <div className="nav-actions">
          <Link to="/login">console</Link>
          <Link to="/docs">docs</Link>
          <a href="https://github.com/suhjohn/pufferfs">github</a>
          <ThemeToggle />
        </div>
      </nav>

      <section className="hero">
        <div className="hero-copy">
          <div className="hero-mark" aria-hidden="true">
            <PixelLogo size={44} />
            <span>pufferfs</span>
          </div>
          <h1>pufferfs</h1>
          <p>
            Ship searchable files to your agents with hybrid retrieval by
            default.
          </p>
          <div className="hero-actions">
            <Link className="pill pill-primary" to="/docs">
              Docs
            </Link>
            <a className="pill pill-muted" href="https://github.com/suhjohn/pufferfs">
              GitHub
            </a>
          </div>
        </div>

        <div className="landing-rule" />
      </section>

      <div className="landing-layout">
        <aside className="landing-toc" aria-label="page navigation">
          <a href="#about">about</a>
          <a href="#cli-setup">install + usage</a>
          <a href="#sandboxes">use this for</a>
          <a href="#product">workflow</a>
        </aside>

        <div className="landing-content">
          <section className="about-section" id="about">
            <h2>about</h2>
            <p>This is a CLI tool that lets you:</p>
            <ol>
              <li>
                Sync local folders into a hosted hybrid vector and full-text
                index your agents can query.
              </li>
              <li>
                Keep roots fresh with a foreground follow process or an
                OS-managed background service.
              </li>
            </ol>
          </section>

          <section className="cli-setup" id="cli-setup" aria-label="CLI setup">
            <h2>install + usage</h2>
            <div className="code-window cli-setup-window">
              <pre className="code-pane solo">{`# install (macOS + Linux)
curl -fsSL https://pufferfs.com/install.sh | sh

# connect your account
pufferfs init

# inspect before uploading
pufferfs sync ./handbook --name handbook --dry-run`}</pre>
            </div>
            <div className="code-window cli-setup-window">
              <pre className="code-pane solo">{`# sync a directory and wait for commit
pufferfs sync ./handbook --name handbook

# query the latest committed generation
pufferfs query "paid time off" --root handbook --top-k 2

# keep it current with a supervised background service
pufferfs service install ./handbook --name handbook && pufferfs service start handbook`}</pre>
            </div>
          </section>

          <section className="product-stage" id="product" aria-label="PufferFS terminal workflow">
            <p className="workflow-caption">
              sync once, then let the agent query the workspace
            </p>
            <div className="code-window workflow-window">
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
          </section>

          <section
            className="use-cases"
            id="sandboxes"
            aria-label="built for any compute environment"
          >
            <h2>use this for</h2>
            <div className="use-case-grid">
              {SANDBOX_POINTS.map((point) => (
                <div className="use-case" key={point.title}>
                  <h3>{point.title}</h3>
                  <p>{point.body}</p>
                </div>
              ))}
            </div>
            <div className="access-block" aria-label="organization and user access">
              <h2>platform</h2>
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
        </div>
      </div>
    </main>
  );
}
