import RepowireKit
import UIKit
import UserNotifications

enum DeviceName {
    static var current: String { UIDevice.current.name }
}

/// Notification categories mirror the daemon's push categories
/// (`daemon-go/push`): APPROVAL and QUESTION carry actions that answer in place.
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
            UNNotificationCategory(identifier: approval, actions: [
                UNNotificationAction(identifier: Action.allow, title: "Allow", options: [.authenticationRequired]),
                UNNotificationAction(identifier: Action.deny, title: "Deny", options: [.destructive, .authenticationRequired]),
            ], intentIdentifiers: []),
            UNNotificationCategory(identifier: question, actions: [
                UNTextInputNotificationAction(identifier: Action.reply, title: "Reply", options: [.authenticationRequired], textInputButtonTitle: "Send", textInputPlaceholder: "Answer"),
            ], intentIdentifiers: []),
            UNNotificationCategory(identifier: message, actions: [], intentIdentifiers: []),
        ]
    }
}

/// Bridges UIKit's push callbacks into the app model.
final class AppDelegate: NSObject, UIApplicationDelegate, UNUserNotificationCenterDelegate {
    var model: AppModel?

    func application(_ application: UIApplication, didFinishLaunchingWithOptions launchOptions: [UIApplication.LaunchOptionsKey: Any]? = nil) -> Bool {
        let center = UNUserNotificationCenter.current()
        center.delegate = self
        center.setNotificationCategories(PushCategory.all)
        return true
    }

    func application(_ application: UIApplication, didRegisterForRemoteNotificationsWithDeviceToken deviceToken: Data) {
        let token = deviceToken.map { String(format: "%02x", $0) }.joined()
        Task { await model?.register(token: token) }
    }

    func application(_ application: UIApplication, didFailToRegisterForRemoteNotificationsWithError error: Error) {
        model?.lastError = "Notifications unavailable: \(error.localizedDescription)"
    }

    nonisolated func userNotificationCenter(_ center: UNUserNotificationCenter, willPresent notification: UNNotification) async -> UNNotificationPresentationOptions {
        [.banner, .list, .sound]
    }

    nonisolated func userNotificationCenter(_ center: UNUserNotificationCenter, didReceive response: UNNotificationResponse) async {
        let info = response.notification.request.content.userInfo["repowire"] as? [String: Any] ?? [:]
        guard let cid = info["correlation_id"] as? String else { return }
        let options = (info["options"] as? [[String: Any]] ?? []).compactMap { $0["id"] as? String }
        let reply = (response as? UNTextInputNotificationResponse)?.userText
        let action = response.actionIdentifier
        await MainActor.run {
            guard let model = self.model else { return }
            Task {
                switch action {
                case PushCategory.Action.allow:
                    await model.answer(correlationId: cid, optionId: options.first, outcome: nil, text: nil)
                case PushCategory.Action.deny:
                    await model.answer(correlationId: cid, optionId: nil, outcome: "denied", text: nil)
                case PushCategory.Action.reply:
                    await model.answer(correlationId: cid, optionId: nil, outcome: nil, text: reply)
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
