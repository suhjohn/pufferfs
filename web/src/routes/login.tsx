import { createFileRoute } from "@tanstack/react-router";
import { API_URL } from "../lib/config";
import { capture } from "../lib/analytics";
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
      <div className="card login-card">
        <div className="login-brand">
          <PixelLogo size={28} />
          <strong>pufferfs</strong>
        </div>
        <h1>login</h1>
        <p className="muted">Use Google to continue.</p>
        <a
          className="btn"
          href={`${API_URL}/auth/google`}
          onClick={() => capture("login_started", { provider: "google" })}
        >
          continue with google
        </a>
      </div>
    </main>
  );
}
