"use client";

import { useEffect, useState } from "react";
import Plate from "./Plate";

// An agent's ask as the Telegram bot renders it (daemon-go/mobile/telegram.go):
// "❓ @peer", the question, "[ask #id]", then one inline button per choice.
// Tapping one posts /answer and the bot confirms with "✓ Answered #id".
const CID = "c91f2b";
const CHOICES = [
  { id: "ship", title: "Ship now", reply: "Shipping 0043. I'll notify you when it's applied." },
  { id: "hold", title: "Hold for review", reply: "Holding. Opened a PR and asked @review-codex to look." },
];

/** The phone side of the mesh: an ask from an agent with inline answer buttons. Tap one to answer it. */
export default function PhoneChat({ fig, caption }: { fig: string; caption: string }) {
  const [choice, setChoice] = useState<(typeof CHOICES)[number] | null>(null);
  const [replied, setReplied] = useState(false);

  useEffect(() => {
    if (!choice) return;
    const t = setTimeout(() => setReplied(true), 900);
    return () => clearTimeout(t);
  }, [choice]);

  const read = !choice ? `ask #${CID} · open` : `answered · ${choice.id}`;

  return (
    <Plate fig={fig} read={read} caption={caption}>
      <div className="chat-stage">
        <div className="chat-screen" role="group" aria-label="A Repowire ask from an agent, shown in Telegram with answer buttons">
          <div className="chat-head">
            <span className="chat-avatar" aria-hidden />
            <span className="chat-title">repowire bot</span>
            {choice && replied && (
              <button type="button" className="chat-reset" onClick={() => { setChoice(null); setReplied(false); }}>
                replay
              </button>
            )}
          </div>
          <div className="chat-log">
            <p className="bubble in">
              <span className="bubble-from">@api-claude</span>
              Migration 0042 applied. CI green.
            </p>
            <div className="bubble in ask">
              <span className="bubble-from">❓ @db-codex</span>
              Migration 0043 drops <code>legacy_sessions</code>. Ship it now, or hold for review?
              <span className="bubble-cid">[ask #{CID}]</span>
              <div className="chat-buttons">
                {CHOICES.map((c) => (
                  <button
                    key={c.id}
                    type="button"
                    className={choice?.id === c.id ? "picked" : ""}
                    disabled={!!choice}
                    onClick={() => setChoice(c)}
                  >
                    {c.title}
                  </button>
                ))}
              </div>
            </div>
            {choice && <p className="bubble sys">✓ Answered #{CID}</p>}
            {choice && replied && (
              <p className="bubble in">
                <span className="bubble-from">@db-codex</span>
                {choice.reply}
              </p>
            )}
          </div>
        </div>
      </div>
    </Plate>
  );
}
