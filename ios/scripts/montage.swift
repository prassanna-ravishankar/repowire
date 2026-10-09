// Lays screenshots side by side in one PNG, for reviewing a whole variant at once.
// Usage: swift scripts/montage.swift <out.png> <image>...
import AppKit

let args = Array(CommandLine.arguments.dropFirst())
guard args.count >= 2 else {
    FileHandle.standardError.write(Data("usage: montage.swift <out.png> <image>...\n".utf8))
    exit(1)
}
let images = args.dropFirst().compactMap { NSImage(contentsOfFile: $0) }
let height: CGFloat = 1100, gap: CGFloat = 24
let widths = images.map { $0.size.width * height / $0.size.height }
let total = Int(widths.reduce(0, +) + gap * CGFloat(images.count + 1))
let rep = NSBitmapImageRep(bitmapDataPlanes: nil, pixelsWide: total, pixelsHigh: Int(height + gap * 2), bitsPerSample: 8, samplesPerPixel: 4, hasAlpha: true, isPlanar: false, colorSpaceName: .deviceRGB, bytesPerRow: 0, bitsPerPixel: 0)!
NSGraphicsContext.saveGraphicsState()
NSGraphicsContext.current = NSGraphicsContext(bitmapImageRep: rep)
NSColor(white: 0.55, alpha: 1).setFill()
NSRect(x: 0, y: 0, width: total, height: Int(height + gap * 2)).fill()
var x = gap
for (image, width) in zip(images, widths) {
    image.draw(in: NSRect(x: x, y: gap, width: width, height: height))
    x += width + gap
}
NSGraphicsContext.restoreGraphicsState()
try! rep.representation(using: .png, properties: [:])!.write(to: URL(fileURLWithPath: args[0]))
