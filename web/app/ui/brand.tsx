"use client";

import Link from "next/link";
import { useSyncExternalStore } from "react";
import { Moon, Sun } from "lucide-react";
import { cn } from "./cn";
import { IconButton } from "./controls";

/** The wordmark in a product top bar: the mark, REPOWIRE, and the surface's name. */
export function Brand({ suffix, href = "/" }: { suffix: string; href?: string }) {
  return (
    <Link href={href} className="flex min-h-11 shrink-0 items-center gap-2.5 md:min-h-0">
      {/* eslint-disable-next-line @next/next/no-img-element */}
      <img src="/brand/logo-mark.svg" alt="" width={20} height={22} />
      <span className="font-headline text-xs font-bold tracking-[0.16em] text-fg">REPOWIRE</span>
      <span className="font-mono text-[10px] font-semibold tracking-[0.18em] text-fg-subtle">{suffix}</span>
    </Link>
  );
}

/** A mono id that copies itself on click. */
export function CopyId({ value, display, className, onCopied }: { value: string; display?: string; className?: string; onCopied?: () => void }) {
  return (
    <button
      type="button"
      title={`Copy ${value}`}
      onClick={(event) => {
        event.stopPropagation();
        void navigator.clipboard?.writeText(value).then(() => onCopied?.(), () => undefined);
      }}
      // A 44px hit area on touch layouts; inline text from md up.
      className={cn("inline-flex min-h-11 items-center font-mono hover:text-accent md:min-h-0", className)}
    >
      {display ?? value}
    </button>
  );
}

const THEME_KEY = "repowire-theme";
type Theme = "light" | "dark";

// data-theme on <html> is the source of truth (set before first paint by the
// script in app/layout.tsx). Read through useSyncExternalStore so the icon
// stays in sync without a hydration mismatch.
function subscribe(onChange: () => void) {
  const observer = new MutationObserver(onChange);
  observer.observe(document.documentElement, { attributes: true, attributeFilter: ["data-theme"] });
  return () => observer.disconnect();
}
function currentTheme(): Theme {
  return document.documentElement.getAttribute("data-theme") === "dark" ? "dark" : "light";
}

/** Light and dark, persisted, shared by every product surface. */
export function ThemeToggle({ className }: { className?: string }) {
  const theme = useSyncExternalStore(subscribe, currentTheme, () => "light" as Theme);
  const next: Theme = theme === "dark" ? "light" : "dark";
  return (
    <IconButton
      label={theme === "dark" ? "Switch to light mode" : "Switch to dark mode"}
      variant="secondary"
      suppressHydrationWarning
      className={className}
      onClick={() => {
        document.documentElement.setAttribute("data-theme", next);
        try {
          localStorage.setItem(THEME_KEY, next);
        } catch {
          // localStorage may be unavailable; the theme still applies for the session.
        }
      }}
    >
      {theme === "dark" ? <Sun aria-hidden className="h-3.5 w-3.5" /> : <Moon aria-hidden className="h-3.5 w-3.5" />}
    </IconButton>
  );
}
