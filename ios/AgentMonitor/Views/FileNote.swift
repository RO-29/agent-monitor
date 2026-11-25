import ImageIO
import QuickLook
import SwiftUI
import UIKit
import UniformTypeIdentifiers

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
    @Environment(\.chatAPI) private var api
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
                    VStack(spacing: 4) {
                        Image(systemName: failed ? "photo.badge.exclamationmark" : "photo").foregroundStyle(failed ? Tok.err : Tok.muted)
                        if failed { Text("Not loaded. Tap to open.").font(.caption2).foregroundStyle(Tok.fg2) }
                    }
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
        guard var req = try? URLRequest(url: api.url(att.url)) else { failed = true; return }
        api.authorize(&req)
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

/// Tap to open an attachment in Quick Look (image, video, audio, PDF, Office); it downloads to a temporary file first.
struct OpenAttachment<Content: View>: View {
    let att: OAttachment
    @ViewBuilder var content: () -> Content
    @Environment(\.chatAPI) private var api
    @State private var file: URL?
    @State private var loading = false
    @State private var failed = false

    var body: some View {
        Button { Task { await open() } } label: {
            content()
                .overlay { if loading { ProgressView().controlSize(.small) } }
                .overlay(alignment: .bottomLeading) {
                    if failed {
                        Label("Could not open. Tap to try again.", systemImage: "exclamationmark.triangle.fill")
                            .font(.caption2.weight(.semibold)).foregroundStyle(Tok.err)
                            .padding(.horizontal, 6).padding(.vertical, 3)
                            .background(.regularMaterial, in: Capsule()).padding(4)
                    }
                }
        }
        .buttonStyle(.plain)
        .disabled(loading)
        .quickLookPreview($file)
        .accessibilityHint("Opens the file")
    }

    private func open() async {
        guard var req = try? URLRequest(url: api.url(att.url)) else { failed = true; return }
        api.authorize(&req)
        loading = true
        failed = false
        defer { loading = false }
        do {
            let (tmp, resp) = try await URLSession.shared.download(for: req)
            guard (resp as? HTTPURLResponse)?.statusCode == 200 else { failed = true; return }
            let fm = FileManager.default
            let dir = fm.temporaryDirectory.appendingPathComponent("att-" + att.id, isDirectory: true)
            try? fm.removeItem(at: dir)
            try fm.createDirectory(at: dir, withIntermediateDirectories: true)
            let dest = dir.appendingPathComponent(Self.fileName(att))
            try fm.moveItem(at: tmp, to: dest)
            file = dest
        } catch {
            failed = true
        }
    }

    /// The original name without path parts; Quick Look needs an extension to pick a viewer.
    static func fileName(_ a: OAttachment) -> String {
        var n = (a.name ?? "").components(separatedBy: CharacterSet(charactersIn: "/\\:")).last ?? ""
        n = n.trimmingCharacters(in: .whitespacesAndNewlines)
        if n.isEmpty || n.hasPrefix(".") { n = "attachment" + n }
        if (n as NSString).pathExtension.isEmpty, let ext = UTType(mimeType: a.mime)?.preferredFilenameExtension {
            n += "." + ext
        }
        return n
    }
}

/// The server a chat's attachments come from: the thread's own Mac, through the primary's proxy for a peer.
private struct ChatAPIKey: EnvironmentKey {
    static let defaultValue: APIClient = .shared
}

extension EnvironmentValues {
    var chatAPI: APIClient {
        get { self[ChatAPIKey.self] }
        set { self[ChatAPIKey.self] = newValue }
    }
}
