import { createFileRoute, Link } from "@tanstack/react-router";
import { PixelLogo } from "../components/PixelLogo";
import { ThemeToggle } from "../components/ThemeToggle";

export const Route = createFileRoute("/docs")({ component: Docs });

const guides = [
  ["Developer guide", "developer-guide.md"],
  ["HTTP API reference", "api-reference.md"],
  ["File formats and chunking", "file-ingestion-and-chunking.md"],
  ["Deployment roles", "architecture-and-functionality.md"],
  ["Configuration", "configuration.md"],
  ["Security and data handling", "security-and-data-handling.md"],
];

function Docs() {
  return (
    <div className="site-shell">
      <nav className="landing-nav" aria-label="primary">
        <Link to="/" className="nav-brand"><PixelLogo size={24} />pufferfs</Link>
        <div className="nav-actions"><Link to="/dashboard">console</Link><ThemeToggle /></div>
      </nav>
      <main className="container docs-content">
        <section className="docs-section">
          <p className="eyebrow">documentation</p>
          <h1>changing files, searchable versions</h1>
          <p>
            PufferFS captures local files into immutable source packs, extracts
            text, and publishes each file independently to a search index.
            Capture acceptance and indexing completion are separate.
          </p>
          <p>
            These docs describe the per-file release in the repository. An
            older hosted or self-hosted deployment needs a coordinated upgrade.
          </p>
        </section>
        <section id="install" className="docs-section">
          <h2>connect and sync</h2>
          <pre className="code-pane solo">{`curl -fsSL https://pufferfs.com/install.sh | sh
pufferfs init
pufferfs sync ./handbook --name handbook --dry-run
pufferfs sync ./handbook --name handbook
pufferfs sync status --root handbook
pufferfs sync wait --root handbook`}</pre>
          <p>
            The CLI needs a server URL and tenant API key.
            The API authorizes short-lived signed uploads for specific immutable
            objects. Sources remain retained after successful indexing.
          </p>
          <p>
            Select files with repeated --include globs; --exclude wins.
            Unselected files stay untouched. Use sync --follow or pufferfs
            service for continuous updates. Pending captures are journaled
            and resumed; --force preserves conflicting spools before recapture.
          </p>
        </section>
        <section id="search" className="docs-section">
          <h2>search and read</h2>
          <pre className="code-pane solo">{`pufferfs query "paid time off" --root handbook --mode hybrid
pufferfs read docs/policy.pdf --root handbook --pages 10:12
pufferfs read src/main.go --root repo --lines 200:400
pufferfs sync wait --root ./handbook --include 'docs/**'`}</pre>
          <p>
            Hybrid search combines keyword and vector ranking. FTS works for
            --no-vector roots. Reads and searches expose only published file
            extractions and enforce tenant, root and path permissions.
          </p>
        </section>
        <section id="formats" className="docs-section">
          <h2>file to chunks</h2>
          <p>
            Text, code and JSONL use bounded byte-preserving line chunks.
            Spreadsheets retain sheet/cell metadata. PDF, Word and presentations
            render temporary images and use Gemini 3.5 Flash-Lite Batch for
            Markdown extraction. Images use frame/page parsing.
          </p>
          <p>
            Audio and video use temporary audio clips and Gemini Batch
            transcription with best-effort speaker labels. Video visual frames
            are not indexed. Generated images and converted media are never
            stored in S3; original input files are retained.
          </p>
        </section>
        <section id="reference" className="docs-section">
          <h2>reference</h2>
          <p>
            The repository guides contain the full endpoint schemas, format
            list, deployment topology, retention contracts and test limitations.
            Root-generation uploads/jobs, NATS and the monolithic Modal app
            have been retired.
          </p>
          <ul>{guides.map(([label, path]) => (
            <li key={path}><a href={`https://github.com/suhjohn/pufferfs/blob/main/docs/${path}`}>{label}</a></li>
          ))}</ul>
        </section>
      </main>
    </div>
  );
}
