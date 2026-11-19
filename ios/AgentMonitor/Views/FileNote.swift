import ImageIO
import SwiftUI
import UIKit

// Reads the "Attached files" note (and older @path lines) that agent-monitor appends for terminal sessions.
enum FileNote {
    static let head = "Attached files (open them with your file tools):"
    private static let line = try! NSRegularExpression(
        pattern: #"^(?:- |@)(/\S*/uploads/[\w-]+/(up_[0-9a-f]{8,})\.(\w+))(?: \((.*), ([\w.+-]+/[\w.+-]+), [^,()]+\))?\s*$"#)
    private static let mimes = ["png": "image/png", "jpg": "image/jpeg", "jpeg": "image/jpeg", "webp": "image/webp", "gif": "image/gif",
                                "heic": "image/heic", "pdf": "application/pdf", "txt": "text/plain", "md": "text/markdown", "csv": "text/csv", "json": "application/json"]

    static func split(_ text: String) -> (text: String, files: [OAttachment]) {
        guard text.contains("/uploads/") else { return (text, []) }
        var keep: [String] = []
        var files: [OAttachment] = []
        for raw in text.components(separatedBy: "\n") {
            let ln = raw.trimmingCharacters(in: .whitespaces)
            if ln == head { continue }
            let ns = ln as NSString
            guard let m = line.firstMatch(in: ln, range: NSRange(location: 0, length: ns.length)) else { keep.append(raw); continue }
            func group(_ i: Int) -> String? { m.range(at: i).location == NSNotFound ? nil : ns.substring(with: m.range(at: i)) }
            let id = group(2) ?? "", ext = (group(3) ?? "").lowercased()
            files.append(OAttachment(id: id, mime: group(5) ?? mimes[ext] ?? "application/octet-stream",
                                     name: group(4) ?? "\(id).\(ext)", size: nil, url: "/api/orch/uploads/\(id)"))
        }
        if files.isEmpty { return (text, []) }
        return (keep.joined(separator: "\n").trimmingCharacters(in: .whitespacesAndNewlines), files)
    }
}

// A thumbnail for an uploaded image, loaded with the daemon credential and kept in a shared cache by upload id.
struct UploadThumb: View {
    let att: OAttachment
    @State private var image: UIImage?
    @State private var failed = false

    private static let cache: NSCache<NSString, UIImage> = {
        let c = NSCache<NSString, UIImage>()
        c.countLimit = 200
        return c
    }()

    var body: some View {
        Group {
            if let img = image ?? Self.cache.object(forKey: att.id as NSString) {
                Image(uiImage: img).resizable().scaledToFill()
            } else {
                Tok.panel2.overlay {
                    Image(systemName: failed ? "photo.badge.exclamationmark" : "photo").foregroundStyle(Tok.muted)
                }
            }
        }
        .frame(width: 132, height: 99)
        .clipShape(RoundedRectangle(cornerRadius: 10))
        .overlay(RoundedRectangle(cornerRadius: 10).stroke(Tok.line, lineWidth: 0.5))
        .accessibilityLabel(att.name ?? "Attached image")
        .task(id: att.id) { await load() }
    }

    private func load() async {
        let key = att.id as NSString
        if let hit = Self.cache.object(forKey: key) { image = hit; return }
        guard var req = try? URLRequest(url: APIClient.shared.url(att.url)) else { return }
        APIClient.authorize(&req)
        do {
            let (data, resp) = try await URLSession.shared.data(for: req)
            guard (resp as? HTTPURLResponse)?.statusCode == 200,
                  let img = await Task.detached(priority: .utility, operation: { Self.thumbnail(data) }).value else { failed = true; return }
            Self.cache.setObject(img, forKey: key)
            image = img
        } catch {
            failed = true
        }
    }

    /// Decodes only a downsampled image (300 px on the long edge), never the full bitmap.
    nonisolated static func thumbnail(_ data: Data) -> UIImage? {
        guard let src = CGImageSourceCreateWithData(data as CFData, [kCGImageSourceShouldCache: false] as CFDictionary) else { return nil }
        let opts: [CFString: Any] = [kCGImageSourceCreateThumbnailFromImageAlways: true,
                                     kCGImageSourceCreateThumbnailWithTransform: true,
                                     kCGImageSourceShouldCacheImmediately: true,
                                     kCGImageSourceThumbnailMaxPixelSize: 300]
        guard let cg = CGImageSourceCreateThumbnailAtIndex(src, 0, opts as CFDictionary) else { return nil }
        return UIImage(cgImage: cg)
    }
}
