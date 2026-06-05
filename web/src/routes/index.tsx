import { createFileRoute, Link } from "@tanstack/react-router";
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
          <strong>pufferfs</strong>
        </div>
        <h1 className="wordmark">pufferfs</h1>
        <p className="tagline">
          A synced, queryable filesystem for your whole organization.
        </p>
        <Link to="/login" className="btn">
          get started
        </Link>
      </section>
    </main>
  );
}
