import type { Metadata } from "next";
import type { ReactNode } from "react";

export const metadata: Metadata = {
  title: "Design system · Repowire",
  robots: { index: false, follow: false },
};

export default function DesignLayout({ children }: { children: ReactNode }) {
  return children;
}
