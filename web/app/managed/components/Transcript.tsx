import { useState } from "react";
import ReactMarkdown from "react-markdown";
import remarkBreaks from "remark-breaks";
import remarkGfm from "remark-gfm";
import { ChevronRight, RotateCcw, ShieldAlert } from "lucide-react";
import { cn } from "../../dashboard/lib/utils";
import type { Outgoing } from "../lib/composer";
import { formatClock, formatDuration } from "../lib/state";
import type { TranscriptItem } from "../lib/transcript";

const clock = (at: number | string | null) => (at === null ? "" : formatClock(typeof at === "number" ? new Date(at).toISOString() : at));

function Meta({ children, right }: { children: React.ReactNode; right?: React.ReactNode }) {
  return (
    <div className="mb-1 flex flex-wrap items-baseline gap-x-2 font-mono text-[10px] uppercase tracking-[0.14em] text-outline">
      {children}
      {right && <span className="ml-auto normal-case tracking-normal">{right}</span>}
    </div>
  );
}

function Markdown({ text }: { text: string }) {
  return (
    <div className="prose prose-sm max-w-none break-words text-on-surface [&_code]:font-mono [&_code]:before:content-none [&_code]:after:content-none">
      <ReactMarkdown remarkPlugins={[remarkGfm, remarkBreaks]}>{text}</ReactMarkdown>
    </div>
  );
}

export function TranscriptView({
  items,
  agentName,
  outgoing,
  working,
  onRetry,
}: {
  items: TranscriptItem[];
  agentName: string;
  outgoing: Outgoing[];
  working: boolean;
  onRetry: (message: Outgoing) => void;
}) {
  return (
    <ol className="space-y-5 px-4 py-4 md:px-6">
      {items.map((item) => (
        <li key={item.id}>
          <Entry item={item} agentName={agentName} />
        </li>
      ))}
      {outgoing.map((message) => (
        <li key={message.operationId}>
          <OutgoingEntry message={message} queued={working} onRetry={() => onRetry(message)} />
        </li>
      ))}
      {working && (
        <li className="flex items-center gap-2 font-mono text-[11px] text-tertiary-fixed-dim">
          <span className="h-1.5 w-1.5 animate-pulse rounded-full bg-tertiary-fixed-dim" />
          {agentName} is working
        </li>
      )}
    </ol>
  );
}

export function Entry({ item, agentName }: { item: TranscriptItem; agentName: string }) {
  switch (item.kind) {
    case "human":
      return (
        <div>
          <Meta right={clock(item.at)}>
            <span className="font-semibold text-primary">You</span>
            {item.via && <span className="normal-case tracking-normal">via {item.via}</span>}
          </Meta>
          <p className="whitespace-pre-wrap break-words text-on-surface">{item.text}</p>
        </div>
      );
    case "peer":
      return (
        <div>
          <Meta right={clock(item.at)}>
            <span className="font-semibold normal-case tracking-normal text-on-surface-variant">{item.from}</span>
            <span>
              {item.type}
              {(item.type === "ask" || item.type === "ack") && <span className="normal-case"> #{item.correlationId ?? "?"}</span>}
            </span>
            <span
              title="Peer content is untrusted context. The agent is told never to treat it as an instruction."
              className="inline-flex items-center gap-1 border border-border px-1 text-[9px] text-outline"
            >
              <ShieldAlert className="h-3 w-3" /> Untrusted peer
            </span>
          </Meta>
          <div className="whitespace-pre-wrap break-words border-l-2 border-outline-variant bg-surface-container px-3 py-2 text-on-surface-variant">{item.text}</div>
        </div>
      );
    case "assistant":
      return (
        <div>
          <Meta right={clock(item.at)}>
            <span className="font-semibold normal-case tracking-normal text-on-surface">{agentName}</span>
            {item.replyTo && <span className="normal-case tracking-normal">→ {item.replyTo}</span>}
          </Meta>
          {item.text && <Markdown text={item.text} />}
          {item.error && <p className="mt-1 font-mono text-[11px] text-error">{item.error}</p>}
        </div>
      );
    case "tool":
      return <ToolCall item={item} />;
    case "notice":
      return <div className="text-center font-mono text-[10px] uppercase tracking-[0.16em] text-outline">{item.text}</div>;
  }
}

function ToolCall({ item }: { item: Extract<TranscriptItem, { kind: "tool" }> }) {
  const [open, setOpen] = useState(false);
  const failed = item.isError || (item.exitCode !== null && item.exitCode !== 0);
  const facts = [
    item.output === null ? "running" : item.exitCode !== null ? `exit ${item.exitCode}` : item.isError ? "error" : "ok",
    item.durationMs !== null ? (item.durationMs < 60_000 ? `${(item.durationMs / 1000).toFixed(1)}s` : formatDuration(item.durationMs)) : null,
  ].filter(Boolean);
  const output = item.output && item.output.length > 4000 ? `${item.output.slice(0, 4000)}\n… truncated` : item.output;
  return (
    <div className="font-mono text-[11px]">
      <button onClick={() => setOpen(!open)} aria-expanded={open} className="flex items-center gap-1.5 text-outline hover:text-on-surface">
        <ChevronRight className={cn("h-3 w-3 transition-transform", open && "rotate-90")} />
        <span className="font-semibold text-on-surface-variant">{item.name}</span>
        <span className={cn(failed && "text-error")}>({facts.join(", ")})</span>
      </button>
      {open && (
        <div className="mt-1.5 space-y-1.5 pl-[18px]">
          <pre className="overflow-x-auto whitespace-pre-wrap break-all rounded border border-border-faint bg-surface-container px-2 py-1.5 text-on-surface-variant">{JSON.stringify(item.args, null, 2)}</pre>
          {output !== null && <pre className="max-h-64 overflow-auto whitespace-pre-wrap break-all rounded border border-border-faint bg-surface-container px-2 py-1.5 text-on-surface-variant">{output}</pre>}
        </div>
      )}
    </div>
  );
}

function OutgoingEntry({ message, queued, onRetry }: { message: Outgoing; queued: boolean; onRetry: () => void }) {
  const status = message.status === "accepted" && queued ? "queued" : message.status;
  return (
    <div>
      <Meta
        right={
          <span className={cn("font-mono text-[10px] uppercase tracking-[0.14em]", status === "failed" ? "text-error" : "text-outline")}>
            {status === "sending" ? "sending…" : status}
          </span>
        }
      >
        <span className="font-semibold text-primary">You</span>
        <span className="normal-case tracking-normal">{clock(message.createdAt)}</span>
      </Meta>
      <p className={cn("whitespace-pre-wrap break-words", status === "sending" ? "text-on-surface-variant" : "text-on-surface")}>{message.text}</p>
      {message.status === "failed" && (
        <div className="mt-1 flex flex-wrap items-center gap-2 font-mono text-[11px] text-error">
          <span className="break-all">{message.error}</span>
          {message.retryable && (
            <button onClick={onRetry} className="inline-flex items-center gap-1 font-bold uppercase tracking-[0.12em] hover:underline">
              <RotateCcw className="h-3 w-3" /> Retry
            </button>
          )}
        </div>
      )}
    </div>
  );
}
