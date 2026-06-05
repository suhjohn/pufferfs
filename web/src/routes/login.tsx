import { createFileRoute } from "@tanstack/react-router";
import { API_URL } from "../lib/config";
import { PixelLogo } from "../components/PixelLogo";

export const Route = createFileRoute("/login")({
  component: Login,
});

function Login() {
  // The Go API owns the OAuth flow: this link kicks off /auth/google, which
  // redirects to Google and, on the callback, sets an httpOnly cookie and
  // bounces the browser to /auth/callback here.
  return (
    <main className="container">
      <div className="card" style={{ maxWidth: 440 }}>
        <div className="term-bar">
          <span className="dot" />
          <span className="dot" />
          <span className="dot" />
          <span className="title">auth.sh</span>
        </div>
        <div className="brandmark" style={{ marginBottom: "0.75rem" }}>
          <PixelLogo size={28} />
          <strong>pufferfs</strong>
        </div>
        <h1 className="prompt">login</h1>
        <p className="muted">Authenticate to access your workspace.</p>
        <a className="btn" href={`${API_URL}/auth/google`}>
          &gt; continue with google
        </a>
      </div>
    </main>
  );
}
