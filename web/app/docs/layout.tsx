import Navbar from "@/components/Navbar";
import { docsNav } from "./_nav";
import DocsSidebar from "./DocsSidebar";

export const metadata = {
  title: "Docs · Repowire",
  description: "Repowire documentation: install, concepts, tools, comparisons.",
};

export default function DocsLayout({ children }: { children: React.ReactNode }) {
  return (
    <div className="min-h-screen bg-surface text-on-surface selection:bg-primary/30 mesh-bg">
      <Navbar />

      <div className="mx-auto flex max-w-7xl flex-col gap-8 px-4 pb-24 pt-20 sm:px-6 lg:flex-row lg:gap-10 lg:px-8 lg:pt-28">
        <DocsSidebar sections={docsNav} />
        <main className="min-w-0 flex-1">{children}</main>
      </div>
    </div>
  );
}
