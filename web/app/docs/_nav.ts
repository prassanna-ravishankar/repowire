export type DocsNavItem = {
  slug: string;
  href: string;
  label: string;
  summary: string;
};

export type DocsNavSection = {
  label: string;
  items: DocsNavItem[];
};

export const docsNav: DocsNavSection[] = [
  {
    label: "Start",
    items: [
      {
        slug: "quickstart",
        href: "/docs/quickstart",
        label: "Quickstart",
        summary: "Install, set up, and route your first ask across two repos.",
      },
      {
        slug: "concepts",
        href: "/docs/concepts",
        label: "Concepts",
        summary: "Peers, circles, ask/notify/broadcast, control surfaces.",
      },
    ],
  },
  {
    label: "Reference",
    items: [
      {
        slug: "reference/tools",
        href: "/docs/reference/tools",
        label: "MCP tools",
        summary: "ask, ack, notify_peer, broadcast, list_peers, spawn_peer, kill_peer.",
      },
      {
        slug: "reference/client",
        href: "/docs/reference/client",
        label: "Python client",
        summary: "AsyncRepowireClient: typed async surface over the daemon API.",
      },
      {
        slug: "reference/cli",
        href: "/docs/reference/cli",
        label: "CLI",
        summary: "repowire setup, serve, build-ui, telegram start, slack start.",
      },
    ],
  },
];

export function flattenNav(): DocsNavItem[] {
  return docsNav.flatMap((section) => section.items);
}
