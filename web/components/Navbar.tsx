import Link from "next/link";
import Image from "next/image";
import { Github, LogIn } from "lucide-react";

const DASHBOARD_URL = "https://relay.repowire.io/dashboard";

export default function Navbar() {
  return (
    <nav className="fixed top-0 z-50 w-full border-b border-border-faint mesh-panel pt-[env(safe-area-inset-top)]">
      <div className="mx-auto flex h-14 max-w-7xl items-center justify-between px-4 sm:px-6 lg:px-8">
        <Link href="/" className="flex items-center gap-3">
          <Image src="/brand/logo-mark-copper.svg" alt="" width={24} height={26} priority />
          <span className="font-headline text-sm font-bold uppercase tracking-[0.2em] text-on-surface">
            REPOWIRE
          </span>
        </Link>
        <div className="flex items-center gap-5">
          <Link
            href="/docs/quickstart"
            className="font-mono text-[10px] font-semibold uppercase tracking-[0.16em] text-outline transition-colors hover:text-primary-fixed"
          >
            Docs
          </Link>
          <Link
            href="https://github.com/prassanna-ravishankar/repowire"
            target="_blank"
            rel="noopener noreferrer"
            className="text-outline transition-colors hover:text-on-surface"
          >
            <span className="sr-only">GitHub</span>
            <Github className="h-5 w-5" />
          </Link>
          <Link
            href={DASHBOARD_URL}
            className="inline-flex items-center gap-1.5 rounded border border-primary/40 bg-primary/10 px-3 py-1.5 font-mono text-[10px] font-bold uppercase tracking-[0.16em] text-primary-fixed transition-[filter,transform,background] hover:bg-primary/20 hover:brightness-110 active:scale-[0.98]"
          >
            <LogIn className="h-3 w-3" />
            <span className="hidden sm:inline">Sign in</span>
            <span className="sm:hidden">Dash</span>
          </Link>
        </div>
      </div>
    </nav>
  );
}
