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
        slug: "install",
        href: "/docs/install",
        label: "Install",
        summary: "uv/pipx/pip, per-agent setup, transports, relay.",
      },
      {
        slug: "tools",
        href: "/docs/tools",
        label: "Tools",
        summary: "MCP tool reference: ask, ack, notify_peer, broadcast.",
      },
      {
        slug: "troubleshooting",
        href: "/docs/troubleshooting",
        label: "Troubleshooting",
        summary: "Hook failures, ghost peers, transport mismatch.",
      },
    ],
  },
  {
    label: "Compare",
    items: [
      {
        slug: "compare",
        href: "/docs/compare",
        label: "Compare",
        summary: "Repowire vs Happy, Memory Bank, cloud agents.",
      },
    ],
  },
];

export function flattenNav(): DocsNavItem[] {
  return docsNav.flatMap((section) => section.items);
}
