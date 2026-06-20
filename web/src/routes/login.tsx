import { createFileRoute } from "@tanstack/react-router";
import { useEffect, useState } from "react";
import type { FormEvent } from "react";
import { API_URL } from "../lib/config";
import { capture } from "../lib/analytics";
import { PixelLogo } from "../components/PixelLogo";
import { fetchAuthProviders, startEmailLogin, verifyEmailLogin } from "../lib/auth";

export const Route = createFileRoute("/login")({
  component: Login,
});

function Login() {
  const [email, setEmail] = useState("");
  const [code, setCode] = useState("");
  const [challengeId, setChallengeId] = useState("");
  const [expiresIn, setExpiresIn] = useState(0);
  const [resendAfter, setResendAfter] = useState(0);
  const [resendRemaining, setResendRemaining] = useState(0);
  const [isSending, setIsSending] = useState(false);
  const [isVerifying, setIsVerifying] = useState(false);
  const [error, setError] = useState("");
  const [providers, setProviders] = useState({ email_code: true, google: false });

  useEffect(() => {
    let cancelled = false;
    fetchAuthProviders()
      .then((next) => {
        if (!cancelled) setProviders(next);
      })
      .catch(() => undefined);
    return () => {
      cancelled = true;
    };
  }, []);

  useEffect(() => {
    if (resendRemaining <= 0) return;
    const timer = window.setInterval(() => {
      setResendRemaining((value) => Math.max(0, value - 1));
    }, 1000);
    return () => window.clearInterval(timer);
  }, [resendRemaining]);

  async function requestCode(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    await requestCodeForEmail();
  }

  async function requestCodeForEmail() {
    setError("");
    setIsSending(true);
    try {
      const resp = await startEmailLogin({ email });
      setChallengeId(resp.challenge_id);
      setExpiresIn(resp.expires_in);
      setResendAfter(resp.resend_after);
      setResendRemaining(resp.resend_after);
      setCode("");
      capture("login_started", { provider: "email_code" });
    } catch (err) {
      setError(errorMessage(err, "Could not send a login code."));
    } finally {
      setIsSending(false);
    }
  }

  async function verifyCode(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    setError("");
    setIsVerifying(true);
    try {
      await verifyEmailLogin({ challenge_id: challengeId, code });
      capture("login_completed", { provider: "email_code" });
      window.location.assign("/dashboard");
    } catch (err) {
      setError(errorMessage(err, "Code invalid or expired."));
    } finally {
      setIsVerifying(false);
    }
  }

  const waitingForCode = challengeId !== "";

  return (
    <main className="container">
      <div className="card login-card">
        <div className="login-brand">
          <PixelLogo size={28} />
          <strong>pufferfs</strong>
        </div>
        <h1>login</h1>
        {providers.email_code && !waitingForCode ? (
          <form className="login-form" onSubmit={requestCode}>
            <label>
              <span>email</span>
              <input
                type="email"
                autoComplete="email"
                value={email}
                onChange={(event) => setEmail(event.target.value)}
                required
              />
            </label>
            <button className="btn" type="submit" disabled={isSending}>
              {isSending ? "sending" : "send code"}
            </button>
          </form>
        ) : providers.email_code ? (
          <form className="login-form" onSubmit={verifyCode}>
            <label>
              <span>code</span>
              <input
                inputMode="numeric"
                autoComplete="one-time-code"
                value={code}
                onChange={(event) => setCode(event.target.value)}
                minLength={6}
                required
              />
            </label>
            <button className="btn" type="submit" disabled={isVerifying}>
              {isVerifying ? "checking" : "continue"}
            </button>
            <button
              className="btn btn-secondary"
              type="button"
              disabled={isSending || resendRemaining > 0}
              onClick={requestCodeForEmail}
            >
              {resendRemaining > 0 ? `resend in ${resendRemaining}s` : "resend code"}
            </button>
            <p className="muted">
              sent to {email}
              {expiresIn > 0 ? `, expires in ${Math.round(expiresIn / 60)} min` : ""}
              {resendAfter > 0 ? `, resend after ${resendAfter}s` : ""}
            </p>
          </form>
        ) : (
          <p className="muted">Email login is not available on this deployment.</p>
        )}
        {error && <p className="muted login-error">{error}</p>}
        {providers.google && (
          <>
            {providers.email_code && (
              <div className="login-divider">
                <span />
                <p>or</p>
                <span />
              </div>
            )}
            <a
              className="btn btn-secondary"
              href={`${API_URL}/auth/google`}
              onClick={() => capture("login_started", { provider: "google" })}
            >
              continue with google
            </a>
          </>
        )}
      </div>
    </main>
  );
}

function errorMessage(err: unknown, fallback: string) {
  if (err instanceof Error && err.message) return err.message;
  return fallback;
}
