import type { Metadata } from "next";

export const metadata: Metadata = {
  title: "Repowire Managed",
  description: "Your managed Repowire agents, their runs and workspaces, and the clients connected to your mesh.",
  robots: { index: false },
};

export default function ManagedLayout({ children }: { children: React.ReactNode }) {
  return children;
}
