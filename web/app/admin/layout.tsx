import type { Metadata } from "next";

export const metadata: Metadata = {
  title: "Repowire Managed Admin",
  description: "Sandboxes, owners, infrastructure cost and connections for Repowire Managed.",
  robots: { index: false },
};

export default function AdminLayout({ children }: { children: React.ReactNode }) {
  return children;
}
