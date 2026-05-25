export const metadata = {
  title: "CLI · Repowire Docs",
};

export default function CliReference() {
  return (
    <article className="max-w-3xl">
      <p className="font-mono text-[10px] font-semibold uppercase tracking-[0.2em] text-primary">
        Reference
      </p>
      <h1 className="mt-3 font-headline text-3xl font-bold text-on-surface sm:text-4xl">
        CLI
      </h1>
      <p className="mt-4 text-base leading-7 text-on-surface-variant">
        The <Mono>repowire</Mono> command is a thin wrapper around setup, the daemon, and the bot peers. Most users only ever need <Mono>setup</Mono>. Everything else is for operators running their own daemon or control surfaces.
      </p>

      <Cmd name="repowire setup" usage="repowire setup [--relay] [--experimental-channels] [--http-mcp] [--update-checks|--no-update-checks] [--no-service] [--non-interactive]">
        <p>
          One-time install. Detects every supported agent runtime present (Claude Code, Codex, Gemini CLI, OpenCode), wires the appropriate Repowire transport for each, and installs the daemon as a user service. <Mono>--relay</Mono> opts in to the hosted relay at <Mono>repowire.io</Mono>. <Mono>--experimental-channels</Mono> enables the experimental MCP channel / ACP transport for Claude Code. <Mono>--http-mcp</Mono> enables localhost Streamable HTTP MCP at <Mono>/mcp</Mono> and generates a bearer token if needed. <Mono>--update-checks</Mono> lets <Mono>status</Mono> and <Mono>doctor</Mono> check PyPI for newer releases; <Mono>--no-update-checks</Mono> disables it again. <Mono>--no-service</Mono> skips daemon service installation. <Mono>--non-interactive</Mono> skips prompts and uses flag values only.
        </p>
        <p>
          Repowire uses SQLite state. On first daemon startup after install or update, it applies migrations and imports legacy <Mono>schedules.json</Mono>, <Mono>events.json</Mono>, and <Mono>sessions.json</Mono> once while leaving those files in place for downgrade/export compatibility. Migrated state is written to <Mono>state.db</Mono>.
        </p>
      </Cmd>

      <Cmd name="repowire serve" usage="repowire serve [--host HOST] [--port PORT] [--relay]">
        <p>
          Run the daemon in the foreground. Useful for debugging hooks or running outside the installed service. Defaults to <Mono>127.0.0.1:8377</Mono>.
        </p>
      </Cmd>

      <Cmd
        name="repowire service"
        usage={`repowire service install
repowire service restart
repowire service status
repowire service uninstall`}
      >
        <p>
          Manage the installed daemon user service. <Mono>install</Mono> writes and starts the platform service (<Mono>launchd</Mono> on macOS, <Mono>systemd --user</Mono> on Linux), <Mono>restart</Mono> restarts the installed daemon after a local reinstall or config change, <Mono>status</Mono> shows whether it is installed/running, and <Mono>uninstall</Mono> removes the service entry. Prefer these commands over raw <Mono>launchctl</Mono> or <Mono>systemctl</Mono> unless you are troubleshooting the platform service manager directly.
        </p>
      </Cmd>

      <Cmd name="repowire build-ui" usage="repowire build-ui">
        <p>
          Build the Next.js dashboard into the static export served by the daemon at <Mono>/dashboard</Mono>. Run after editing files under <Mono>web/</Mono>.
        </p>
      </Cmd>

      <Cmd
        name="repowire peer"
        usage={`repowire peer list
repowire peer describe NAME_OR_ID [--circle C]
repowire peer claim-role orchestrator [--peer NAME_OR_ID] [--circle C] [--force]
repowire peer restart NAME_OR_ID [--circle C] [--dry-run] [-m MESSAGE]
repowire peer prune
repowire peer whoami [--register --backend B --name NAME --circle C --path P]
repowire peer asks [--peer-id ID | --pane-id PANE | --peer NAME]
repowire peer ack CORR_ID [-m MESSAGE] [--from-peer NAME]`}
      >
        <p>
          Inspect and repair registered peers. <Mono>peer list</Mono> is an operator view across all circles. <Mono>peer describe</Mono> shows one peer&apos;s identity, role, liveness, open asks, and recent events. <Mono>peer claim-role orchestrator</Mono> is a narrow repair command for an existing peer whose durable session mapping lost the orchestrator role after daemon restart; it refuses to demote a fresh online or busy holder unless <Mono>--force</Mono> is passed.
        </p>
        <p>
          <Mono>peer whoami</Mono>, <Mono>peer asks</Mono>, and <Mono>peer ack</Mono> are shellable mesh primitives for runtimes whose hooks do not fire yet. They use the local daemon token automatically and let an Antigravity <Mono>agy</Mono> session self-register, poll pending asks, and close them from a shell.
        </p>
        <p>
          <Mono>peer new</Mono> accepts <Mono>--profile</Mono> to append configured <Mono>daemon.spawn.profiles</Mono> args to the backend command. <Mono>peer restart</Mono> intentionally restarts a daemon-spawned peer on the same backend, path, circle, role, and mesh identity. It is for reloading startup context such as <Mono>AGENTS.md</Mono> or an orchestrator <Mono>SOUL.md</Mono>. It requires explicit spawn ownership proof plus live tmux evidence, so manually attached peers and stale or mismatched pane records are refused instead of killed. Restart is same-window/name first, not same-pane; tmux allocates a fresh pane through the normal spawn path. The restart mode is <Mono>fresh_runtime_context</Mono>: startup context is reloaded, but transcript replay, selected spawn profile, and exact backend conversation resume are not guaranteed unless Repowire deliberately selects and reports a backend-specific mode.
        </p>
      </Cmd>

      <Cmd
        name="repowire schedule"
        usage={`repowire schedule self WHEN_OR_CRON TEXT [--cron]
repowire schedule create TO_PEER WHEN_OR_CRON TEXT --from-peer FROM_PEER [--cron]
repowire schedule list
repowire schedule delete SCHEDULE_ID`}
      >
        <p>
          Create one-shot or recurring scheduled mesh messages. Without <Mono>--cron</Mono>, the time may be ISO-8601 or relative, such as <Mono>10m</Mono>, <Mono>1h</Mono>, or <Mono>in 30s</Mono>. With <Mono>--cron</Mono>, use five-field cron syntax or aliases such as <Mono>@hourly</Mono>, <Mono>@daily</Mono>, <Mono>@midnight</Mono>, <Mono>@weekly</Mono>, and <Mono>@monthly</Mono>. Add <Mono>--kind ask</Mono> when the scheduled delivery should open an ask thread.
        </p>
      </Cmd>

      <Cmd
        name="repowire orchestrator persona"
        usage={`repowire orchestrator persona list
repowire orchestrator persona show [NAME]
repowire orchestrator persona path [NAME]
repowire orchestrator persona use NAME
repowire orchestrator persona clear`}
      >
        <p>
          Manage orchestrator persona <Mono>SOUL.md</Mono> files. Repowire resolves workspace personas from <Mono>~/.repowire/orchestrator/personas/&lt;name&gt;/SOUL.md</Mono> first, then global personas from <Mono>~/.repowire/personas/&lt;name&gt;/SOUL.md</Mono>. <Mono>use</Mono> writes the workspace active marker.
        </p>
        <p>
          On SessionStart, orchestrator peers receive the active persona with its source path and SHA-256 short hash. Persona context is identity guidance, not a permission policy.
        </p>
      </Cmd>

      <Cmd
        name="repowire memory"
        usage={`repowire memory path [--scope SCOPE] [--project NAME] [--persona NAME]
repowire memory list [--scope SCOPE] [--project NAME] [--persona NAME]
repowire memory show SLUG [--scope SCOPE] [--project NAME] [--persona NAME]
repowire memory search QUERY [--scope SCOPE] [--project NAME] [--persona NAME] [--all]
repowire memory write SLUG --body BODY [--scope SCOPE] [--project NAME] [--persona NAME] [--type TYPE] [--description TEXT] [--append|--force]`}
      >
        <p>
          Inspect and explicitly write filesystem-backed mesh memory under <Mono>~/.repowire/memory/</Mono>. The first slice resolves scope directories, lists Markdown memories, prints a memory by slug, searches text, and writes one curated memory at a time.
        </p>
        <p>
          Scopes are <Mono>global</Mono>, <Mono>user</Mono>, <Mono>project</Mono>, <Mono>persona</Mono>, and <Mono>orchestrator</Mono>. Writes never happen from hooks, transcripts, schedules, or daemon side effects. Existing memories are protected unless <Mono>--force</Mono> overwrites or <Mono>--append</Mono> adds to the current body.
        </p>
        <p>
          Proposed memory writes must show a full-file or unified diff before someone runs <Mono>repowire memory write</Mono>. Rejections leave files unchanged; edited proposals need a fresh diff before write.
        </p>
      </Cmd>

      <Cmd name="repowire telegram start" usage="repowire telegram start">
        <p>
          Run the Telegram bot peer. Reads <Mono>TELEGRAM_BOT_TOKEN</Mono> and <Mono>TELEGRAM_CHAT_ID</Mono> from the environment. The bot registers as the <Mono>telegram</Mono> peer; messages from it are framed as human input.
        </p>
      </Cmd>

      <Cmd name="repowire slack start" usage="repowire slack start">
        <p>
          Run the Slack bot peer over Socket Mode (no public URL needed). Reads <Mono>SLACK_BOT_TOKEN</Mono>, <Mono>SLACK_APP_TOKEN</Mono>, and <Mono>SLACK_CHANNEL_ID</Mono>.
        </p>
      </Cmd>

      <Cmd name="repowire update" usage="repowire update">
        <p>
          Re-install repowire via the same package manager that installed it. Use after pulling a new release. When config enables optional runtime support such as ACP, <Mono>update</Mono> upgrades the matching package extra so the service runtime keeps the dependency. After reinstalling hooks/plugins, <Mono>update</Mono> restarts the daemon service when it is running. SQLite state migrations run during that daemon restart; verify with <Mono>repowire doctor</Mono>. This is the only command that upgrades the installed package; hooks, MCP calls, daemon routing, <Mono>status</Mono>, and <Mono>doctor</Mono> never auto-update Repowire.
        </p>
      </Cmd>

      <Cmd name="repowire uninstall" usage="repowire uninstall [--yes]">
        <p>
          Remove hooks, MCP entries, and the daemon service. Prompts before deleting <Mono>~/.repowire/</Mono> (config, logs, attachments); decline to keep it for reinstalls. <Mono>--yes</Mono> skips the prompts and removes the directory along with the installed package.
        </p>
      </Cmd>

      <div className="mt-12 border-t border-border-faint pt-8">
        <div className="mb-3 font-mono text-[10px] font-semibold uppercase tracking-[0.2em] text-outline">
          See also
        </div>
        <p className="text-sm leading-6 text-on-surface-variant">
          Configuration lives in <Mono>~/.repowire/config.yaml</Mono>. The <a className="text-primary-fixed underline-offset-4 hover:underline" href="/docs/reference/tools">MCP tools</a> reference covers what agents call once the daemon is running.
        </p>
      </div>
    </article>
  );
}

function Cmd({
  name,
  usage,
  children,
}: {
  name: string;
  usage: string;
  children: React.ReactNode;
}) {
  return (
    <section className="mt-10">
      <h2 className="font-mono text-base font-semibold text-primary-fixed">{name}</h2>
      <pre className="mt-3 overflow-x-auto border border-border-faint bg-surface-container-low p-3 font-mono text-xs leading-6 text-on-surface">
        <code>{usage}</code>
      </pre>
      <div className="mt-4 space-y-4 text-sm leading-6 text-on-surface-variant">{children}</div>
    </section>
  );
}

function Mono({ children }: { children: React.ReactNode }) {
  return <code className="font-mono text-primary-fixed">{children}</code>;
}
