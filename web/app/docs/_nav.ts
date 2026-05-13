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
];

export function flattenNav(): DocsNavItem[] {
  return docsNav.flatMap((section) => section.items);
}
