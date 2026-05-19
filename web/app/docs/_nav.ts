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
        summary: "Peers, circles, message types, control surfaces, and session roadmap.",
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
        summary: "Messaging, peer identity, review queue, and scheduling tools.",
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
        summary: "setup, serve, peer, schedule, build-ui, telegram, and slack.",
      },
    ],
  },
];

export function flattenNav(): DocsNavItem[] {
  return docsNav.flatMap((section) => section.items);
}
