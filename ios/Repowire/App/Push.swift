import RepowireKit
import UIKit
import UserNotifications

enum DeviceName {
    static var current: String { UIDevice.current.name }
}

/// Notification categories mirror the daemon's inbox surface (`daemon-go/push`):
/// APPROVAL and QUESTION carry actions that answer in place.
enum PushCategory {
    static let approval = "APPROVAL"
    static let question = "QUESTION"
    static let message = "MESSAGE"

    enum Action {
        static let allow = "ALLOW"
        static let deny = "DENY"
        static let reply = "REPLY"
    }

    static var all: Set<UNNotificationCategory> {
        [
            UNNotificationCategory(
                identifier: approval,
                actions: [
                    UNNotificationAction(identifier: Action.allow, title: "Allow", options: [.authenticationRequired]),
                    UNNotificationAction(identifier: Action.deny, title: "Deny", options: [.destructive, .authenticationRequired]),
                ], intentIdentifiers: []),
            UNNotificationCategory(
                identifier: question,
                actions: [
                    UNTextInputNotificationAction(
                        identifier: Action.reply, title: "Reply", options: [.authenticationRequired], textInputButtonTitle: "Send",
                        textInputPlaceholder: "Answer")
                ], intentIdentifiers: []),
            UNNotificationCategory(identifier: message, actions: [], intentIdentifiers: []),
        ]
    }
}

/// Bridges UIKit's push callbacks into the app model.
final class AppDelegate: NSObject, UIApplicationDelegate, UNUserNotificationCenterDelegate {
    var model: AppModel?

    func application(
        _ application: UIApplication, didFinishLaunchingWithOptions launchOptions: [UIApplication.LaunchOptionsKey: Any]? = nil
    ) -> Bool {
        let center = UNUserNotificationCenter.current()
        center.delegate = self
        center.setNotificationCategories(PushCategory.all)
        // Re-register on every launch once permitted: Apple may rotate the
        // token, and the relay binds a token to the key that last registered it.
        Task {
            if await Notifications.status() == .authorized {
                await MainActor.run { application.registerForRemoteNotifications() }
            }
        }
        return true
    }

    func application(_ application: UIApplication, didRegisterForRemoteNotificationsWithDeviceToken deviceToken: Data) {
        let token = deviceToken.map { String(format: "%02x", $0) }.joined()
        Task { await model?.register(token: token) }
    }

    func application(_ application: UIApplication, didFailToRegisterForRemoteNotificationsWithError error: Error) {
        model?.lastError = "Notifications unavailable: \(error.localizedDescription)"
    }

    nonisolated func userNotificationCenter(_ center: UNUserNotificationCenter, willPresent notification: UNNotification) async
        -> UNNotificationPresentationOptions
    {
        [.banner, .list, .sound]
    }

    nonisolated func userNotificationCenter(_ center: UNUserNotificationCenter, didReceive response: UNNotificationResponse) async {
        let info = response.notification.request.content.userInfo["repowire"] as? [String: Any] ?? [:]
        let itemId = info["item_id"] as? String
        let cid = info["correlation_id"] as? String
        let options = (info["options"] as? [[String: Any]] ?? []).compactMap { $0["id"] as? String }
        let reply = (response as? UNTextInputNotificationResponse)?.userText ?? ""
        let action = response.actionIdentifier
        await MainActor.run {
            guard let model = self.model else { return }
            Task {
                switch (action, cid, itemId) {
                case (PushCategory.Action.allow, let cid?, _):
                    await model.answer(correlationId: cid, optionId: options.first, outcome: nil, text: nil)
                case (PushCategory.Action.deny, let cid?, _):
                    await model.answer(correlationId: cid, optionId: nil, outcome: "denied", text: nil)
                case (PushCategory.Action.reply, _, let itemId?) where !reply.isEmpty:
                    // The inbox reply acks a plain ask, answers a free-text
                    // question, and marks the item replied everywhere.
                    await model.reply(itemId: itemId, text: reply)
                case (UNNotificationDefaultActionIdentifier, _, let itemId?):
                    await model.markRead(itemId: itemId)
                default:
                    break
                }
            }
        }
    }
}

enum Notifications {
    /// Asks once, at the moment the user turns notifications on in Settings.
    static func enable() async -> Bool {
        let granted = (try? await UNUserNotificationCenter.current().requestAuthorization(options: [.alert, .sound, .badge])) ?? false
        if granted { UIApplication.shared.registerForRemoteNotifications() }
        return granted
    }

    static func status() async -> UNAuthorizationStatus {
        await UNUserNotificationCenter.current().notificationSettings().authorizationStatus
    }
}
