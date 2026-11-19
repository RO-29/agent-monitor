// The Approvals window (/approvals): a panel above every window on every Space that shows itself when an agent waits for you.
import Carbon
import Cocoa
import WebKit

let kApprovalsHotkeyLabel = "⌃⌥Space"

// A borderless panel must opt in to key status, or the web view gets no typing; it never becomes main, so it joins all Spaces.
final class ApprovalsPanel: NSPanel {
    var onClose: (() -> Void)?
    override var canBecomeKey: Bool { true }
    override var canBecomeMain: Bool { false }
    override func performClose(_ sender: Any?) { onClose?() }
    // Edit shortcuts reach the web view even when the panel is key and the app is not active.
    override func performKeyEquivalent(with e: NSEvent) -> Bool {
        if e.modifierFlags.intersection(.deviceIndependentFlagsMask) == .command, let k = e.charactersIgnoringModifiers {
            let map: [String: Selector] = ["x": #selector(NSText.cut(_:)), "c": #selector(NSText.copy(_:)), "v": #selector(NSText.paste(_:)),
                                           "a": #selector(NSText.selectAll(_:)), "z": Selector(("undo:"))]
            if k == "w" { onClose?(); return true }
            if let s = map[k], NSApp.sendAction(s, to: nil, from: self) { return true }
        }
        return super.performKeyEquivalent(with: e)
    }
}

// The web view eats every click, so this native strip is the drag handle.
final class DragStrip: NSView {
    override var mouseDownCanMoveWindow: Bool { false }
    override func mouseDown(with event: NSEvent) { window?.performDrag(with: event) }
}

final class PassthroughView: NSView {
    override func hitTest(_ point: NSPoint) -> NSView? { nil }
}

// Microphone capture: only the daemon's own main frame, and only over https or loopback.
func daemonMicDecision(_ origin: WKSecurityOrigin, _ frame: WKFrameInfo, _ type: WKMediaCaptureType) -> WKPermissionDecision {
    guard frame.isMainFrame, type == .microphone, let u = URL(string: "\(origin.protocol)://\(origin.host)" + (origin.port > 0 ? ":\(origin.port)" : "")),
          isDaemon(u) else { return .deny }
    let h = u.host?.lowercased() ?? ""
    return u.scheme?.lowercased() == "https" || h == "localhost" || h == "127.0.0.1" || h == "::1" ? .grant : .deny
}

// GET /api/needs-you: the one list every surface draws.
struct NeedsYouList: Decodable { let items: [NeedsYouRow]; let count: Int? }
struct NeedsYouRow: Decodable {
    let key: String
    let sessionId: String?
    let approval: PendingApproval?
}

// ApprovalsWindow owns the panel, the hotkey and the watch on pending requests.
final class ApprovalsWindow: NSObject, WKNavigationDelegate, WKUIDelegate, NSWindowDelegate, WKScriptMessageHandler {
    static let autoKey = "ApprovalsAutoShow"
    static let frameKey = "ApprovalsUserFrame"
    static let hideDelay: TimeInterval = 3
    static let blankAfter: TimeInterval = 300

    let panel: ApprovalsPanel
    let web: WKWebView
    private let container = NSView()
    private let bar = DragStrip()
    private var hotKey: EventHotKeyRef?
    private var loaded = false
    private var placing = false
    private var retry: Timer?
    private var poll: Timer?
    private var hideTimer: Timer?
    private var blankTimer: Timer?
    private var inflight = false
    private var again = false
    private var refreshQueued = false
    // Keys of GET /api/needs-you, and the item behind each key.
    private var current: Set<String> = []
    private var rows: [String: NeedsYouRow] = [:]
    /// The count the page draws now, from its "state" message.
    private var pageShown = -1
    private var firstRead = true
    // A user close while requests wait snoozes auto-show for exactly those requests.
    private var snoozed: Set<String> = []
    private var typing = false
    /// The window came up from the hotkey or the menu, not by itself; focus changes leave it alone.
    private var userOpened = false
    private var watchdog: Timer?
    private var reloads = 0

    var isShown: Bool { panel.isVisible }
    var autoShow: Bool {
        get { UserDefaults.standard.bool(forKey: ApprovalsWindow.autoKey) }
        set {
            UserDefaults.standard.set(newValue, forKey: ApprovalsWindow.autoKey)
            if newValue && !current.subtracting(snoozed).isEmpty && !panel.isVisible { show(activate: false) }
        }
    }

    init(config: WKWebViewConfiguration) {
        UserDefaults.standard.register(defaults: [ApprovalsWindow.autoKey: true])
        panel = ApprovalsPanel(contentRect: NSRect(x: 0, y: 0, width: 420, height: 640),
                               styleMask: [.borderless, .resizable, .fullSizeContentView, .nonactivatingPanel],
                               backing: .buffered, defer: false)
        web = WKWebView(frame: .zero, configuration: config)
        super.init()
        panel.isReleasedWhenClosed = false
        panel.isFloatingPanel = true
        panel.hidesOnDeactivate = false
        panel.becomesKeyOnlyIfNeeded = false
        // Above .floating, so it also shows over full-screen apps and other floating panels.
        panel.level = .statusBar
        // No .stationary: tv/main.swift found it pins the window to the desktop and hides it over other Spaces' windows.
        panel.collectionBehavior = [.canJoinAllSpaces, .fullScreenAuxiliary, .ignoresCycle]
        panel.isOpaque = false
        panel.backgroundColor = .clear
        panel.hasShadow = true
        panel.minSize = NSSize(width: 320, height: 300)
        panel.delegate = self
        panel.onClose = { [weak self] in self?.hide(user: true) }
        build()
        setFrame(dark: true)

        config.userContentController.add(self, name: "approvals")
        config.userContentController.add(self, name: "open")
        config.userContentController.add(self, name: "theme")
        NSWorkspace.shared.notificationCenter.addObserver(self, selector: #selector(spaceChanged), name: NSWorkspace.activeSpaceDidChangeNotification, object: nil)
        NotificationCenter.default.addObserver(self, selector: #selector(appActivated), name: NSApplication.didBecomeActiveNotification, object: nil)
        NotificationCenter.default.addObserver(self, selector: #selector(appDeactivated), name: NSApplication.didResignActiveNotification, object: nil)
        installHotKey()
    }

    private func build() {
        container.wantsLayer = true
        container.layer?.cornerRadius = 14
        container.layer?.masksToBounds = true
        container.layer?.borderWidth = 1
        panel.contentView = container

        bar.translatesAutoresizingMaskIntoConstraints = false
        bar.wantsLayer = true
        container.addSubview(bar)
        let grip = PassthroughView()
        grip.translatesAutoresizingMaskIntoConstraints = false
        grip.wantsLayer = true
        grip.layer?.cornerRadius = 2
        grip.layer?.backgroundColor = NSColor(white: 0.5, alpha: 0.45).cgColor
        bar.addSubview(grip)
        let close = NSButton(title: "", target: self, action: #selector(closeClicked))
        close.translatesAutoresizingMaskIntoConstraints = false
        close.isBordered = false
        close.image = NSImage(systemSymbolName: "xmark", accessibilityDescription: "Hide")
        close.contentTintColor = NSColor(white: 0.5, alpha: 0.9)
        close.toolTip = "Hide (\(kApprovalsHotkeyLabel) brings it back)"
        bar.addSubview(close)

        web.translatesAutoresizingMaskIntoConstraints = false
        web.navigationDelegate = self
        web.uiDelegate = self
        web.setValue(false, forKey: "drawsBackground")
        if #available(macOS 13.3, *) { web.isInspectable = kDebug }
        container.addSubview(web)
        NSLayoutConstraint.activate([
            bar.topAnchor.constraint(equalTo: container.topAnchor),
            bar.leadingAnchor.constraint(equalTo: container.leadingAnchor),
            bar.trailingAnchor.constraint(equalTo: container.trailingAnchor),
            bar.heightAnchor.constraint(equalToConstant: 18),
            grip.centerXAnchor.constraint(equalTo: bar.centerXAnchor),
            grip.centerYAnchor.constraint(equalTo: bar.centerYAnchor),
            grip.widthAnchor.constraint(equalToConstant: 44),
            grip.heightAnchor.constraint(equalToConstant: 4),
            close.centerYAnchor.constraint(equalTo: bar.centerYAnchor),
            close.trailingAnchor.constraint(equalTo: bar.trailingAnchor, constant: -8),
            close.widthAnchor.constraint(equalToConstant: 12),
            close.heightAnchor.constraint(equalToConstant: 12),
            web.topAnchor.constraint(equalTo: bar.bottomAnchor),
            web.leadingAnchor.constraint(equalTo: container.leadingAnchor),
            web.trailingAnchor.constraint(equalTo: container.trailingAnchor),
            web.bottomAnchor.constraint(equalTo: container.bottomAnchor),
        ])
    }

    // MARK: watch

    // start reads the list now and every 3 s; the notify stream also calls refreshSoon on each item.
    func start() {
        refresh()
        poll = Timer.scheduledTimer(withTimeInterval: 3, repeats: true) { [weak self] _ in self?.refresh() }
        poll?.tolerance = 0.5
    }

    func refreshSoon() {
        guard !refreshQueued else { return }
        refreshQueued = true
        DispatchQueue.main.asyncAfter(deadline: .now() + 0.4) { [weak self] in
            self?.refreshQueued = false
            self?.refresh()
        }
    }

    private func get<T: Decodable>(_ p: String, _ done: @escaping (T?) -> Void) {
        guard let u = URL(string: kBase + p) else { return done(nil) }
        var r = URLRequest(url: u, cachePolicy: .reloadIgnoringLocalCacheData, timeoutInterval: 6)
        authorized(&r)
        URLSession.shared.dataTask(with: r) { d, resp, _ in
            let ok = (resp as? HTTPURLResponse).map { (200..<300).contains($0.statusCode) } ?? false
            done(ok ? d.flatMap { try? JSONDecoder().decode(T.self, from: $0) } : nil)
        }.resume()
    }

    // GET /api/needs-you is the only source; a failed read keeps the last known list.
    func refresh() {
        if inflight { again = true; return }
        inflight = true
        get("/api/needs-you") { [weak self] (l: NeedsYouList?) in
            DispatchQueue.main.async {
                guard let self = self else { return }
                self.inflight = false
                if let l = l {
                    self.rows = Dictionary(l.items.map { ($0.key, $0) }, uniquingKeysWith: { a, _ in a })
                    self.apply(Set(self.rows.keys))
                    AppDelegate.setBadge(l.count ?? l.items.count) // the Dock shows what this list shows
                }
                if self.again { self.again = false; self.refresh() }
            }
        }
    }

    // apply shows the window for a request that was not there before and is not snoozed; an empty queue hides it.
    private func apply(_ ids: Set<String>) {
        let was = current
        let fresh = firstRead ? ids : ids.subtracting(was)
        firstRead = false
        current = ids
        snoozed.formIntersection(ids)
        if ids.isEmpty {
            // Only the queue running empty hides it; a window you opened on an empty queue stays.
            if panel.isVisible && !was.isEmpty { scheduleHide() }
            return
        }
        hideTimer?.invalidate()
        hideTimer = nil
        let want = fresh.subtracting(snoozed)
        guard autoShow, !want.isEmpty, !panel.isVisible else { return }
        // The main window already lists every request, so the panel waits until you leave the app.
        if appInFront { return Self.trace("\(want.count) new, app in front: stay hidden") }
        show(activate: false)
    }

    private var appInFront: Bool { NSApp.isActive && NSApp.keyWindow !== panel && NSApp.keyWindow != nil }

    @objc private func appActivated() {
        // The key window settles after activation; a click on the panel itself must not hide it.
        DispatchQueue.main.asyncAfter(deadline: .now() + 0.2) { [weak self] in
            guard let self = self, self.panel.isVisible, !self.userOpened, self.appInFront, !self.typing else { return }
            self.hide(user: false)
        }
    }

    // Leaving the app brings back a request that still waits.
    @objc private func appDeactivated() {
        if autoShow && !current.subtracting(snoozed).isEmpty && !panel.isVisible { show(activate: false) }
    }

    // Hides after a short delay, but never under the pointer or while you type in the window.
    private func scheduleHide(after delay: TimeInterval = ApprovalsWindow.hideDelay) {
        hideTimer?.invalidate()
        hideTimer = Timer.scheduledTimer(withTimeInterval: delay, repeats: false) { [weak self] _ in
            guard let self = self else { return }
            self.hideTimer = nil
            guard self.current.isEmpty, self.panel.isVisible else { return }
            if NSMouseInRect(NSEvent.mouseLocation, self.panel.frame, false) || self.typing { return self.scheduleHide(after: 1) }
            self.hide(user: false)
        }
    }

    // MARK: show and hide

    // The hotkey hides the window only when you already look at it; otherwise it brings it forward.
    func toggle(fromHotkey: Bool) {
        if panel.isVisible && (!fromHotkey || panel.isKeyWindow) { hide(user: true) } else { userOpened = true; show(activate: true) }
    }

    // Never activates the app: that could move you to the main window's Space.
    func show(activate: Bool) {
        if !loaded { load() }
        blankTimer?.invalidate()
        blankTimer = nil
        place()
        panel.collectionBehavior = [.canJoinAllSpaces, .fullScreenAuxiliary, .ignoresCycle]
        let fade = !panel.isVisible
        if fade { panel.alphaValue = 0 }
        if activate {
            panel.makeKeyAndOrderFront(nil)
            panel.makeFirstResponder(web)
        } else {
            panel.orderFrontRegardless()
        }
        pause(false)
        repaint()
        checkDrawn()
        if fade {
            NSAnimationContext.runAnimationGroup { c in
                c.duration = 0.18
                c.timingFunction = CAMediaTimingFunction(name: .easeOut)
                panel.animator().alphaValue = 1
            }
        }
    }

    func hide(user: Bool) {
        hideTimer?.invalidate()
        hideTimer = nil
        userOpened = false
        watchdog?.invalidate()
        if user { snoozed = current }
        panel.orderOut(nil)
        pause(true)
        // A long-hidden page is unloaded, so it polls nothing; the native watch keeps running.
        blankTimer?.invalidate()
        blankTimer = Timer.scheduledTimer(withTimeInterval: ApprovalsWindow.blankAfter, repeats: false) { [weak self] _ in
            guard let self = self, !self.panel.isVisible else { return }
            self.loaded = false
            self.web.load(URLRequest(url: URL(string: "about:blank")!))
        }
    }

    /// A web view in a panel that was ordered out can stop painting; lay it out and redraw it on each show.
    private func repaint() {
        web.isHidden = false
        web.needsLayout = true
        web.layoutSubtreeIfNeeded()
        web.setNeedsDisplay(web.bounds)
        web.evaluateJavaScript("document.dispatchEvent(new Event('visibilitychange')); window.dispatchEvent(new Event('resize'))", completionHandler: nil)
    }

    /// Appends one line to ~/Library/Logs/agent-monitor-approvals.log; the system log does not keep this app's lines.
    static func trace(_ line: String) {
        let url = FileManager.default.homeDirectoryForCurrentUser.appendingPathComponent("Library/Logs/agent-monitor-approvals.log")
        let text = ISO8601DateFormatter().string(from: Date()) + " " + line + "\n"
        // Start over past 1 MB, so the file stays small.
        if let size = (try? FileManager.default.attributesOfItem(atPath: url.path))?[.size] as? Int, size > 1 << 20 {
            try? FileManager.default.removeItem(at: url)
        }
        if let h = try? FileHandle(forWritingTo: url) {
            h.seekToEndOfFile()
            h.write(Data(text.utf8))
            try? h.close()
        } else {
            try? text.write(to: url, atomically: true, encoding: .utf8)
        }
    }

    /// A shown window whose page drew nothing after 3 s reloads, at most twice in a row.
    /// A page that says what happened and retries itself, instead of a blank panel.
    private func showFallback() {
        let html = "<meta name='color-scheme' content='light dark'><meta http-equiv='refresh' content='15;url=/approvals'><body style='font:13px -apple-system;padding:20px'><b>The approvals list did not draw.</b><p>agent-monitor tries again every 15 seconds. <a href='/approvals'>Try now</a>, or answer requests in the main window.</p></body>"
        reloads = 0
        web.loadHTMLString(html, baseURL: URL(string: kBase))
    }

    private func checkDrawn() {
        watchdog?.invalidate()
        watchdog = Timer.scheduledTimer(withTimeInterval: 3, repeats: false) { [weak self] _ in
            guard let self = self, self.panel.isVisible else { return }
            let probe = "JSON.stringify({n: document.getElementById('root')?.childElementCount || 0, href: location.href, ready: document.readyState, vis: document.visibilityState, textLen: (document.body?.innerText || '').length, html: (document.getElementById('root')?.innerHTML || '').length, errs: window.__amErrors || []})"
            self.web.evaluateJavaScript(probe) { v, err in
                let state = (v as? String) ?? "no answer: \(err?.localizedDescription ?? "")"
                ApprovalsWindow.trace("shown: web \(self.web.frame) hidden=\(self.web.isHidden) panel \(self.panel.frame) occlusion=\(self.panel.occlusionState.contains(.visible) ? "visible" : "occluded") page \(state)")
                if state.contains("\"n\":0") == false && v != nil { self.reloads = 0; return }
                NSLog("agent-monitor approvals: blank page state %@", state)
                guard self.reloads < 2 else {
                    NSLog("agent-monitor approvals: page still blank after 2 reloads")
                    return self.showFallback()
                }
                self.reloads += 1
                NSLog("agent-monitor approvals: page drew nothing, reload %d", self.reloads)
                self.load()
                self.checkDrawn()
            }
        }
    }

    @objc private func closeClicked() { hide(user: true) }

    private func pause(_ on: Bool) {
        web.evaluateJavaScript("window.__amPause && window.__amPause(\(on ? "true" : "false"))", completionHandler: nil)
    }

    // canJoinAllSpaces can lapse for a regular app; ordering front on each Space change keeps it there.
    @objc private func spaceChanged() {
        guard panel.isVisible else { return }
        panel.collectionBehavior = [.canJoinAllSpaces, .fullScreenAuxiliary, .ignoresCycle]
        panel.orderFrontRegardless()
    }

    // MARK: position

    // A position you chose wins while it is on a screen; otherwise the top right of the screen with the pointer.
    private func place() {
        placing = true
        defer { placing = false }
        let screens = NSScreen.screens
        if let s = UserDefaults.standard.string(forKey: ApprovalsWindow.frameKey) {
            let f = NSRectFromString(s)
            if f.width > 0, let scr = screens.first(where: { $0.visibleFrame.intersects(f) }) {
                panel.setFrame(clamp(f, scr.visibleFrame), display: false)
                return
            }
        }
        if panel.isVisible, screens.contains(where: { $0.visibleFrame.intersects(panel.frame) }) { return }
        let mouse = NSEvent.mouseLocation
        let vf = (screens.first { NSMouseInRect(mouse, $0.frame, false) } ?? NSScreen.main)?.visibleFrame ?? NSRect(x: 0, y: 0, width: 1440, height: 900)
        let s = panel.frame.size
        panel.setFrame(clamp(NSRect(x: vf.maxX - s.width - 16, y: vf.maxY - s.height - 12, width: s.width, height: s.height), vf), display: false)
    }

    private func clamp(_ f: NSRect, _ vf: NSRect) -> NSRect {
        var r = f
        r.size.width = min(r.width, vf.width)
        r.size.height = min(r.height, vf.height)
        r.origin.x = min(max(r.minX, vf.minX), vf.maxX - r.width)
        r.origin.y = min(max(r.minY, vf.minY), vf.maxY - r.height)
        return r
    }

    private func remember() {
        guard !placing, panel.isVisible else { return }
        UserDefaults.standard.set(NSStringFromRect(panel.frame), forKey: ApprovalsWindow.frameKey)
    }
    func windowDidMove(_ n: Notification) { remember() }
    func windowDidEndLiveResize(_ n: Notification) { remember() }

    // MARK: page

    // The page is never served from cache, so a daemon update shows on the next load.
    func load() {
        guard let u = URL(string: kBase + "/approvals") else { return }
        var r = URLRequest(url: u, cachePolicy: .reloadIgnoringLocalAndRemoteCacheData)
        authorized(&r)
        loaded = true
        web.load(r)
    }

    private func scheduleRetry() {
        retry?.invalidate()
        retry = Timer.scheduledTimer(withTimeInterval: 2, repeats: false) { [weak self] _ in
            guard let self = self, self.loaded else { return }
            self.load()
        }
    }

    // Sets the page's theme mode, then matches the frame to the page.
    func setTheme(_ mode: String) {
        web.evaluateJavaScript("window.__amSetTheme && window.__amSetTheme('\(mode)')") { [weak self] _, _ in self?.syncFrame() }
    }

    // Reads the frame colors from the page's CSS variables, so the native strip matches either theme.
    func syncFrame() {
        let js = "(() => { const s = getComputedStyle(document.querySelector('.v3') || document.documentElement); return [document.documentElement.dataset.theme || ''].concat(['--bg','--panel','--line'].map(v => s.getPropertyValue(v).trim())); })()"
        web.evaluateJavaScript(js) { [weak self] r, _ in
            guard let self = self, let a = r as? [String], a.count == 4 else { return }
            self.setFrame(dark: a[0] != "light", bg: parseCSSColor(a[1]), strip: parseCSSColor(a[2]), line: parseCSSColor(a[3]))
        }
    }

    func setFrame(dark: Bool, bg: NSColor? = nil, strip: NSColor? = nil, line: NSColor? = nil) {
        let fallback = dark ? NSColor(red: 0.07, green: 0.07, blue: 0.063, alpha: 1) : NSColor(red: 0.957, green: 0.953, blue: 0.937, alpha: 1)
        container.layer?.backgroundColor = (bg ?? fallback).cgColor
        bar.layer?.backgroundColor = (strip ?? bg ?? fallback).cgColor
        container.layer?.borderColor = (line ?? NSColor(white: dark ? 1 : 0, alpha: 0.1)).cgColor
        panel.appearance = NSAppearance(named: dark ? .darkAqua : .aqua)
    }

    func userContentController(_ c: WKUserContentController, didReceive m: WKScriptMessage) {
        guard m.webView === web else { return }
        switch m.name {
        case "approvals":
            guard let b = m.body as? [String: Any] else { return }
            if b["type"] as? String == "hide" { hide(user: true) }
            if b["type"] as? String == "state" {
                typing = b["typing"] as? Bool ?? false
                pageShown = (b["shown"] as? NSNumber)?.intValue ?? pageShown
                // The page drew nothing and the list is empty: nothing is left to answer here.
                if pageShown == 0 && current.isEmpty && panel.isVisible && !userOpened && hideTimer == nil { scheduleHide() }
            }
        case "open":
            if let s = m.body as? String { (NSApp.delegate as? AppDelegate)?.open(s) }
        case "theme":
            syncFrame()
        default: break
        }
    }

    // MARK: navigation

    // The window stays on its own page; daemon links open in the main window, other web links in the browser.
    func webView(_ w: WKWebView, decidePolicyFor a: WKNavigationAction, decisionHandler: @escaping (WKNavigationActionPolicy) -> Void) {
        guard let u = a.request.url else { return decisionHandler(.allow) }
        if u.isFileURL { return decisionHandler(.cancel) }
        if ["blob", "data", "about"].contains(u.scheme ?? "") { return decisionHandler(.allow) }
        if isDaemon(u) && (a.navigationType != .linkActivated || u.path == "/approvals") { return decisionHandler(.allow) }
        (NSApp.delegate as? AppDelegate)?.open(u.absoluteString)
        decisionHandler(.cancel)
    }
    func webView(_ w: WKWebView, createWebViewWith c: WKWebViewConfiguration, for a: WKNavigationAction, windowFeatures f: WKWindowFeatures) -> WKWebView? {
        if let u = a.request.url { (NSApp.delegate as? AppDelegate)?.open(u.absoluteString) }
        return nil
    }
    func webView(_ w: WKWebView, didFinish n: WKNavigation!) {
        syncFrame()
        if !panel.isVisible { pause(true) }
    }
    func webView(_ w: WKWebView, didFailProvisionalNavigation n: WKNavigation!, withError e: Error) {
        NSLog("agent-monitor approvals: load failed: %@", e.localizedDescription)
        if (e as NSError).code != NSURLErrorCancelled { scheduleRetry() }
    }
    func webView(_ w: WKWebView, didFail n: WKNavigation!, withError e: Error) {
        if (e as NSError).code != NSURLErrorCancelled { scheduleRetry() }
    }
    func webViewWebContentProcessDidTerminate(_ w: WKWebView) {
        NSLog("agent-monitor approvals: web content process ended")
        if loaded { load() }
    }

    @available(macOS 12.0, *)
    func webView(_ w: WKWebView, requestMediaCapturePermissionFor origin: WKSecurityOrigin, initiatedByFrame frame: WKFrameInfo, type: WKMediaCaptureType, decisionHandler: @escaping (WKPermissionDecision) -> Void) {
        decisionHandler(daemonMicDecision(origin, frame, type))
    }

    // MARK: hotkey

    // Carbon hotkey: control + option + space toggles the window and needs no Accessibility permission.
    private func installHotKey() {
        var spec = EventTypeSpec(eventClass: OSType(kEventClassKeyboard), eventKind: UInt32(kEventHotKeyPressed))
        InstallEventHandler(GetApplicationEventTarget(), { _, _, ud in
            guard let ud = ud else { return noErr }
            let f = Unmanaged<ApprovalsWindow>.fromOpaque(ud).takeUnretainedValue()
            DispatchQueue.main.async { f.toggle(fromHotkey: true) }
            return noErr
        }, 1, &spec, Unmanaged.passUnretained(self).toOpaque(), nil)
        let id = EventHotKeyID(signature: OSType(0x414D_4150), id: 1) // "AMAP"
        let st = RegisterEventHotKey(UInt32(kVK_Space), UInt32(controlKey | optionKey), id, GetApplicationEventTarget(), 0, &hotKey)
        if st != noErr { NSLog("agent-monitor: \(kApprovalsHotkeyLabel) is taken by another app (status \(st)); use Window > Approvals") }
    }
}

// Parses "#rgb", "#rrggbb" and "rgb()/rgba()" CSS colors.
func parseCSSColor(_ s: String) -> NSColor? {
    let t = s.trimmingCharacters(in: .whitespaces).lowercased()
    if t.hasPrefix("#") {
        var h = String(t.dropFirst())
        if h.count == 3 { h = h.map { "\($0)\($0)" }.joined() }
        guard h.count == 6, let v = UInt32(h, radix: 16) else { return nil }
        return NSColor(srgbRed: CGFloat((v >> 16) & 0xff) / 255, green: CGFloat((v >> 8) & 0xff) / 255, blue: CGFloat(v & 0xff) / 255, alpha: 1)
    }
    guard t.hasPrefix("rgb"), let open = t.firstIndex(of: "("), let close = t.lastIndex(of: ")") else { return nil }
    let n = t[t.index(after: open)..<close].split(whereSeparator: { $0 == "," || $0 == " " || $0 == "/" }).compactMap { Double($0) }
    guard n.count >= 3 else { return nil }
    return NSColor(srgbRed: n[0] / 255, green: n[1] / 255, blue: n[2] / 255, alpha: n.count > 3 ? n[3] : 1)
}
