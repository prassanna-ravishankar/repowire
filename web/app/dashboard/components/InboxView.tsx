import { useState, type ReactNode } from "react";
import ReactMarkdown from "react-markdown";
import remarkGfm from "remark-gfm";
import { ArrowLeft, Inbox, RefreshCw, Send, XCircle } from "lucide-react";
import { cn, timeAgo } from "../lib/utils";
import type { InboxState } from "../lib/useInbox";
import type { InboxItem, InboxReply } from "../types";
import { EmptyState } from "./EmptyState";
import { AttachmentChips } from "./MeshFeed";

const KIND_LABEL: Record<InboxItem["kind"], string> = {
  notify: "message",
  ask: "ask",
  answer: "reply",
  broadcast: "broadcast",
};

function kindTone(kind: InboxItem["kind"]): string {
  if (kind === "ask") return "border-tertiary-fixed-dim/30 bg-tertiary-fixed-dim/10 text-tertiary-fixed-dim";
  if (kind === "answer") return "border-secondary/25 bg-secondary/10 text-secondary";
  return "border-border-faint bg-surface-container-low text-outline";
}

/** An ask the human has not answered yet: replying closes it. */
function isOpenAsk(item: InboxItem): boolean {
  return item.kind === "ask" && Boolean(item.correlation_id) && !item.replied;
}

function preview(text: string): string {
  return text.replace(/\s+/g, " ").trim();
}

export function InboxView({ inbox, apiBase }: { inbox: InboxState; apiBase: string }) {
  const [selectedId, setSelectedId] = useState<string | null>(null);
  const selected = inbox.items.find((item) => item.id === selectedId) ?? null;

  const open = (item: InboxItem) => {
    setSelectedId(item.id);
    if (!item.read) inbox.markRead(item.id);
  };

  return (
    <>
      <div className="sticky top-[var(--topbar-offset)] z-10 flex flex-wrap items-center justify-between gap-3 border-b border-border-faint bg-surface-dim px-4 py-3 md:static md:px-6">
        <div>
          <div className="font-mono text-[10px] font-semibold uppercase tracking-[0.22em] text-primary">INBOX / to you</div>
          <h1 className="mt-1 font-headline text-2xl font-semibold text-on-surface">inbox</h1>
        </div>
        <div className="border border-border-faint bg-surface-container-low px-2.5 py-1 text-right font-mono">
          <div className="text-sm font-bold tabular-nums text-primary-fixed">{inbox.unread}</div>
          <div className="text-[9px] uppercase tracking-[0.16em] text-outline">unread</div>
        </div>
      </div>

      <div className="grid min-h-0 flex-1 bg-surface-dim md:grid-cols-[minmax(300px,420px)_1fr] md:overflow-hidden">
        <div className={cn("min-h-0 border-r border-border-faint md:block md:overflow-y-auto", selected ? "hidden" : "block")}>
          {inbox.loading && inbox.items.length === 0 ? (
            <EmptyState icon={<RefreshCw className="h-5 w-5 animate-spin" />} title="loading inbox" detail="reading your inbox from the daemon" />
          ) : inbox.error === "unavailable" ? (
            <EmptyState icon={<XCircle className="h-5 w-5" />} title="inbox unavailable" detail="this daemon has no human peer; check the daemon log" />
          ) : inbox.error ? (
            <EmptyState icon={<XCircle className="h-5 w-5" />} title="inbox unavailable" detail={inbox.error} />
          ) : inbox.items.length === 0 ? (
            <EmptyState icon={<Inbox className="h-5 w-5" />} title="inbox empty" detail="messages and asks agents send you land here" />
          ) : (
            <ul aria-label="Inbox">
              {inbox.items.map((item) => (
                <li key={item.id}>
                  <InboxRow item={item} active={item.id === selectedId} onClick={() => open(item)} />
                </li>
              ))}
            </ul>
          )}
        </div>

        <div className={cn("min-h-0 md:block md:overflow-y-auto", selected ? "block" : "hidden")}>
          {selected ? (
            <InboxDetail key={selected.id} item={selected} apiBase={apiBase} onBack={() => setSelectedId(null)} onReply={(body) => inbox.reply(selected.id, body)} />
          ) : (
            <EmptyState icon={<Inbox className="h-5 w-5" />} title="select a message" detail="opening a message marks it read on every surface" />
          )}
        </div>
      </div>
    </>
  );
}

function InboxRow({ item, active, onClick }: { item: InboxItem; active: boolean; onClick: () => void }) {
  return (
    <button
      onClick={onClick}
      aria-pressed={active}
      data-unread={item.read ? undefined : "true"}
      className={cn(
        "block w-full border-b border-border-faint border-l-2 px-3 py-2.5 text-left transition-colors",
        active ? "border-l-primary bg-primary/10" : "border-l-transparent hover:bg-surface-container",
      )}
    >
      <div className="mb-1 flex min-w-0 items-center gap-2">
        <span
          aria-label={item.read ? undefined : "unread"}
          className={cn("h-2 w-2 shrink-0 rounded-full", item.read ? "bg-transparent" : "bg-primary")}
        />
        <span className={cn("min-w-0 flex-1 truncate font-mono text-[13px]", item.read ? "text-on-surface-variant" : "font-semibold text-on-surface")}>
          @{item.from}
        </span>
        <span className="shrink-0 font-mono text-[11px] text-outline">{timeAgo(item.created_at)}</span>
      </div>
      <div className="flex min-w-0 items-center gap-2 pl-4">
        <span className={cn("shrink-0 border px-1.5 py-0.5 font-mono text-[9px] uppercase tracking-[0.12em]", kindTone(item.kind))}>
          {KIND_LABEL[item.kind]}
        </span>
        <span className="min-w-0 flex-1 truncate font-mono text-[11px] leading-5 text-outline">{preview(item.text)}</span>
        {item.replied && <span className="shrink-0 font-mono text-[11px] text-outline" title="replied">↩</span>}
      </div>
    </button>
  );
}

function InboxDetail({
  item,
  apiBase,
  onBack,
  onReply,
}: {
  item: InboxItem;
  apiBase: string;
  onBack: () => void;
  onReply: (body: InboxReply) => Promise<{ action?: string; error?: string }>;
}) {
  const [text, setText] = useState("");
  const [pending, setPending] = useState(false);
  const [status, setStatus] = useState<{ ok: boolean; message: string } | null>(null);
  const openAsk = isOpenAsk(item);
  const options = openAsk && item.question?.kind === "choice" ? item.question.options ?? [] : [];

  const send = async (body: InboxReply) => {
    setPending(true);
    setStatus(null);
    const result = await onReply(body);
    setPending(false);
    if (result.error) {
      setStatus({ ok: false, message: result.error });
      return;
    }
    setText("");
    const done: Record<string, string> = { acked: "replied, ask closed", answered: "answered", asked: `sent to @${item.from}`, read: "marked read" };
    setStatus({ ok: true, message: done[result.action ?? ""] ?? "sent" });
  };

  return (
    <div className="space-y-4 px-4 py-4 md:px-6">
      <button onClick={onBack} className="inline-flex items-center gap-1.5 font-mono text-[10px] font-bold uppercase tracking-[0.12em] text-outline hover:text-on-surface md:hidden">
        <ArrowLeft className="h-3.5 w-3.5" /> inbox
      </button>
      <section className="border border-border-faint bg-surface-container-low p-4">
        <div className="mb-3 flex flex-wrap items-start justify-between gap-3">
          <div className="min-w-0">
            <p className="mb-1 font-mono text-[10px] uppercase tracking-[0.18em] text-outline">
              {KIND_LABEL[item.kind]} / {new Date(item.created_at).toLocaleString([], { month: "short", day: "2-digit", hour: "2-digit", minute: "2-digit" })}
            </p>
            <h2 className="break-words font-headline text-xl font-semibold text-on-surface">@{item.from}</h2>
          </div>
          <span className={cn("shrink-0 border px-2 py-1 font-mono text-[10px] font-semibold uppercase tracking-[0.14em]", kindTone(item.kind))}>
            {openAsk ? "awaiting reply" : item.replied ? `replied${item.reply_surface ? ` via ${item.reply_surface}` : ""}` : KIND_LABEL[item.kind]}
          </span>
        </div>
        <div className="prose prose-sm max-w-none break-words font-mono text-[13px] leading-6 text-on-surface [overflow-wrap:anywhere] [&_pre]:overflow-x-auto">
          <ReactMarkdown remarkPlugins={[remarkGfm]}>{item.text}</ReactMarkdown>
        </div>
        <AttachmentChips attachments={item.attachments} apiBase={apiBase} />
      </section>

      <section className="border border-border-faint bg-surface-container-low p-4">
        {options.length > 0 && (
          <div className="mb-3 flex flex-wrap gap-2">
            {options.map((option) => (
              <ChoiceButton key={option.id} disabled={pending} title={option.description ?? undefined} onClick={() => void send({ option_id: option.id })}>
                {option.title}
              </ChoiceButton>
            ))}
          </div>
        )}
        {openAsk && item.question?.kind === "acknowledge" && (
          <div className="mb-3">
            <ChoiceButton disabled={pending} onClick={() => void send({ text: "" })}>✓ Acknowledge</ChoiceButton>
          </div>
        )}
        <form
          className="flex items-end gap-2"
          onSubmit={(event) => {
            event.preventDefault();
            if (text.trim()) void send({ text: text.trim() });
          }}
        >
          <textarea
            value={text}
            onChange={(event) => setText(event.target.value)}
            onKeyDown={(event) => {
              if (event.key === "Enter" && (event.metaKey || event.ctrlKey)) event.currentTarget.form?.requestSubmit();
            }}
            disabled={pending}
            rows={2}
            aria-label={`Reply to @${item.from}`}
            placeholder={openAsk ? `reply to @${item.from}; this closes their ask` : `reply to @${item.from}; this asks them back`}
            className="max-h-40 min-h-8 flex-1 resize-y rounded border border-border-faint bg-surface-container-lowest px-2.5 py-1.5 font-mono text-xs text-on-surface outline-none placeholder:text-outline focus:border-primary focus:ring-1 focus:ring-primary disabled:opacity-50"
          />
          <button
            type="submit"
            disabled={pending || !text.trim()}
            aria-label="Send reply"
            className={cn(
              "flex h-8 w-8 shrink-0 items-center justify-center rounded transition-[filter,transform] active:scale-[0.98]",
              text.trim() ? "bg-primary text-on-primary hover:brightness-110" : "bg-surface-container-high text-outline",
            )}
          >
            {pending ? <RefreshCw className="h-4 w-4 animate-spin" /> : <Send className="h-4 w-4" />}
          </button>
        </form>
        {status && (
          <p role="status" className={cn("mt-2 font-mono text-xs", status.ok ? "text-primary" : "text-error")}>
            {status.ok ? "✓ " : "✗ "}
            {status.message}
          </p>
        )}
      </section>
    </div>
  );
}

function ChoiceButton({ children, disabled, title, onClick }: { children: ReactNode; disabled: boolean; title?: string; onClick: () => void }) {
  return (
    <button
      type="button"
      disabled={disabled}
      title={title}
      onClick={onClick}
      className="rounded border border-tertiary/40 bg-tertiary/15 px-2.5 py-1 font-mono text-[11px] font-semibold text-on-surface hover:bg-tertiary/25 disabled:opacity-50"
    >
      {children}
    </button>
  );
}
