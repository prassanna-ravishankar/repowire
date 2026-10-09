// Renders the App Store icon from the brand mark: cobalt mark on warm paper,
// cropped to the mark's drawn bounds so SVG padding does not shrink it.
// Also writes the cropped mark (transparent) for in-app use.
// Usage: swift scripts/render-icon.swift <logo.svg> <icon.png> <mark.png>
import AppKit

let args = CommandLine.arguments
guard args.count == 4, let mark = NSImage(contentsOfFile: args[1]) else {
    FileHandle.standardError.write(Data("usage: render-icon.swift <logo.svg> <icon.png> <mark.png>\n".utf8))
    exit(1)
}

func bitmap(_ side: Int, draw: () -> Void) -> NSBitmapImageRep {
    let rep = NSBitmapImageRep(bitmapDataPlanes: nil, pixelsWide: side, pixelsHigh: side, bitsPerSample: 8, samplesPerPixel: 4, hasAlpha: true, isPlanar: false, colorSpaceName: .deviceRGB, bytesPerRow: 0, bitsPerPixel: 0)!
    NSGraphicsContext.saveGraphicsState()
    NSGraphicsContext.current = NSGraphicsContext(bitmapImageRep: rep)
    draw()
    NSGraphicsContext.restoreGraphicsState()
    return rep
}

// 1. Rasterize the mark large and find its opaque bounds.
let probeSide = 2048
let probe = bitmap(probeSide) { mark.draw(in: NSRect(x: 0, y: 0, width: probeSide, height: probeSide)) }
var minX = probeSide, minY = probeSide, maxX = 0, maxY = 0
for y in 0..<probeSide { for x in 0..<probeSide where probe.colorAt(x: x, y: y)!.alphaComponent > 0.1 {
    minX = min(minX, x); maxX = max(maxX, x); minY = min(minY, y); maxY = max(maxY, y)
} }
// colorAt uses a top-left origin; drawing uses bottom-left.
let crop = NSRect(x: minX, y: probeSide - maxY - 1, width: maxX - minX + 1, height: maxY - minY + 1)
let markImage = NSImage(size: crop.size, flipped: false) { _ in
    probe.draw(in: NSRect(origin: .zero, size: crop.size), from: crop, operation: .sourceOver, fraction: 1, respectFlipped: false, hints: nil)
    return true
}

// 2. Compose the icon: warm paper, mark at 58% of the shorter side.
let side = 1024
let icon = bitmap(side) {
    NSColor(red: 0xFC / 255, green: 0xFB / 255, blue: 0xFA / 255, alpha: 1).setFill()
    NSRect(x: 0, y: 0, width: side, height: side).fill()
    let scale = CGFloat(side) * 0.58 / max(crop.width, crop.height)
    let size = NSSize(width: crop.width * scale, height: crop.height * scale)
    markImage.draw(in: NSRect(x: (CGFloat(side) - size.width) / 2, y: (CGFloat(side) - size.height) / 2, width: size.width, height: size.height))
}
try! icon.representation(using: .png, properties: [:])!.write(to: URL(fileURLWithPath: args[2]))

let markSide = 600
let markOut = bitmap(markSide) {
    let scale = CGFloat(markSide) / max(crop.width, crop.height)
    let size = NSSize(width: crop.width * scale, height: crop.height * scale)
    markImage.draw(in: NSRect(x: (CGFloat(markSide) - size.width) / 2, y: (CGFloat(markSide) - size.height) / 2, width: size.width, height: size.height))
}
try! markOut.representation(using: .png, properties: [:])!.write(to: URL(fileURLWithPath: args[3]))
