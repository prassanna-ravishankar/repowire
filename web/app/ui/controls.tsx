import type { ButtonHTMLAttributes, ReactNode } from "react";
import { cn } from "./cn";

// Controls are 44px tall on touch layouts (the craft floor) and compact from
// md up, where a pointer is the norm. Labels use the mono chrome of the
// product surfaces; prose stays in the body face.

const base =
  "inline-flex items-center justify-center gap-1.5 rounded font-mono text-[10px] font-bold uppercase tracking-[0.12em] transition-[color,background-color,border-color,filter] duration-[120ms] focus-visible:outline-none focus-visible:shadow-[var(--ring)] disabled:cursor-not-allowed disabled:opacity-50";

const sizes = {
  md: "h-11 px-3 md:h-8 md:px-2.5",
  icon: "h-11 w-11 md:h-8 md:w-8",
} as const;

const variants = {
  /** The one primary action on a screen. */
  primary: "bg-accent text-on-accent hover:bg-accent-hover",
  /** Everything else that is a button. */
  secondary: "border border-border text-fg-muted hover:bg-raised hover:text-fg",
  /** Toolbars and inline actions. */
  ghost: "text-fg-subtle hover:bg-raised hover:text-fg",
  /** Irreversible actions, behind a confirmation. */
  danger: "bg-danger text-on-danger hover:brightness-110",
} as const;

export type ButtonVariant = keyof typeof variants;
export type ButtonSize = keyof typeof sizes;

/** The class string for a button-looking element (a link styled as a button, say). */
export function buttonClass(variant: ButtonVariant = "secondary", size: ButtonSize = "md", className?: string) {
  return cn(base, sizes[size], variants[variant], className);
}

export function Button({
  variant = "secondary",
  size = "md",
  className,
  type = "button",
  ...props
}: ButtonHTMLAttributes<HTMLButtonElement> & { variant?: ButtonVariant; size?: ButtonSize }) {
  return <button type={type} className={buttonClass(variant, size, className)} {...props} />;
}

/** An icon-only button. `label` is its accessible name and tooltip. */
export function IconButton({ label, children, variant = "ghost", className, ...props }: ButtonHTMLAttributes<HTMLButtonElement> & { label: string; children: ReactNode; variant?: ButtonVariant }) {
  return (
    <Button size="icon" variant={variant} aria-label={label} title={label} className={className} {...props}>
      {children}
    </Button>
  );
}

/** Text inputs and textareas. 16px on touch layouts, so iOS does not zoom. */
export const inputClass =
  "w-full rounded border border-border bg-sunken px-3 py-2.5 font-mono text-base text-fg outline-none transition-colors placeholder:text-fg-subtle focus:border-accent focus:ring-1 focus:ring-accent md:py-2 md:text-sm";

/** A small mono label above a field or a section. */
export const labelClass = "font-mono text-[10px] font-semibold uppercase tracking-[0.18em] text-fg-subtle";
