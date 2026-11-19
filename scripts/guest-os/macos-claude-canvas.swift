// A geometry token describes only the signed Claude window actually captured.
// All coordinates used for presentation and input are measured in guest points.
import AppKit
import ScreenCaptureKit
import ImageIO
import UniformTypeIdentifiers

struct ClaudeCanvas {
    let id: CGWindowID
    let pid: pid_t
    let frame: CGRect
    let cropTop: CGFloat
    let scale: CGFloat
    let resizable: Bool
    var content: CGSize { CGSize(width: frame.width, height: frame.height - cropTop) }

    func matches(_ other: ClaudeCanvas) -> Bool {
        id == other.id && pid == other.pid && frame == other.frame && cropTop == other.cropTop && scale == other.scale
    }
}

private func axAttribute(_ element: AXUIElement, _ attribute: String) -> CFTypeRef? {
    var value: CFTypeRef?
    guard AXUIElementCopyAttributeValue(element, attribute as CFString, &value) == .success else { return nil }
    return value
}

private func axRect(_ element: AXUIElement) -> CGRect? {
    guard let p = axAttribute(element, kAXPositionAttribute), CFGetTypeID(p) == AXValueGetTypeID(),
          let s = axAttribute(element, kAXSizeAttribute), CFGetTypeID(s) == AXValueGetTypeID() else { return nil }
    var pos = CGPoint.zero, size = CGSize.zero
    guard AXValueGetValue(p as! AXValue, .cgPoint, &pos), AXValueGetValue(s as! AXValue, .cgSize, &size) else { return nil }
    return CGRect(origin: pos, size: size)
}

private func liveBounds(_ id: CGWindowID, _ pid: pid_t) throws -> CGRect {
    let windows = CGWindowListCopyWindowInfo(.optionIncludingWindow, id) as? [[String:Any]]
    guard let info = windows?.first, info[kCGWindowOwnerPID as String] as? Int32 == pid,
          info[kCGWindowIsOnscreen as String] as? Bool == true,
          let box = info[kCGWindowBounds as String] as? NSDictionary,
          let rect = CGRect(dictionaryRepresentation: box), !rect.isEmpty else { throw Failure(7, "window_changed") }
    return rect
}

// AXIsProcessTrusted asks tccd, which re-validates this helper's signature.
// Input and layout ask on every event; a one-second answer is current enough.
private final class AccessibilityTrust: @unchecked Sendable {
    static let shared = AccessibilityTrust()
    private let lock = NSLock()
    private var checked = Date.distantPast, trusted = false
    var current: Bool {
        lock.lock(); defer { lock.unlock() }
        if Date().timeIntervalSince(checked) >= 1 { trusted = AXIsProcessTrusted(); checked = Date() }
        return trusted
    }
}

func owningAXWindow(_ pid: pid_t, _ frame: CGRect) -> AXUIElement? {
    guard AccessibilityTrust.shared.current, NSWorkspace.shared.frontmostApplication?.processIdentifier == pid,
          let raw = axAttribute(AXUIElementCreateApplication(pid), kAXFocusedWindowAttribute),
          CFGetTypeID(raw) == AXUIElementGetTypeID() else { return nil }
    let window = raw as! AXUIElement
    guard let actual = axRect(window), abs(actual.minX-frame.minX)<1, abs(actual.minY-frame.minY)<1,
          abs(actual.width-frame.width)<1, abs(actual.height-frame.height)<1 else { return nil }
    return window
}

func macApplicationWindowBounds(_ pid: pid_t) -> [CGRect] {
    guard AccessibilityTrust.shared.current,
          let windows = axAttribute(AXUIElementCreateApplication(pid), kAXWindowsAttribute) as? [AXUIElement] else { return [] }
    return windows.compactMap(axRect)
}

private func canSet(_ window: AXUIElement, _ name: String) -> Bool {
    var result = DarwinBoolean(false)
    return AXUIElementIsAttributeSettable(window, name as CFString, &result) == .success && result.boolValue
}

private func titleBarHeight(_ window: AXUIElement, _ frame: CGRect) -> CGFloat {
    var bottom: CGFloat = 0
    for name in [kAXCloseButtonAttribute, kAXMinimizeButtonAttribute, kAXZoomButtonAttribute] {
        guard let raw = axAttribute(window, name), CFGetTypeID(raw) == AXUIElementGetTypeID(),
              let button = axRect(raw as! AXUIElement), button.width > 2, button.height > 2,
              frame.contains(button), button.minX-frame.minX < 100, button.maxY-frame.minY < 70 else { continue }
        let margin = min(8, max(2, button.minY - frame.minY))
        bottom = max(bottom, ceil(button.maxY - frame.minY + margin))
    }
    return bottom < frame.height / 3 ? bottom : 0
}

// JPEG has no alpha. A window's rounded corners are filled with the colour
// just inside its bottom edge instead of the encoder's black.
private func windowEdgeColor(_ image: CGImage) -> CGColor? {
    switch image.alphaInfo {
    case .none, .noneSkipFirst, .noneSkipLast: return nil
    default: break
    }
    guard image.width > 40, image.height > 40,
          let pixel = image.cropping(to: CGRect(x: 16, y: image.height - 17, width: 1, height: 1)),
          let context = CGContext(data: nil, width: 1, height: 1, bitsPerComponent: 8, bytesPerRow: 4,
                                  space: CGColorSpaceCreateDeviceRGB(),
                                  bitmapInfo: CGImageAlphaInfo.premultipliedLast.rawValue) else { return nil }
    context.draw(pixel, in: CGRect(x: 0, y: 0, width: 1, height: 1))
    guard let data = context.data else { return nil }
    let bytes = data.assumingMemoryBound(to: UInt8.self)
    guard bytes[3] == 255 else { return nil }
    return CGColor(red: CGFloat(bytes[0]) / 255, green: CGFloat(bytes[1]) / 255, blue: CGFloat(bytes[2]) / 255, alpha: 1)
}

// ImageIO compresses the whole window on the CPU. Callers run this off the
// main actor, which also delivers input to the same window. The window is
// sent as JPEG: it encodes and decodes several times faster than PNG and is
// the same kind of image a Linux guest sends, at that display's quality.
private func encodedWindowFrame(_ content: CGImage, phoneJPEG: Bool) throws -> (data: Data, mime: String) {
    if phoneJPEG { return (try phoneDesktopJPEG(content), "image/jpeg") }
    let edge = windowEdgeColor(content)
    var data = Data()
    // Preserve native glyph density and geometry even on photo-heavy windows.
    for quality in [0.92, 0.84] {
        var properties: [CFString: Any] = [kCGImageDestinationLossyCompressionQuality: quality]
        if let edge { properties[kCGImageDestinationBackgroundColor] = edge }
        data = try encodedImage(content, type: .jpeg, properties: properties as CFDictionary)
        if data.count < 8<<20 { break }
    }
    guard data.count < 8<<20 else { throw Failure(8, "frame_limit") }
    return (data, "image/jpeg")
}

@MainActor
final class ClaudeCanvasSession {
    static let shared = ClaudeCanvasSession()
    static let browser = ClaudeCanvasSession(browser: true)
    private let browser: Bool
    let stream: MacWindowStream
    init(browser: Bool = false) {
        self.browser = browser
        self.stream = browser ? MacWindowStream() : .shared
    }

    private func owns(_ pid: pid_t) throws -> Bool {
        if browser { return MacChromeWindow.shared.owns(pid) }
        return try claudePIDs().contains(pid)
    }

    private func cropHeight(_ ax: AXUIElement, _ rect: CGRect) -> CGFloat {
        browser ? 0 : titleBarHeight(ax, rect)
    }

    private var last: ClaudeCanvas?
    private var token = ""

    // Finding the window (ScreenCaptureKit enumeration) and laying it out
    // (Accessibility queries) dominate a frame on a virtual Mac. While the
    // window keeps its bounds and the view keeps its size, reuse them for a
    // short time; capture still verifies bounds and ownership on every frame.
    private struct Target {
        let window: SCWindow, filter: SCContentFilter, layout: ClaudeCanvas
        let width: Int, height: Int, scale: Double, time: Date
    }
    private var target: Target?
    private static let targetLifetime: TimeInterval = 2

    func cachedTarget(width: Int, height: Int, scale: Double) -> (SCWindow, SCContentFilter, ClaudeCanvas)? {
        guard let cached = target, cached.width == width, cached.height == height, cached.scale == scale,
              Date().timeIntervalSince(cached.time) < Self.targetLifetime,
              (try? liveBounds(cached.layout.id, cached.layout.pid)) == cached.layout.frame else {
            target = nil
            return nil
        }
        return (cached.window, cached.filter, cached.layout)
    }

    func remember(_ window: SCWindow, filter: SCContentFilter, layout: ClaudeCanvas, width: Int, height: Int, scale: Double) {
        target = Target(window: window, filter: filter, layout: layout, width: width, height: height, scale: scale, time: Date())
    }

    func forgetTarget() { target = nil }

    // Input reuses the window the frame stream already verified. Its geometry
    // and focus are still checked by inputGeometry before every delivery.
    func cachedWindow() -> SCWindow? {
        guard let cached = target, Date().timeIntervalSince(cached.time) < Self.targetLifetime,
              (try? liveBounds(cached.layout.id, cached.layout.pid)) == cached.layout.frame else { return nil }
        return cached.window
    }
    private var requested: (id: CGWindowID, pid: pid_t, size: CGSize, observed: CGSize, time: Date)?
    // Each accessibility query is a round trip into the application. What they
    // report follows the window's geometry, which every frame reads anyway.
    private var measured: (id: CGWindowID, pid: pid_t, bounds: CGRect, width: Int, height: Int, crop: CGFloat, resized: Bool, time: Date)?

    func layout(_ window: SCWindow, filter: SCContentFilter, width: Int, height: Int, scale: Double) async throws -> ClaudeCanvas {
        guard let pid = window.owningApplication?.processID,
              (320...2560).contains(width), (240...1600).contains(height),
              scale.isFinite, (1...2).contains(scale) else { throw Failure(8, "invalid_input") }
        var bounds = try liveBounds(window.windowID, pid)
        let density = min(max(1,CGFloat(filter.pointPixelScale)), CGFloat(scale))
        if let known = measured, known.id == window.windowID, known.pid == pid, known.bounds == bounds,
           known.width == width, known.height == height, Date().timeIntervalSince(known.time) < 1 {
            return ClaudeCanvas(id: window.windowID, pid: pid, frame: bounds, cropTop: known.crop,
                                scale: density, resizable: known.resized)
        }
        var crop: CGFloat = 0, resized = false
        if let ax = owningAXWindow(pid, bounds) {
            crop = cropHeight(ax, bounds)
            if canSet(ax, kAXSizeAttribute) {
                let display = CGDisplayBounds(CGMainDisplayID())
                let wanted = CGSize(width: min(CGFloat(width), display.width),
                                    height: min(CGFloat(height) + crop, display.height))
                let close = abs(bounds.width - wanted.width) < 2 && abs(bounds.height - wanted.height) < 2
                let recent = requested.map { $0.id == window.windowID && $0.pid == pid && $0.size == wanted &&
                    $0.observed == bounds.size && Date().timeIntervalSince($0.time) < 3 } ?? false
                if !close && !recent {
                    var size = wanted
                    if let value = AXValueCreate(.cgSize, &size),
                       AXUIElementSetAttributeValue(ax, kAXSizeAttribute as CFString, value) == .success {
                        try await Task.sleep(nanoseconds: 70_000_000)
                        bounds = try liveBounds(window.windowID, pid)
                        // Position only an enlarged guest window that would extend off its display.
                        var origin = CGPoint(x: max(display.minX, min(bounds.minX, display.maxX-bounds.width)),
                                             y: max(display.minY, min(bounds.minY, display.maxY-bounds.height)))
                        if origin != bounds.origin, canSet(ax, kAXPositionAttribute),
                           let point = AXValueCreate(.cgPoint, &origin) {
                            _ = AXUIElementSetAttributeValue(ax, kAXPositionAttribute as CFString, point)
                            bounds = try liveBounds(window.windowID, pid)
                        }
                    }
                    requested = (window.windowID, pid, wanted, bounds.size, Date())
                }
                resized = abs(bounds.width-wanted.width) < 2 && abs(bounds.height-wanted.height) < 2
                crop = cropHeight(ax, bounds)
            }
        }
        measured = (window.windowID, pid, bounds, width, height, crop, resized, Date())
        return ClaudeCanvas(id: window.windowID, pid: pid, frame: bounds, cropTop: crop,
                            scale: density, resizable: resized)
    }

    func record(_ value: ClaudeCanvas) -> String {
        if last?.matches(value) != true { token = UUID().uuidString; last = value }
        return token
    }

    func inputGeometry(_ requested: String, window: SCWindow) throws -> ClaudeCanvas {
        guard let last, token == requested, last.id == window.windowID, last.pid == window.owningApplication?.processID,
              try liveBounds(last.id, last.pid) == last.frame,
              let ax = owningAXWindow(last.pid, last.frame), cropHeight(ax,last.frame) == last.cropTop else {
            throw Failure(7, "window_changed")
        }
        return last
    }

    func capture(_ window: SCWindow, filter: SCContentFilter, layout: ClaudeCanvas, after: String? = nil, wait: Bool = false, binary: Bool = false, phoneJPEG: Bool = false) async throws -> [String:Any] {
        let config = SCStreamConfiguration()
        config.width = max(1, Int((layout.frame.width*layout.scale).rounded()))
        config.height = max(1, Int((layout.frame.height*layout.scale).rounded()))
        config.captureResolution = .best
        config.ignoreShadowsSingleWindow = true; config.showsCursor = false
        if #available(macOS 14.2, *) { config.includeChildWindows = browser }
        let geometry = record(layout)
        // Timings let the host separate waiting for a new frame, encoding
        // in the guest and the transport. They contain no window content.
        let captureStarted = Date()
        let (updated, sequence) = try await stream.frame(filter: filter, configuration: config, identity: geometry, after: after, wait: wait)
        let captureMs = Date().timeIntervalSince(captureStarted) * 1000
        guard try liveBounds(layout.id, layout.pid) == layout.frame,
              try owns(layout.pid) else { throw Failure(7, "window_changed") }
        guard let image = updated else { return ["unchanged": true, "sequence": sequence, "geometry": geometry] }
        let top = Int((layout.cropTop * CGFloat(image.height) / layout.frame.height).rounded())
        guard top >= 0, top < image.height,
              let content = image.cropping(to: CGRect(x:0,y:top,width:image.width,height:image.height-top)) else {
            throw Failure(7, "window_changed")
        }
        let encodeStarted = Date()
        let encoded = try await Task.detached(priority: .userInitiated) {
            try encodedWindowFrame(content, phoneJPEG: phoneJPEG)
        }.value
        let data = encoded.data, mime = encoded.mime
        // The second ownership check covers the time spent encoding.
        guard try liveBounds(layout.id, layout.pid) == layout.frame,
              try owns(layout.pid) else { throw Failure(7, "window_changed") }
        var reply: [String:Any] = ["window":layout.id,"pid":layout.pid,"mime":mime,"sequence":sequence,
                "contentWidth":layout.content.width,"contentHeight":layout.content.height,
                "pixelWidth":content.width,"pixelHeight":content.height,"cropTop":layout.cropTop,
                "resizable":layout.resizable, "geometry":geometry,
                "captureMs":captureMs.rounded(), "encodeMs":(Date().timeIntervalSince(encodeStarted) * 1000).rounded()]
        if binary { reply["imageBytes"] = data }
        else { reply["image"] = data.base64EncodedString() }
        return reply
    }
}
