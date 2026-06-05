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
    <main className="container">
      <h1 className="prompt">billing</h1>
      {isPending ? (
        <p className="muted">// loading…</p>
      ) : (
        <div className="card">
          <div className="row">
            <span className="muted">plan</span>{" "}
            <strong>{billing?.plan}</strong>
          </div>
          <div className="row">
            <span className="muted">status</span>{" "}
            <strong>{billing?.status}</strong>
          </div>
          {billing?.currentPeriodEnd && (
            <div className="row">
              <span className="muted">renews</span> {billing.currentPeriodEnd}
            </div>
          )}
        </div>
      )}
      <button
        className="btn"
        onClick={() => checkout.mutate()}
        disabled={checkout.isPending}
      >
        {billing?.status === "active"
          ? "> manage subscription"
          : "> upgrade to pro"}
      </button>
    </main>
  );
}
