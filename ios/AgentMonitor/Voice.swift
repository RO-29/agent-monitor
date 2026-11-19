import AVFoundation
import Speech
import SwiftUI

/// What to do with dictated text once the mic stops.
enum VoiceCleanup: String, CaseIterable, Identifiable {
    case always, ask, never
    var id: String { rawValue }
    var label: String {
        switch self {
        case .ask: return "Ask"
        case .always: return "Always"
        case .never: return "Never"
        }
    }
    static var current: VoiceCleanup { VoiceCleanup(rawValue: UserDefaults.standard.string(forKey: "voice.cleanup") ?? "") ?? .always }
}

/// GET /api/voice/status once per launch; a daemon without the route hides Clean up.
@MainActor
enum VoiceBridge {
    private(set) static var available: Bool?
    static func check() async -> Bool {
        if let a = available { return a }
        #if DEBUG
        if DebugRoute.sample { available = true; return true }
        #endif
        struct S: Decodable { var available: Bool?; var engine: String? }
        do {
            let s = try await APIClient.shared.get("/api/voice/status", as: S.self)
            available = s.available ?? false
        } catch let e as APIError where e.status == 404 || e.status == 405 {
            available = false
        } catch {
            return false
        }
        return available ?? false
    }

    struct Cleaned: Decodable { var text: String; var cleaned: Bool?; var ms: Int?; var error: String? }

    static func clean(_ text: String, mode: String? = nil) async throws -> Cleaned {
        var body: [String: Any] = ["text": text]
        if let mode { body["mode"] = mode }
        let data = try await APIClient.shared.post("/api/voice/clean", body: body, timeout: 35)
        return try JSONDecoder().decode(Cleaned.self, from: data)
    }
}

/// On-device speech to text (SFSpeechRecognizer + AVAudioEngine) with live partial results.
@MainActor
@Observable
final class Dictation {
    enum Phase: Equatable { case idle, listening, cleaning, review }

    var phase: Phase = .idle
    /// Text spoken in this take; `base` is the field text before the mic started.
    var spoken = ""
    var base = ""
    /// Segments iOS already closed; after a pause it reports only the new segment.
    private var kept = ""
    private var lastPartial = ""
    var cleaned: String?
    var error: String?
    var canClean = false

    private let engine = AVAudioEngine()
    private var request: SFSpeechAudioBufferRecognitionRequest?
    private var task: SFSpeechRecognitionTask?
    private let recognizer = SFSpeechRecognizer(locale: Locale.current) ?? SFSpeechRecognizer(locale: Locale(identifier: "en-US"))

    var listening: Bool { phase == .listening }
    /// The field text with this take joined to what was there.
    var joined: String { Self.join(base, spoken) }

    static func join(_ a: String, _ b: String) -> String {
        if a.isEmpty || b.isEmpty { return a + b }
        return a.hasSuffix(" ") || a.hasSuffix("\n") ? a + b : a + " " + b
    }

    func start(base: String) async {
        error = nil
        cleaned = nil
        guard await Self.authorize() else {
            error = "Allow Microphone and Speech Recognition for Agent Monitor in Settings."
            return
        }
        guard let recognizer, recognizer.isAvailable else {
            error = "Speech recognition is not available now."
            return
        }
        self.base = base
        spoken = ""
        kept = ""
        lastPartial = ""
        do {
            let session = AVAudioSession.sharedInstance()
            try session.setCategory(.record, mode: .measurement, options: .duckOthers)
            try session.setActive(true, options: .notifyOthersOnDeactivation)
            let req = SFSpeechAudioBufferRecognitionRequest()
            req.shouldReportPartialResults = true
            req.addsPunctuation = true
            if recognizer.supportsOnDeviceRecognition { req.requiresOnDeviceRecognition = true }
            request = req
            let input = engine.inputNode
            input.installTap(onBus: 0, bufferSize: 1024, format: input.outputFormat(forBus: 0)) { buf, _ in req.append(buf) }
            engine.prepare()
            try engine.start()
            phase = .listening
            task = recognizer.recognitionTask(with: req) { [weak self] result, err in
                let text = result?.bestTranscription.formattedString
                let final = result?.isFinal ?? false
                let segmentEnd = result?.speechRecognitionMetadata != nil
                Task { @MainActor in
                    guard let self else { return }
                    if let text { self.take(text, segmentEnd: segmentEnd && !final) }
                    if final || err != nil { self.finish() }
                }
            }
            canClean = await VoiceBridge.check()
        } catch {
            self.error = "The microphone did not start: \(error.localizedDescription)"
            teardown()
        }
    }

    /// Adds a result to the closed segments; a result that does not continue the last one starts a new segment.
    private func take(_ text: String, segmentEnd: Bool) {
        // Some recognizers report the whole dictation again; then nothing is kept apart.
        if !kept.isEmpty, text.hasPrefix(String(kept.prefix(12))) { kept = "" }
        let prev = lastPartial
        if !prev.isEmpty, !text.isEmpty, !text.hasPrefix(String(prev.prefix(8))), text.count < prev.count {
            kept = Self.join(kept, prev)
        }
        lastPartial = text
        spoken = Self.join(kept, text)
        if segmentEnd {
            kept = spoken
            lastPartial = ""
        }
    }

    /// Stops listening; the final result arrives through the recognition callback.
    func stop() {
        guard phase == .listening else { return }
        engine.stop()
        engine.inputNode.removeTap(onBus: 0)
        request?.endAudio()
        Task {
            try? await Task.sleep(nanoseconds: 1_200_000_000)
            if phase == .listening { finish() }
        }
    }

    private func finish() {
        guard phase == .listening else { return }
        teardown()
        let t = spoken.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !t.isEmpty, canClean else { phase = .idle; return }
        switch VoiceCleanup.current {
        case .never: phase = .idle
        case .ask: phase = .review
        case .always: Task { await clean() }
        }
    }

    private func teardown() {
        if engine.isRunning { engine.stop(); engine.inputNode.removeTap(onBus: 0) }
        task?.cancel()
        task = nil
        request = nil
        try? AVAudioSession.sharedInstance().setActive(false, options: .notifyOthersOnDeactivation)
    }

    /// Sends this take through the daemon's voice bridge and keeps the result for review.
    func clean() async {
        let t = spoken.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !t.isEmpty else { phase = .idle; return }
        phase = .cleaning
        do {
            let r = try await VoiceBridge.clean(t)
            if !(r.cleaned ?? false) {
                error = "The Mac did not clean this up" + ((r.error ?? "").isEmpty ? "." : ": \(r.error!)")
                phase = .idle
            } else {
                cleaned = r.text
                phase = .review
            }
        } catch let e as APIError where e.status == 413 {
            self.error = "Too long to clean up (over 8 KB)."
            phase = .idle
        } catch {
            self.error = "Clean up failed: \(error.localizedDescription)"
            phase = .idle
        }
    }

    func dismiss() { phase = .idle; cleaned = nil }

    private static func authorize() async -> Bool {
        let speech: Bool = await withCheckedContinuation { c in
            SFSpeechRecognizer.requestAuthorization { c.resume(returning: $0 == .authorized) }
        }
        guard speech else { return false }
        return await AVAudioApplication.requestRecordPermission()
    }

    #if DEBUG
    func sample(_ p: Phase) {
        base = ""
        spoken = "um so add a retry button to the upload chip and uh make it red when it fails"
        canClean = true
        phase = p
        if p == .review { cleaned = "Add a retry button to the upload chip, and make it red when the upload fails." }
    }
    #endif
}

/// The mic button: tap to talk, tap again to stop. Live text goes into `text` while it listens.
struct MicButton: View {
    @Bindable var dictation: Dictation
    @Binding var text: String
    var size: CGFloat = 44

    var body: some View {
        Button {
            Task {
                if dictation.listening { dictation.stop() } else { await dictation.start(base: text) }
            }
        } label: {
            ZStack {
                if dictation.listening {
                    Circle().fill(Tok.err.opacity(0.15)).frame(width: size - 6, height: size - 6)
                }
                Image(systemName: dictation.listening ? "stop.fill" : "mic")
                    .font(.system(size: dictation.listening ? 15 : 18, weight: .semibold))
                    .foregroundStyle(dictation.listening ? Tok.err : Tok.fg2)
                    .symbolEffect(.pulse, isActive: dictation.listening)
            }
            .frame(width: size, height: size)
            .contentShape(Circle())
        }
        .buttonStyle(.plain)
        .accessibilityLabel(dictation.listening ? "Stop dictation" : "Dictate")
        .onChange(of: dictation.spoken) { _, _ in if dictation.listening || dictation.phase == .idle { text = dictation.joined } }
        .sensoryFeedback(.impact(weight: .light), trigger: dictation.listening)
    }
}

/// After a take: Clean up (ask mode), the cleaned text for review, or an error.
struct DictationBar: View {
    @Bindable var dictation: Dictation
    @Binding var text: String

    var body: some View {
        Group {
            if let e = dictation.error {
                HStack(spacing: 8) {
                    Label(e, systemImage: "mic.slash").font(.footnote).foregroundStyle(Tok.err)
                    Spacer(minLength: 4)
                    Button { dictation.error = nil } label: { Image(systemName: "xmark").font(.caption.weight(.semibold)) }
                        .buttonStyle(.plain).foregroundStyle(Tok.muted).frame(width: 32, height: 32)
                        .accessibilityLabel("Dismiss")
                }
            } else {
                switch dictation.phase {
                case .listening:
                    HStack(spacing: 8) {
                        WorkingPulse()
                        Text("Listening. Tap stop when you finish.").font(.footnote).foregroundStyle(Tok.fg2)
                    }
                case .cleaning:
                    HStack(spacing: 8) {
                        ProgressView().controlSize(.small)
                        Text("Cleaning up on the Mac").font(.footnote).foregroundStyle(Tok.fg2)
                    }
                case .review:
                    review
                case .idle:
                    EmptyView()
                }
            }
        }
        .padding(.horizontal, 6)
        .animation(.snappy, value: dictation.phase)
    }

    @ViewBuilder private var review: some View {
        if let c = dictation.cleaned {
            VStack(alignment: .leading, spacing: 8) {
                Label("Cleaned up", systemImage: "wand.and.stars").font(.footnote.weight(.semibold)).foregroundStyle(Tok.accent)
                Text(c).font(.subheadline).foregroundStyle(Tok.fg).textSelection(.enabled)
                    .fixedSize(horizontal: false, vertical: true)
                    .frame(maxWidth: .infinity, alignment: .leading)
                HStack(spacing: 8) {
                    Button("Use This") {
                        // Text typed during the clean-up stays: swap only the spoken part.
                        let spoken = dictation.spoken
                        if text == dictation.joined {
                            text = Dictation.join(dictation.base, c)
                        } else if !spoken.isEmpty, let r = text.range(of: spoken) {
                            text.replaceSubrange(r, with: c)
                        } else {
                            text = Dictation.join(text, c)
                        }
                        dictation.dismiss()
                    }
                    .buttonStyle(.am(.accent))
                    Button("Keep Original") { dictation.dismiss() }.buttonStyle(.am(.ghost))
                }
            }
            .padding(10)
            .background(Tok.accentSoft, in: RoundedRectangle(cornerRadius: Radius.box))
        } else {
            HStack(spacing: 8) {
                Image(systemName: "waveform").foregroundStyle(Tok.muted)
                Text("Dictated").font(.footnote).foregroundStyle(Tok.fg2)
                Spacer()
                Button { Task { await dictation.clean() } } label: { Label("Clean Up", systemImage: "wand.and.stars") }
                    .buttonStyle(.am(.normal))
                Button { dictation.dismiss() } label: { Image(systemName: "xmark").font(.caption.weight(.semibold)) }
                    .buttonStyle(.plain).foregroundStyle(Tok.muted).frame(width: 32, height: 32)
                    .accessibilityLabel("Dismiss")
            }
        }
    }
}

/// A one-line answer field with its own mic, for "Other" answers in question cards.
struct VoiceField: View {
    let placeholder: String
    @Binding var text: String
    var disabled = false
    var onSubmit: () -> Void = {}
    @State private var dictation = Dictation()

    var body: some View {
        VStack(alignment: .leading, spacing: 6) {
            HStack(spacing: 4) {
                TextField(placeholder, text: $text, axis: .vertical)
                    .lineLimit(1...4)
                    .submitLabel(.send)
                    .onSubmit(onSubmit)
                MicButton(dictation: dictation, text: $text, size: 36)
            }
            .font(.body)
            .padding(.leading, 12).padding(.trailing, 2).padding(.vertical, 4)
            .frame(minHeight: 44)
            .background(Tok.panel2, in: RoundedRectangle(cornerRadius: 10))
            .overlay(RoundedRectangle(cornerRadius: 10).stroke(Tok.line, lineWidth: 1))
            .disabled(disabled)
            DictationBar(dictation: dictation, text: $text)
        }
    }
}
