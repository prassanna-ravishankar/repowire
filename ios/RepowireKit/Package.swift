// swift-tools-version: 6.0
import PackageDescription

let package = Package(
    name: "RepowireKit",
    platforms: [.iOS(.v18), .macOS(.v15)],
    products: [.library(name: "RepowireKit", targets: ["RepowireKit"])],
    targets: [
        .target(name: "RepowireKit"),
        .testTarget(name: "RepowireKitTests", dependencies: ["RepowireKit"]),
    ]
)
