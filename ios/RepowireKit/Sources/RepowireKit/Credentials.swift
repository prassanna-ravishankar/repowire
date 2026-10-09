import Foundation
import Security

/// Relay connection details. The key lives in the Keychain; the URL is not secret.
public struct Credentials: Codable, Equatable, Sendable {
    // swift-format-ignore: NeverForceUnwrap
    // A constant, valid URL literal; failing to parse it is a programmer error.
    public static let defaultRelay = URL(string: "https://relay.repowire.io")!

    public var relayURL: URL
    public var key: String

    public init(relayURL: URL = Credentials.defaultRelay, key: String) {
        self.relayURL = relayURL
        self.key = key
    }

    /// Relay keys are minted as `rw_` bearer secrets; reject anything else early.
    public static func isPlausibleKey(_ key: String) -> Bool {
        let trimmed = key.trimmingCharacters(in: .whitespacesAndNewlines)
        return trimmed.hasPrefix("rw_") && trimmed.count >= 10 && !trimmed.contains(" ")
    }

    /// Accepts a bare host ("relay.example.com") as well as a full URL.
    public static func normalizedRelayURL(_ raw: String) -> URL? {
        var text = raw.trimmingCharacters(in: .whitespacesAndNewlines)
        while text.hasSuffix("/") { text.removeLast() }
        if !text.contains("://") { text = "https://" + text }
        guard let url = URL(string: text), let scheme = url.scheme, ["https", "http"].contains(scheme), url.host() != nil else {
            return nil
        }
        return url
    }
}

/// Keychain persistence for `Credentials`, one generic-password item.
public struct CredentialStore: Sendable {
    let service: String

    public init(service: String = "io.repowire.app.relay") {
        self.service = service
    }

    public func load() -> Credentials? {
        let query: [String: Any] = [
            kSecClass as String: kSecClassGenericPassword,
            kSecAttrService as String: service,
            kSecReturnData as String: true,
            kSecMatchLimit as String: kSecMatchLimitOne,
        ]
        var result: AnyObject?
        guard SecItemCopyMatching(query as CFDictionary, &result) == errSecSuccess, let data = result as? Data else { return nil }
        return try? JSONDecoder().decode(Credentials.self, from: data)
    }

    @discardableResult
    public func save(_ credentials: Credentials) -> Bool {
        clear()
        guard let data = try? JSONEncoder().encode(credentials) else { return false }
        let item: [String: Any] = [
            kSecClass as String: kSecClassGenericPassword,
            kSecAttrService as String: service,
            kSecAttrAccessible as String: kSecAttrAccessibleAfterFirstUnlockThisDeviceOnly,
            kSecValueData as String: data,
        ]
        return SecItemAdd(item as CFDictionary, nil) == errSecSuccess
    }

    public func clear() {
        SecItemDelete([kSecClass as String: kSecClassGenericPassword, kSecAttrService as String: service] as CFDictionary)
    }
}
