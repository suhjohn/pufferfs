import { createFileRoute, redirect } from "@tanstack/react-router";
import { useMutation, useQuery } from "@tanstack/react-query";
import { createCheckoutSession, fetchBilling } from "../../lib/queries";
import { BILLING_ENABLED } from "../../lib/config";

// Optional surface: when this deployment ships without payments
// (VITE_ENABLE_BILLING !== "true") the route is unreachable and bounces back to
// the dashboard, matching the hidden nav item.
export const Route = createFileRoute("/_app/billing")({
  beforeLoad: () => {
    if (!BILLING_ENABLED) {
      throw redirect({ to: "/dashboard" });
    }
  },
  component: BillingPage,
});

function BillingPage() {
  const { data: billing, isPending } = useQuery({
    queryKey: ["billing"],
    queryFn: fetchBilling,
  });

  const checkout = useMutation({
    mutationFn: createCheckoutSession,
    onSuccess: (url) => {
      window.location.href = url;
    },
  });

  return (
    <main className="console-page">
      <header className="console-header">
        <div>
          <p className="eyebrow">billing</p>
          <h1>plan</h1>
          <p className="muted">
            View subscription state and manage the current organization plan.
          </p>
        </div>
      </header>
      {isPending ? (
        <p className="muted">loading</p>
      ) : (
        <section className="console-section">
          <div className="section-heading">
            <div>
              <h2>subscription</h2>
              <p>Current billing state for this organization.</p>
            </div>
          </div>
          <div className="meta-row">
            <span className="muted">plan</span>
            <strong>{billing?.plan}</strong>
          </div>
          <div className="meta-row">
            <span className="muted">status</span>
            <strong>{billing?.status}</strong>
          </div>
          {billing?.currentPeriodEnd && (
            <div className="meta-row">
              <span className="muted">renews</span> {billing.currentPeriodEnd}
            </div>
          )}
        </section>
      )}
      <button
        className="btn"
        onClick={() => checkout.mutate()}
        disabled={checkout.isPending}
      >
        {billing?.status === "active"
          ? "manage subscription"
          : "upgrade to pro"}
      </button>
    </main>
  );
}
