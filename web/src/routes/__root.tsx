import {
  createRootRoute,
  HeadContent,
  Outlet,
  Scripts,
} from "@tanstack/react-router";
import { QueryClientProvider } from "@tanstack/react-query";
import { queryClient } from "../lib/queryClient";
import appCss from "../styles.css?url";

export const Route = createRootRoute({
  // Defined on the root so the prerendered SPA shell always carries the SEO
  // meta (the body renders client-side).
  head: () => ({
    meta: [
      { charSet: "utf-8" },
      { name: "viewport", content: "width=device-width, initial-scale=1" },
      { title: "PufferFS" },
      {
        name: "description",
        content:
          "PufferFS — hybrid search retrieval for your agents over a local filesystem or sandbox.",
      },
      { property: "og:title", content: "PufferFS" },
      {
        property: "og:description",
        content:
          "PufferFS — hybrid search retrieval for your agents over a local filesystem or sandbox.",
      },
    ],
    links: [
      { rel: "icon", type: "image/svg+xml", href: "/favicon.svg" },
      { rel: "stylesheet", href: appCss },
    ],
  }),
  component: RootComponent,
});

function RootComponent() {
  return (
    <html lang="en">
      <head>
        <script
          dangerouslySetInnerHTML={{
            __html:
              "try{var t=localStorage.getItem('pufferfs-theme');if(t==='light'||t==='dark')document.documentElement.dataset.theme=t}catch(e){}",
          }}
        />
        <HeadContent />
      </head>
      <body>
        <QueryClientProvider client={queryClient}>
          <Outlet />
        </QueryClientProvider>
        <Scripts />
      </body>
    </html>
  );
}
