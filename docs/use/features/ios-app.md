# iOS app

## What it is

The Repowire iOS app is a native client for your mesh. It connects through the relay as `@dashboard`, the same human surface as the web dashboard, so agents treat what you send as direct user instructions.

From the app you can:

- Answer questions and approve or deny tool calls, including from the lock screen.
- Browse peers by circle and read each peer's recent conversation.
- Send a peer an ask (expects an ack) or a notification (fire and forget).
- Follow running and recent jobs.

## Setup

1. Enable the relay on the machine running your agents:

    ```bash
    repowire setup --relay
    ```

2. Copy `relay.api_key` from `~/.repowire/config.yaml`.
3. In the app, enter the relay host (`repowire.io` for the hosted relay) and paste the key. The app checks the key against the relay before saving it to the Keychain.

To receive notifications, open **Settings → Turn on notifications**. The app asks for permission there, not at launch.

## Notifications

The daemon sends a push when an event is addressed to you (`human` or `@dashboard`):

| Event | Notification | Actions |
| --- | --- | --- |
| Tool permission request | `@peer needs approval` | Allow, Deny |
| Question or ask | `@peer asks` | Reply |
| Notification | `@peer` | Opens the app |

Push requires the relay to be configured for APNs; see [Relay](../../operate/relay.md#push-notifications). If the relay is not configured, the daemon logs `push failed: push is not configured on this relay` and the app still works in the foreground.

## Disconnecting

**Settings → Disconnect** removes the key from the device. The daemon and agents keep running. The device's push token stays registered until APNs reports it invalid, which removes it automatically. To remove it sooner:

```bash
curl -s http://127.0.0.1:8377/push/devices
curl -s -X DELETE http://127.0.0.1:8377/push/devices/<token>
```

## Related

- [Relay access](relay-access.md)
- [Dashboard](dashboard.md)
