import { useState } from "react";
import { ApiError, managedApi, type Agent, type Workspace } from "../lib/api";
import { agentLabel } from "../lib/state";
import { InlineError, Modal, buttonClass, dangerButtonClass, inputClass, labelClass, primaryButtonClass } from "./ui";

const ID_PATTERN = /^[a-zA-Z0-9_-]{1,80}$/;

export function CreateAgentDialog({
  workspaces,
  onClose,
  onCreated,
}: {
  workspaces: Workspace[];
  onClose: () => void;
  onCreated: (agent: Agent, existed: boolean) => void;
}) {
  const [id, setId] = useState("");
  const [workspace, setWorkspace] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const idValid = ID_PATTERN.test(id);
  const wsValid = !workspace || ID_PATTERN.test(workspace);
  const shared = workspaces.find((w) => w.id === workspace);

  const submit = async () => {
    if (!idValid || !wsValid || busy) return;
    setBusy(true);
    setError(null);
    try {
      const res = await managedApi.createAgent({ id, ...(workspace ? { workspace } : {}) });
      onCreated(res.agent, !res.created);
    } catch (err) {
      setError(err instanceof ApiError && err.status === 409 ? `${err.message} (409: this id is attached to a different workspace)` : (err as Error).message);
      setBusy(false);
    }
  };

  return (
    <Modal
      title="New managed agent"
      onClose={onClose}
      footer={
        <>
          <button onClick={onClose} className={buttonClass}>Cancel</button>
          <button onClick={submit} disabled={!idValid || !wsValid || busy} className={primaryButtonClass}>
            {busy ? "Creating" : "Create agent"}
          </button>
        </>
      }
    >
      <form
        className="space-y-4"
        onSubmit={(event) => {
          event.preventDefault();
          void submit();
        }}
      >
        <label className="block space-y-1.5">
          <span className={labelClass}>Id</span>
          <input autoFocus value={id} onChange={(e) => setId(e.target.value)} placeholder="reviewer-2" className={inputClass} aria-invalid={id !== "" && !idValid} />
          <span className={id && !idValid ? "block font-mono text-[11px] text-error" : "block font-mono text-[11px] text-outline"}>Letters, digits, - and _. Up to 80.</span>
        </label>
        <label className="block space-y-1.5">
          <span className={labelClass}>Workspace (optional)</span>
          <input value={workspace} onChange={(e) => setWorkspace(e.target.value)} list="managed-workspaces" placeholder="none" className={inputClass} aria-invalid={!wsValid} />
          <datalist id="managed-workspaces">
            {workspaces.map((w) => (
              <option key={w.id} value={w.id} />
            ))}
          </datalist>
          <span className="block font-mono text-[11px] text-outline">
            {shared && shared.agents.length > 0 ? `Shared with ${shared.agents.join(", ")}. ` : workspace ? "A new workspace. " : "Without one the agent has no shell. "}
            Cannot be changed later.
          </span>
        </label>
        <button type="submit" hidden />
      </form>
      {error && <InlineError error={error} />}
    </Modal>
  );
}

export function RetireDialog({
  agent,
  workspace,
  onClose,
  onRetired,
}: {
  agent: Agent;
  workspace: Workspace | null;
  onClose: () => void;
  onRetired: (agent: Agent) => void;
}) {
  const [typed, setTyped] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const others = workspace?.agents.filter((id) => id !== agent.id) ?? [];
  const confirmed = typed === agent.id;

  const submit = async () => {
    if (!confirmed || busy) return;
    setBusy(true);
    setError(null);
    try {
      onRetired(await managedApi.retire(agent.id));
    } catch (err) {
      setError((err as Error).message);
      setBusy(false);
    }
  };

  return (
    <Modal
      title={`Retire ${agentLabel(agent)}`}
      onClose={onClose}
      footer={
        <>
          <button onClick={onClose} className={buttonClass}>Cancel</button>
          <button onClick={submit} disabled={!confirmed || busy} className={dangerButtonClass}>
            {busy ? "Retiring" : "Retire agent"}
          </button>
        </>
      }
    >
      <p className="text-sm leading-6 text-on-surface-variant">
        Retire <code className="font-mono text-on-surface">{agent.id}</code>? A running run is aborted and it stops accepting messages. Its transcript stays readable.
        {agent.workspace &&
          (others.length > 0
            ? ` Workspace ${agent.workspace} is shared with ${others.join(", ")} and is kept.`
            : ` Workspace ${agent.workspace} is kept.`)}
      </p>
      <label className="block space-y-1.5">
        <span className={labelClass}>Type {agent.id} to confirm</span>
        <input
          autoFocus
          value={typed}
          onChange={(e) => setTyped(e.target.value)}
          onKeyDown={(e) => e.key === "Enter" && void submit()}
          className={inputClass}
          autoComplete="off"
          spellCheck={false}
        />
      </label>
      {error && <InlineError error={error} />}
    </Modal>
  );
}
