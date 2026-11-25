import PhotosUI
import SwiftUI
import UIKit
import UniformTypeIdentifiers

// Files picked from Photos, Files or the camera; each uploads at once to POST /api/orch/uploads.
@MainActor
@Observable
final class AttachmentTray {
    struct Item: Identifiable {
        let id = UUID()
        var name: String
        var size: Int64
        var mime: String
        var thumb: UIImage?
        var progress: Double = 0
        var uploaded: OAttachment?
        var error: String?
        var started = false
    }

    static let maxBytes: Int64 = 100 << 20
    static let maxCount = 10
    static let parallel = 3

    var items: [Item] = []
    /// A driven thread id (ot_...) scopes the upload; watched sessions and new threads leave it nil.
    var threadID: String?
    private var tasks: [UUID: Task<Void, Never>] = [:]
    /// File bytes kept until the upload succeeds, so a failed one can retry.
    private var payloads: [UUID: Data] = [:]

    var isEmpty: Bool { items.isEmpty }
    var uploading: Bool { items.contains { $0.uploaded == nil && $0.error == nil } }
    var failed: Bool { items.contains { $0.error != nil } }
    /// Mean progress of the uploads still running, 0...1.
    var uploadProgress: Double {
        let p = items.filter { $0.uploaded == nil && $0.error == nil }.map(\.progress)
        return p.isEmpty ? 1 : p.reduce(0, +) / Double(p.count)
    }
    var ids: [String] { items.compactMap { $0.uploaded?.id } }
    var uploadedList: [OAttachment] { items.compactMap(\.uploaded) }

    /// Puts back attachments a saved draft had already uploaded; nothing uploads again.
    func restore(_ atts: [OAttachment]) {
        for a in atts where !items.contains(where: { $0.uploaded?.id == a.id }) {
            items.append(Item(name: a.name ?? "Attachment", size: a.size ?? 0, mime: a.mime, thumb: nil, progress: 1, uploaded: a, started: true))
        }
    }
    var uploadedCount: Int { items.filter { $0.uploaded != nil }.count }
    /// "3 of 5 uploaded" while uploads run or wait; nil when none do.
    var uploadSummary: String? {
        guard uploading else { return nil }
        return "\(uploadedCount) of \(items.filter { $0.uploaded != nil || $0.error == nil }.count) uploaded"
    }
    func canRetry(_ id: UUID) -> Bool { payloads[id] != nil && items.contains { $0.id == id && $0.error != nil } }
    var full: Bool { items.count >= Self.maxCount }

    func add(data: Data, name: String, mime: String, thumb: UIImage?) {
        guard !full else {
            ToastCenter.shared.show("At most \(Self.maxCount) attachments", icon: "exclamationmark.circle.fill")
            return
        }
        var it = Item(name: name, size: Int64(data.count), mime: mime, thumb: thumb)
        if it.size > Self.maxBytes {
            it.error = "Larger than 100 MB."
        }
        items.append(it)
        guard it.error == nil else { return }
        payloads[it.id] = data
        pump()
    }

    func retry(_ id: UUID) {
        guard canRetry(id) else { return }
        update(id) { $0.error = nil; $0.progress = 0; $0.started = false }
        pump()
    }

    /// Starts waiting uploads in order while fewer than `parallel` run.
    private func pump() {
        while tasks.count < Self.parallel,
              let it = items.first(where: { !$0.started && $0.uploaded == nil && $0.error == nil }),
              let data = payloads[it.id] {
            let id = it.id, name = it.name, mime = it.mime, tid = threadID
            update(id) { $0.started = true }
            tasks[id] = Task { [weak self] in
                do {
                    let up = try await APIClient.shared.upload(data: data, name: name, mime: mime, threadID: tid) { p in
                        Task { @MainActor in self?.update(id) { $0.progress = p } }
                    }
                    self?.update(id) { $0.uploaded = up; $0.progress = 1 }
                    self?.payloads[id] = nil
                } catch {
                    if Task.isCancelled { return }
                    self?.update(id) { $0.error = error.localizedDescription }
                }
                self?.tasks[id] = nil
                self?.pump()
            }
        }
    }

    func remove(_ id: UUID) {
        tasks[id]?.cancel()
        tasks[id] = nil
        payloads[id] = nil
        items.removeAll { $0.id == id }
        pump()
    }

    func clear() {
        for t in tasks.values { t.cancel() }
        tasks = [:]
        payloads = [:]
        items = []
    }

    private func update(_ id: UUID, _ f: (inout Item) -> Void) {
        guard let i = items.firstIndex(where: { $0.id == id }) else { return }
        f(&items[i])
    }

    #if DEBUG
    /// Local chips in every state for screenshots; nothing is uploaded.
    func addSamples() {
        let img = UIGraphicsImageRenderer(size: CGSize(width: 80, height: 80)).image { c in
            UIColor(red: 0.47, green: 0.62, blue: 0.86, alpha: 1).setFill(); c.fill(CGRect(x: 0, y: 0, width: 80, height: 80))
            UIColor(red: 0.95, green: 0.78, blue: 0.42, alpha: 1).setFill(); c.cgContext.fillEllipse(in: CGRect(x: 44, y: 12, width: 22, height: 22))
            UIColor(red: 0.29, green: 0.52, blue: 0.36, alpha: 1).setFill(); c.fill(CGRect(x: 0, y: 52, width: 80, height: 28))
        }
        items = [
            Item(name: "IMG_2041.jpg", size: 1_840_000, mime: "image/jpeg", thumb: img, progress: 1,
                 uploaded: OAttachment(id: "up_sample", mime: "image/jpeg", name: "IMG_2041.jpg", size: 1_840_000, url: "")),
            Item(name: "design-review.pdf", size: 6_300_000, mime: "application/pdf", thumb: nil, progress: 0.42),
            Item(name: "notes.md", size: 3_200, mime: "text/markdown", thumb: nil, progress: 1,
                 uploaded: OAttachment(id: "up_sample2", mime: "text/markdown", name: "notes.md", size: 3_200, url: "")),
            Item(name: "flow.png", size: 820_000, mime: "image/png", thumb: img, error: "The network connection was lost."),
            Item(name: "spec.txt", size: 12_000, mime: "text/plain", thumb: nil),
        ]
        items[1].started = true
        payloads[items[3].id] = Data()
    }
    #endif
}

/// MIME type for a picked file, by its type identifier or extension.
func mimeType(for url: URL) -> String {
    switch url.pathExtension.lowercased() {
    case "md", "markdown": return "text/markdown"
    case "csv": return "text/csv"
    case "json": return "application/json"
    case "txt", "log": return "text/plain"
    case "jpg", "jpeg": return "image/jpeg"
    case "heic", "heif": return "image/heic"
    default: return UTType(filenameExtension: url.pathExtension)?.preferredMIMEType ?? "application/octet-stream"
    }
}

func byteCount(_ n: Int64) -> String {
    ByteCountFormatter.string(fromByteCount: n, countStyle: .file)
}

// MARK: - Add button

/// The paperclip menu: Photos, Camera (when the device has one) and Files.
struct AttachButton: View {
    let tray: AttachmentTray
    var size: CGFloat = 44

    @State private var showPhotos = false
    @State private var showFiles = false
    @State private var showCamera = false
    @State private var picks: [PhotosPickerItem] = []
    private let hasCamera = UIImagePickerController.isSourceTypeAvailable(.camera)

    var body: some View {
        Menu {
            Button { showPhotos = true } label: { Label("Photo Library", systemImage: "photo.on.rectangle") }
            if hasCamera {
                Button { showCamera = true } label: { Label("Take Photo", systemImage: "camera") }
            }
            Button { showFiles = true } label: { Label("Choose File", systemImage: "folder") }
        } label: {
            Image(systemName: "plus")
                .font(.system(size: 17, weight: .semibold))
                .foregroundStyle(Tok.fg2)
                .frame(width: 34, height: 34)
                .background(Color(.tertiarySystemFill), in: Circle())
                .frame(width: size, height: size)
                .contentShape(Circle())
        }
        .disabled(tray.full)
        .accessibilityLabel("Attach")
        .accessibilityHint("Adds a photo, a picture from the camera, or a file")
        .photosPicker(isPresented: $showPhotos, selection: $picks, maxSelectionCount: AttachmentTray.maxCount, matching: .any(of: [.images, .videos]))
        .onChange(of: picks) { _, new in
            guard !new.isEmpty else { return }
            picks = []
            Task { for p in new { await addPhoto(p) } }
        }
        .sheet(isPresented: $showFiles) {
            DocumentPicker { urls in for u in urls { addFile(u) } }
                .ignoresSafeArea()
        }
        .fullScreenCover(isPresented: $showCamera) {
            CameraPicker { img in
                if let d = ImageShrink.jpeg(img) {
                    tray.add(data: d, name: "Photo \(Self.stamp()).jpg", mime: "image/jpeg", thumb: img)
                }
            }
            .ignoresSafeArea()
        }
    }

    private static func stamp() -> String {
        let f = DateFormatter()
        f.dateFormat = "yyyy-MM-dd HH.mm.ss"
        return f.string(from: Date())
    }

    /// Photos keep PNG, GIF and WebP; HEIC and the rest become JPEG so every agent can read them.
    private func addPhoto(_ p: PhotosPickerItem) async {
        guard let data = try? await p.loadTransferable(type: Data.self) else {
            ToastCenter.shared.show("Could not read the photo", icon: "exclamationmark.circle.fill")
            return
        }
        let types = p.supportedContentTypes
        if let movie = types.first(where: { $0.conforms(to: .movie) }) {
            let ext = movie.preferredFilenameExtension ?? "mov"
            tray.add(data: data, name: "Video \(Self.stamp()).\(ext)", mime: movie.preferredMIMEType ?? "video/quicktime", thumb: nil)
            return
        }
        let img = UIImage(data: data)
        let thumb = img?.preparingThumbnail(of: CGSize(width: 120, height: 120))
        let base = "Photo \(Self.stamp())"
        if types.contains(where: { $0.conforms(to: .png) }) {
            tray.add(data: img.flatMap(ImageShrink.png) ?? data, name: base + ".png", mime: "image/png", thumb: thumb)
        } else if types.contains(where: { $0.conforms(to: .gif) }) {
            tray.add(data: data, name: base + ".gif", mime: "image/gif", thumb: thumb)
        } else if types.contains(where: { $0.conforms(to: .webP) }) {
            tray.add(data: data, name: base + ".webp", mime: "image/webp", thumb: thumb)
        } else if types.contains(where: { $0.conforms(to: .jpeg) }) {
            tray.add(data: img.flatMap(ImageShrink.jpegIfLarge) ?? data, name: base + ".jpg", mime: "image/jpeg", thumb: thumb)
        } else if let jpg = img.flatMap({ ImageShrink.jpeg($0) }) {
            tray.add(data: jpg, name: base + ".jpg", mime: "image/jpeg", thumb: thumb)
        } else {
            tray.add(data: data, name: base + ".heic", mime: "image/heic", thumb: thumb)
        }
    }

    private func addFile(_ url: URL) {
        let scoped = url.startAccessingSecurityScopedResource()
        defer { if scoped { url.stopAccessingSecurityScopedResource() } }
        guard let data = try? Data(contentsOf: url) else {
            ToastCenter.shared.show("Could not read \(url.lastPathComponent)", icon: "exclamationmark.circle.fill")
            return
        }
        var mime = mimeType(for: url)
        var body = data
        var name = url.lastPathComponent
        var thumb: UIImage?
        if mime.hasPrefix("image/") {
            let img = UIImage(data: data)
            thumb = img?.preparingThumbnail(of: CGSize(width: 120, height: 120))
            if mime == "image/heic", let jpg = img.flatMap({ ImageShrink.jpeg($0) }) {
                body = jpg
                mime = "image/jpeg"
                name = (name as NSString).deletingPathExtension + ".jpg"
            } else if mime == "image/jpeg", let jpg = img.flatMap(ImageShrink.jpegIfLarge) {
                body = jpg
            } else if mime == "image/png", let png = img.flatMap(ImageShrink.png) {
                body = png
            }
        }
        tray.add(data: body, name: name, mime: mime, thumb: thumb)
    }
}

/// Photos go up at most 2000 px on the long edge: enough for an agent to read, and far smaller to upload.
enum ImageShrink {
    static let maxEdge: CGFloat = 2000

    static func scaled(_ img: UIImage) -> UIImage? {
        let w = img.size.width * img.scale, h = img.size.height * img.scale
        let long = max(w, h)
        guard long > maxEdge else { return nil }
        let k = maxEdge / long
        let size = CGSize(width: (w * k).rounded(), height: (h * k).rounded())
        let fmt = UIGraphicsImageRendererFormat()
        fmt.scale = 1
        return UIGraphicsImageRenderer(size: size, format: fmt).image { _ in img.draw(in: CGRect(origin: .zero, size: size)) }
    }

    /// JPEG at quality 0.85, downscaled when larger than maxEdge.
    static func jpeg(_ img: UIImage) -> Data? { (scaled(img) ?? img).jpegData(compressionQuality: 0.85) }

    /// A re-encoded JPEG only when the image is larger than maxEdge; nil keeps the original bytes.
    static func jpegIfLarge(_ img: UIImage) -> Data? { scaled(img)?.jpegData(compressionQuality: 0.85) }

    /// A downscaled PNG when larger than maxEdge; nil keeps the original bytes.
    static func png(_ img: UIImage) -> Data? { scaled(img)?.pngData() }
}

// MARK: - Chips

/// One row of attachment chips: thumbnail or icon, name, size or progress, and a remove button.
struct AttachmentChips: View {
    let tray: AttachmentTray

    var body: some View {
        ScrollView(.horizontal, showsIndicators: false) {
            HStack(spacing: 8) {
                ForEach(tray.items) { it in chip(it) }
            }
            .padding(.horizontal, 2)
            .padding(.vertical, 2)
        }
        // A fade on the trailing edge shows that more chips scroll in.
        .mask {
            HStack(spacing: 0) {
                Color.black
                LinearGradient(colors: [.black, .black.opacity(tray.items.count > 1 ? 0.15 : 1)], startPoint: .leading, endPoint: .trailing)
                    .frame(width: 28)
            }
        }
    }

    private func chip(_ it: AttachmentTray.Item) -> some View {
        HStack(spacing: 10) {
            ZStack {
                if let t = it.thumb {
                    Image(uiImage: t).resizable().scaledToFill()
                } else {
                    Tok.panel3
                    Image(systemName: icon(it.mime)).font(.system(size: 18)).foregroundStyle(Tok.fg2)
                }
                if it.uploaded == nil && it.error == nil && it.started {
                    Color.black.opacity(0.35)
                    ProgressView(value: max(it.progress, 0.02))
                        .progressViewStyle(RingStyle())
                        .frame(width: 22, height: 22)
                }
            }
            .frame(width: 40, height: 40)
            .clipShape(RoundedRectangle(cornerRadius: 8, style: .continuous))
            VStack(alignment: .leading, spacing: 2) {
                Text(it.name).font(.subheadline.weight(.medium)).foregroundStyle(Tok.fg).lineLimit(1).truncationMode(.middle)
                Group {
                    if let e = it.error {
                        Text(e).foregroundStyle(Tok.err).lineLimit(2)
                    } else if it.uploaded == nil && !it.started {
                        Text("Waiting").foregroundStyle(Tok.muted)
                    } else if it.uploaded == nil {
                        Text("Uploading \(Int(it.progress * 100))%").foregroundStyle(Tok.muted)
                    } else {
                        Text(byteCount(it.size)).foregroundStyle(Tok.muted)
                    }
                }
                .font(.caption)
            }
            .frame(maxWidth: it.error == nil ? 150 : 190, alignment: .leading)
            if tray.canRetry(it.id) {
                Button { tray.retry(it.id) } label: {
                    Text("Retry").font(.subheadline.weight(.semibold)).foregroundStyle(Tok.accent)
                        .frame(minWidth: 44, minHeight: 44)
                        .contentShape(Rectangle())
                }
                .buttonStyle(.plain)
                .accessibilityLabel("Retry \(it.name)")
            }
            Button { withAnimation(.snappy) { tray.remove(it.id) } } label: {
                Image(systemName: "xmark.circle.fill")
                    .font(.system(size: 20))
                    .symbolRenderingMode(.hierarchical)
                    .foregroundStyle(Tok.muted)
                    .frame(width: 32, height: 44)
                    .contentShape(Rectangle())
            }
            .buttonStyle(.plain)
            .accessibilityLabel("Remove \(it.name)")
        }
        .padding(.leading, 6).padding(.trailing, 2)
        .frame(height: 52)
        .background(Tok.panel2, in: RoundedRectangle(cornerRadius: 12, style: .continuous))
        .overlay(RoundedRectangle(cornerRadius: 12, style: .continuous).stroke(it.error != nil ? Tok.err.opacity(0.5) : Tok.line, lineWidth: 1))
        .accessibilityElement(children: .contain)
    }

    private func icon(_ mime: String) -> String {
        if mime == "application/pdf" { return "doc.richtext" }
        if mime == "text/csv" { return "tablecells" }
        if mime == "application/json" { return "curlybraces" }
        if mime.hasPrefix("image/") { return "photo" }
        return "doc.text"
    }
}

/// A small determinate progress ring.
private struct RingStyle: ProgressViewStyle {
    func makeBody(configuration: Configuration) -> some View {
        let f = configuration.fractionCompleted ?? 0
        ZStack {
            Circle().stroke(.white.opacity(0.35), lineWidth: 3)
            Circle().trim(from: 0, to: f).stroke(.white, style: StrokeStyle(lineWidth: 3, lineCap: .round)).rotationEffect(.degrees(-90))
        }
    }
}

// MARK: - UIKit pickers

struct DocumentPicker: UIViewControllerRepresentable {
    let onPick: ([URL]) -> Void

    static var types: [UTType] {
        [.item] // any file: the daemon keeps what it cannot read as text, an image or a pdf
    }

    func makeUIViewController(context: Context) -> UIDocumentPickerViewController {
        let vc = UIDocumentPickerViewController(forOpeningContentTypes: Self.types, asCopy: true)
        vc.allowsMultipleSelection = true
        vc.delegate = context.coordinator
        return vc
    }
    func updateUIViewController(_ vc: UIDocumentPickerViewController, context: Context) {}
    func makeCoordinator() -> Coordinator { Coordinator(onPick) }

    final class Coordinator: NSObject, UIDocumentPickerDelegate {
        let onPick: ([URL]) -> Void
        init(_ f: @escaping ([URL]) -> Void) { onPick = f }
        func documentPicker(_ controller: UIDocumentPickerViewController, didPickDocumentsAt urls: [URL]) { onPick(urls) }
    }
}

struct CameraPicker: UIViewControllerRepresentable {
    let onImage: (UIImage) -> Void
    @Environment(\.dismiss) private var dismiss

    func makeUIViewController(context: Context) -> UIImagePickerController {
        let vc = UIImagePickerController()
        vc.sourceType = .camera
        vc.delegate = context.coordinator
        return vc
    }
    func updateUIViewController(_ vc: UIImagePickerController, context: Context) {}
    func makeCoordinator() -> Coordinator { Coordinator(self) }

    final class Coordinator: NSObject, UIImagePickerControllerDelegate, UINavigationControllerDelegate {
        let parent: CameraPicker
        init(_ p: CameraPicker) { parent = p }
        func imagePickerController(_ picker: UIImagePickerController, didFinishPickingMediaWithInfo info: [UIImagePickerController.InfoKey: Any]) {
            if let img = info[.originalImage] as? UIImage { parent.onImage(img) }
            parent.dismiss()
        }
        func imagePickerControllerDidCancel(_ picker: UIImagePickerController) { parent.dismiss() }
    }
}
