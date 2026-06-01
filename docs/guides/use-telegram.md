# Use Telegram

## Goal

Control Repowire from your phone and receive agent updates through Telegram.

## Before you start

Create a Telegram bot token and know the chat id Repowire should accept.

## Steps

1. Configure the bot:

   ```bash
   TELEGRAM_BOT_TOKEN=... TELEGRAM_CHAT_ID=... repowire telegram start
   ```

2. Select a peer with `/select`.
3. Send normal messages to open tracked asks to the selected peer.
4. Use `/notify` or `/fyi` for fire-and-forget nudges.
5. Use attachments when you want the target agent to read an uploaded file or image from the local path.

## Verify

Send `/peers` from Telegram and confirm the expected non-Telegram peers appear. If messages do not arrive, check [Telegram](../capabilities/telegram.md) and [Diagnostic commands](../troubleshooting/diagnostics.md).

## Related

- [Capabilities: Telegram](../capabilities/telegram.md)
- [Pattern: mobile mesh management](../patterns/mobile-mesh.md)
