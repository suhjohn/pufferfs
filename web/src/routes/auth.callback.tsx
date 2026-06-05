import { createFileRoute, redirect } from "@tanstack/react-router";

// The backend OAuth callback has already set the auth cookie and redirected
// the browser here. Nothing to do but forward into the app.
export const Route = createFileRoute("/auth/callback")({
  beforeLoad: () => {
    throw redirect({ to: "/dashboard" });
  },
});
