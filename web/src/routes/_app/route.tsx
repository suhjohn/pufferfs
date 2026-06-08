import {
  createFileRoute,
  Link,
  Outlet,
  redirect,
  useRouter,
} from "@tanstack/react-router";
import { fetchSession, logout } from "../../lib/auth";
import { BILLING_ENABLED } from "../../lib/config";
import { PixelLogo } from "../../components/PixelLogo";
import { ThemeToggle } from "../../components/ThemeToggle";

// Pathless layout for every authenticated page. `beforeLoad` runs on the
// client (these routes are not prerendered) and gates access on the session
// cookie, redirecting to /login on 401.
export const Route = createFileRoute("/_app")({
  beforeLoad: async () => {
    try {
      return { session: await fetchSession() };
    } catch {
      throw redirect({ to: "/login" });
    }
  },
  component: AppLayout,
});

function AppLayout() {
  const { session } = Route.useRouteContext();
  const router = useRouter();

  async function handleLogout() {
    await logout();
    router.navigate({ to: "/login" });
  }

  return (
    <>
      <nav className="nav">
        <span className="brand">
          <PixelLogo size={18} />
          pufferfs
        </span>
        <Link to="/dashboard">dashboard</Link>
        <Link to="/organization">organization</Link>
        {BILLING_ENABLED && <Link to="/billing">billing</Link>}
        <span className="spacer" />
        <span className="muted">{session.email}</span>
        <ThemeToggle />
        <button className="btn btn-sm" onClick={handleLogout}>
          logout
        </button>
      </nav>
      <Outlet />
    </>
  );
}
