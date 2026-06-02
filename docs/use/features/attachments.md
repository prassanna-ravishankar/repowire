# Attachments

## What it is

Attachments let dashboard and Telegram users send files or images through the daemon. The receiving message includes structured attachment metadata plus a local file path fallback the target agent can read with its normal tools.

## When to use it

Use attachments when a human wants an agent to inspect a screenshot, image, document, log bundle, or other short-lived file without manually copying it into the target workspace.

Use normal text asks when the context is small enough to paste directly. Slack currently routes text control messages only; it does not relay files.

## Setup

No separate attachment service is required. Start the daemon and use a surface that supports uploads:

- Dashboard compose bar.
- Telegram photos and documents.
- Relay-hosted dashboard uploads, tunneled back to the local daemon.

Files are stored under `~/.repowire/attachments/`.

## Common workflows

1. Upload a file through the dashboard compose bar or send a Telegram photo/document.
2. Repowire stores the attachment in the local attachment directory.
3. The outgoing ask or notify carries attachment metadata and a text fallback with the local path.
4. Ask the target agent to inspect that path with its normal file-reading or image-reading tool.

When Repowire sends a message with attachment metadata back to Telegram, the bot sends local image files as photos and other local files as documents when it can access the daemon-local path. If the file is not local to the bot, it falls back to a download link.

## Commands and API

- `POST /attachments` uploads a file to the daemon.
- `GET /attachments/{id}` downloads a retained attachment.
- Dashboard and Telegram call those routes for you.

Attachment metadata is carried with asks, notifications, and dashboard events; agents usually interact with the local path in the message body.

## Limits

- Uploads are limited to 10 MB.
- Attachments are retained for 24 hours.
- Repowire passes paths to agents; it does not force a runtime to inspect the file.
- Slack file relay is not part of the current Slack surface.
- Through the hosted relay, upload payloads tunnel back to the local daemon before the target agent sees the path.

## Troubleshooting

- Uploads fail from the remote dashboard: confirm relay access is connected and the local daemon is reachable.
- Telegram photos do not reach the agent: confirm the Telegram bot can reach the daemon and `/attachments` is not blocked by a proxy.
- The target agent cannot read the path: confirm the agent runs on the same machine or filesystem where the daemon stored the attachment.

## See also

- [Dashboard](dashboard.md)
- [Telegram](telegram.md)
- [HTTP API](../../reference/http-api.md)
