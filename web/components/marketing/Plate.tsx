import type { ReactNode } from "react";

/**
 * A drawing plate: the frame every figure on the page sits in. The figure's
 * live read-out top right, a caption underneath.
 */
export default function Plate({
  read,
  caption,
  children,
  className = "",
}: {
  read: string;
  caption: ReactNode;
  children: ReactNode;
  className?: string;
}) {
  return (
    <figure className={`plate ${className}`}>
      <div className="plate-sheet">
        <span className="plate-tag plate-read" aria-live="polite">{read}</span>
        <span className="plate-tick tl" aria-hidden />
        <span className="plate-tick tr" aria-hidden />
        <span className="plate-tick bl" aria-hidden />
        <span className="plate-tick br" aria-hidden />
        {children}
      </div>
      <figcaption>{caption}</figcaption>
    </figure>
  );
}
