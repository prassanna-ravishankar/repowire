# Hook Payloads

Hook payloads vary by agent runtime. Repowire normalizes them before handler code updates peer state or extracts responses.

## Normalized concepts

| Concept | Claude Code | Codex | Gemini |
| --- | --- | --- | --- |
| Prompt event | `UserPromptSubmit` | `UserPromptSubmit` | `BeforeAgent` |
| Stop event | `Stop` | `Stop` | `AfterAgent` |
| Response field | transcript JSONL | `last_assistant_message` | `prompt_response` |
| Hook output | empty | empty | `{"decision": "allow"}` |

## Default delivery path

The default hooks + MCP transport uses hooks for lifecycle and Stop-hook reminders, MCP for outbound commands, and tmux pane injection for live inbound delivery.

## Related

- [Operate: transports](../operate/transports.md)
- [Troubleshooting: hooks not firing](../troubleshooting/hooks.md)
- [Connect Claude Code](../use/features/connect-claude-code.md)
