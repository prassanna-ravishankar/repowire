# iOS app

## What it is

The Repowire iOS app is a native client for your mesh. It connects through the relay and sends as `@dashboard`, so agents treat what you send as direct user instructions. It is also a surface of the [human inbox](../../concepts/human-inbox.md), named `ios`: messages to the human reach your phone as notifications, and reading or replying in the app marks them on every surface.

From the app you can:

- Answer questions and approve or deny tool calls; from the lock screen too when previews are on.
- Read the human inbox and reply to it, with read state shared with Telegram and the dashboard.
- Browse peers by circle and read each peer's recent conversation.
- Send a peer an ask (expects an ack) or a notification (fire and forget).
- Follow running and recent jobs.

## Install

The app is not on the App Store yet. Build it onto your iPhone from Xcode: see [Install on your iPhone](../../contributing/ios-app.md#install-on-your-iphone).

## Setup

1. Enable the relay on the machine running your agents:

    ```bash
    repowire setup --relay
    ```

2. Copy `relay.api_key` from `~/.repowire/config.yaml`.
3. In the app, enter the relay host (`relay.repowire.io` for the hosted relay) and paste the key. The app checks the key against the relay before saving it to the Keychain.

To receive notifications, open **Settings → Turn on notifications**. The app asks for permission there, not at launch. Once permitted, it re-registers its device token on every launch, which keeps the relay's record of whose phone it is current.

## Notifications

Every item stored in the human inbox is pushed to each registered install, while the relay is connected:

| Item | Notification | Actions with `preview: true` |
| --- | --- | --- |
| Tool permission request | `@peer needs approval` | Allow, Deny |
| Ask or question | `@peer asks` | Reply |
| Reply to an ask you sent | `@peer replied` | Opens the app |
| Notify | `@peer` | Opens the app |

By default a notification is content-free: it names the sender and the kind of item, and nothing an agent wrote leaves the mesh for Apple or your lock screen. Tapping it opens the app, which reads the item through the relay. You approve or answer from the app, where you can see what you are approving. To put up to 240 characters of the text in the notification and get the lock-screen actions above, set:

```yaml
human:
  surfaces:
    ios:
      preview: true
```

Broadcasts reach the phone only with `human.broadcast: true` and `human.surfaces.ios.broadcast: true`. Each push's outcome is on the message's trace (`surface_pushed` or `surface_failed`, `surface: ios`).

Push requires the relay to be configured for APNs; see [Relay](../../operate/relay.md#push-notifications). If it is not, each push records `surface_failed` and the app still works in the foreground.

## Disconnecting

**Settings → Disconnect** removes the key from the device. The daemon and agents keep running. The device's push token stays registered until APNs reports it invalid, which removes it automatically. To remove it sooner:

```bash
curl -s http://127.0.0.1:8377/push/devices
curl -s -X DELETE http://127.0.0.1:8377/push/devices/<token>
```

## Related

- [Relay access](relay-access.md)
- [Dashboard](dashboard.md)
