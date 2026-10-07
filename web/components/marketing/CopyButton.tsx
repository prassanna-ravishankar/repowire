"use client";

import { useState } from "react";
import { Check, Copy } from "lucide-react";

export default function CopyButton({ text, label = "Copy install command" }: { text: string; label?: string }) {
  const [copied, setCopied] = useState(false);

  function copy() {
    if (!navigator.clipboard) return;
    navigator.clipboard
      .writeText(text)
      .then(() => {
        setCopied(true);
        setTimeout(() => setCopied(false), 1400);
      })
      .catch(() => {
        // Clipboard write was blocked/failed; don't show a false "Copied".
      });
  }

  return (
    <button className="copy-btn" onClick={copy} aria-label={label}>
      {copied ? <Check width={14} height={14} strokeWidth={1.5} /> : <Copy width={14} height={14} strokeWidth={1.5} />}
      <span className="copy-label">{copied ? "Copied" : "Copy"}</span>
    </button>
  );
}
