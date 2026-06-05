import { createFileRoute, Link } from "@tanstack/react-router";
import { BILLING_ENABLED } from "../lib/config";
import { PixelLogo } from "../components/PixelLogo";

// Landing page. SEO meta lives on the root route (so the SPA shell carries it);
// this route just renders the marketing body.
export const Route = createFileRoute("/")({
  component: Landing,
});

function Landing() {
  return (
    <main className="container">
      <section className="hero">
        <div className="brandmark">
          <PixelLogo size={48} />
          <span className="tag">v1</span>
        </div>
        <h1 className="wordmark cursor">pufferfs</h1>
        <p className="tagline">
          A synced, queryable filesystem for your whole organization.
        </p>
        <Link to="/login" className="btn">
          &gt; get started
        </Link>
      </section>

      <div className="grid">
        <section className="card">
          <h2>sync everything</h2>
          <p className="muted">Push your files once; query them from anywhere.</p>
        </section>
        <section className="card">
          <h2>org-aware</h2>
          <p className="muted">Invite your team, manage members and API keys.</p>
        </section>
        {BILLING_ENABLED && (
          <section className="card">
            <h2>simple pricing</h2>
            <p className="muted">Start free, upgrade when you grow.</p>
          </section>
        )}
      </div>
    </main>
  );
}
