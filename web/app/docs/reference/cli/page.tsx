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

      <Cmd name="repowire setup" usage="repowire setup [--relay] [--experimental-channels] [--http-mcp] [--non-interactive]">
        <p>
          One-time install. Detects every supported agent runtime present (Claude Code, Codex, Gemini CLI, OpenCode), wires the appropriate Repowire transport for each, and installs the daemon as a user service. <Mono>--relay</Mono> opts in to the hosted relay at <Mono>repowire.io</Mono>. <Mono>--experimental-channels</Mono> enables the experimental MCP channel / ACP transport for Claude Code. <Mono>--http-mcp</Mono> enables localhost Streamable HTTP MCP at <Mono>/mcp</Mono> and generates a bearer token if needed. <Mono>--non-interactive</Mono> skips prompts and uses flag values only.
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
repowire peer prune`}
      >
        <p>
          Inspect and repair registered peers. <Mono>peer list</Mono> is an operator view across all circles. <Mono>peer describe</Mono> shows one peer&apos;s identity, role, liveness, open asks, and recent events. <Mono>peer claim-role orchestrator</Mono> is a narrow repair command for an existing peer whose durable session mapping lost the orchestrator role after daemon restart; it refuses to demote a fresh online or busy holder unless <Mono>--force</Mono> is passed.
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
          Re-install repowire via the same package manager that installed it. Use after pulling a new release.
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
