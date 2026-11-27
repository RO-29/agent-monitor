// WebWindow: one Workspace window with a native unified toolbar around a WKWebView.
import Cocoa
import WebKit

// Files dropped anywhere reach the composer; the page never navigates to file://.
let kDropScript = """
(() => {
  const files = e => !!e.dataTransfer && Array.from(e.dataTransfer.types || []).includes("Files");
  addEventListener("dragover", e => { if (files(e)) { e.preventDefault(); e.dataTransfer.dropEffect = "copy"; } });
  addEventListener("drop", e => {
    if (!files(e) || e.defaultPrevented) return;
    e.preventDefault();
    const box = document.querySelector(".wcomp:not(.off)");
    if (!box || box.contains(e.target)) return;
    let dt = e.dataTransfer;
    try { const c = new DataTransfer(); for (const f of e.dataTransfer.files) c.items.add(f); dt = c; } catch (_) {}
    box.dispatchEvent(new DragEvent("drop", { bubbles: true, cancelable: true, dataTransfer: dt }));
  });
})();
"""

// Marks the page as running in the Mac app, so the web hides its own left rail behind the toolbar segments.
let kShellScript = "document.documentElement.dataset.shell = 'mac';"

private let kAttachProbe = """
const b = document.querySelector('button[aria-label*="Attach"], [role="button"][aria-label*="Attach"], input[type="file"][aria-label*="Attach"]');
if (b) { b.click(); return true; }
return false;
"""

extension NSToolbarItem.Identifier {
    static let nav = Self("am.nav"), sections = Self("am.sections")
    static let newThread = Self("am.new"), search = Self("am.search"), more = Self("am.more")
    static let leftBar = Self("am.leftBar"), rightBar = Self("am.rightBar")
}

// Toolbar sections in order; the index is the segment.
let kSections: [(title: String, path: String)] = [("Threads", "/w"), ("Needs you", "/needs"), ("Search", "/w/search"), ("Settings", "/settings")]

func sectionIndex(_ path: String) -> Int {
    if path.hasPrefix("/needs") || path.hasPrefix("/notifications") { return 1 }
    if path == "/w/search" { return 2 }
    if path.hasPrefix("/settings") || path == "/more" || path.hasPrefix("/rules") { return 3 }
    if path == "/" || path == "/w" || path.hasPrefix("/w/") { return path == "/w/new" ? -1 : 0 }
    return -1
}

final class WebWindow: NSWindowController, WKNavigationDelegate, WKUIDelegate, WKDownloadDelegate, NSWindowDelegate, NSToolbarDelegate, NSSearchFieldDelegate {
    let web: WKWebView
    var onClose: ((WebWindow) -> Void)?
    var onChange: (() -> Void)?
    private var watches: [NSKeyValueObservation] = []
    private let segments = NSSegmentedControl(labels: kSections.map { $0.title }, trackingMode: .selectOne, target: nil, action: nil)
    // Back and forward as one momentary segmented control, like Safari's.
    private let navSeg = NSSegmentedControl(images: [NSImage(systemSymbolName: "chevron.backward", accessibilityDescription: "Back") ?? NSImage(),
                                                     NSImage(systemSymbolName: "chevron.forward", accessibilityDescription: "Forward") ?? NSImage()],
                                            trackingMode: .momentary, target: nil, action: nil)
    private weak var leftButton: NSButton?
    // The offline page sits at the daemon's base URL; it must not be saved as the tab's page.
    private var offlineShown = false
    private var crashes: [Date] = []
    private weak var rightButton: NSButton?

    // Path, query and fragment of the current daemon page, or nil for anything else.
    var path: String? {
        guard !offlineShown, let u = web.url, isDaemon(u), u.scheme?.hasPrefix("http") == true else { return nil }
        return daemonPath(u)
    }

    init(path: String, config: WKWebViewConfiguration, autosave: String?) {
        web = WKWebView(frame: .zero, configuration: config)
        let win = NSWindow(contentRect: NSRect(x: 0, y: 0, width: 1360, height: 860),
                           styleMask: [.titled, .closable, .miniaturizable, .resizable],
                           backing: .buffered, defer: false)
        win.minSize = NSSize(width: 520, height: 480)
        win.title = "agent-monitor"
        win.tabbingMode = .preferred
        win.toolbarStyle = .unified
        super.init(window: win)
        win.delegate = self
        web.navigationDelegate = self
        web.uiDelegate = self
        web.allowsBackForwardNavigationGestures = true
        web.allowsMagnification = true
        if #available(macOS 13.3, *) { web.isInspectable = kDebug }
        win.contentView = web

        segments.target = self
        segments.action = #selector(pickSection(_:))
        segments.segmentStyle = .automatic
        segments.controlSize = .regular
        navSeg.target = self
        navSeg.action = #selector(pickNav(_:))
        navSeg.segmentStyle = .separated
        navSeg.setToolTip("Back (⌘[)", forSegment: 0)
        navSeg.setToolTip("Forward (⌘])", forSegment: 1)
        navSeg.setEnabled(false, forSegment: 0)
        navSeg.setEnabled(false, forSegment: 1)
        let bar = NSToolbar(identifier: "AgentMonitorToolbar")
        bar.delegate = self
        bar.displayMode = .iconOnly
        bar.allowsUserCustomization = false
        win.toolbar = bar

        watches = [
            web.observe(\.title, options: [.new]) { [weak self] w, _ in
                let t = w.title ?? ""
                self?.window?.title = t.isEmpty ? "agent-monitor" : t
            },
            web.observe(\.url, options: [.new]) { [weak self] _, _ in self?.urlChanged() },
            web.observe(\.canGoBack, options: [.new]) { [weak self] w, _ in self?.navSeg.setEnabled(w.canGoBack, forSegment: 0) },
            web.observe(\.canGoForward, options: [.new]) { [weak self] w, _ in self?.navSeg.setEnabled(w.canGoForward, forSegment: 1) },
        ]

        if let a = autosave {
            if !win.setFrameUsingName(a) { win.center() }
            win.setFrameAutosaveName(a)
        } else {
            win.center()
        }
        go(path)
    }
    required init?(coder: NSCoder) { fatalError("unused") }

    func go(_ path: String) {
        guard let u = URL(string: kBase + path) else { return }
        var r = URLRequest(url: u)
        if !kToken.isEmpty { r.setValue("Bearer " + kToken, forHTTPHeaderField: "Authorization") }
        web.load(r)
    }

    private func urlChanged() {
        segments.selectedSegment = path.map(sectionIndex) ?? -1
        let ws = path.map { $0 == "/" || $0.hasPrefix("/w") } ?? false
        leftButton?.isEnabled = ws
        rightButton?.isEnabled = ws
        onChange?()
    }

    func windowWillClose(_ n: Notification) {
        watches.removeAll()
        onClose?(self)
    }

    // MARK: toolbar

    func toolbarDefaultItemIdentifiers(_ t: NSToolbar) -> [NSToolbarItem.Identifier] {
        [.leftBar, .nav, .flexibleSpace, .sections, .flexibleSpace, .newThread, .more, .search, .rightBar]
    }
    func toolbarAllowedItemIdentifiers(_ t: NSToolbar) -> [NSToolbarItem.Identifier] {
        toolbarDefaultItemIdentifiers(t) + [.space]
    }

    func toolbar(_ t: NSToolbar, itemForItemIdentifier id: NSToolbarItem.Identifier, willBeInsertedIntoToolbar flag: Bool) -> NSToolbarItem? {
        func button(_ label: String, _ symbol: String, _ action: Selector) -> NSToolbarItem {
            let i = NSToolbarItem(itemIdentifier: id)
            i.label = label
            i.toolTip = label
            i.image = NSImage(systemSymbolName: symbol, accessibilityDescription: label)
            i.isBordered = true
            i.target = self
            i.action = action
            i.autovalidates = false
            return i
        }
        switch id {
        case .nav:
            let i = NSToolbarItem(itemIdentifier: id)
            i.label = "Back/Forward"
            i.view = navSeg
            i.isNavigational = true
            return i
        case .sections:
            let i = NSToolbarItem(itemIdentifier: id)
            i.label = "Section"
            i.view = segments
            return i
        case .more:
            // Less-used items live here: reload and appearance, then views, tools and settings.
            let i = NSMenuToolbarItem(itemIdentifier: id)
            i.label = "More"
            i.toolTip = "Reload, appearance, other views, tools and settings"
            i.image = NSImage(systemSymbolName: "ellipsis.circle", accessibilityDescription: i.label)
            i.showsIndicator = false
            let m = NSMenu(title: i.label)
            let reload = NSMenuItem(title: "Reload Page", action: #selector(reloadPage), keyEquivalent: "")
            reload.target = self
            let look = NSMenuItem(title: "Appearance", action: nil, keyEquivalent: "")
            let sub = NSMenu(title: "Appearance")
            ((NSApp.delegate as? AppDelegate)?.menuItems(.appearance) ?? []).forEach { sub.addItem($0) }
            look.submenu = sub
            [reload, look, .separator()].forEach { m.addItem($0) }
            ((NSApp.delegate as? AppDelegate)?.menuItems(.more) ?? []).forEach { m.addItem($0) }
            i.menu = m
            return i
        case .newThread:
            let i = button("New Thread", "square.and.pencil", #selector(newThread))
            i.toolTip = "New thread (⇧⌘N)"
            return i
        case .leftBar, .rightBar:
            let left = id == .leftBar
            let b = NSButton(image: NSImage(systemSymbolName: left ? "sidebar.left" : "sidebar.right", accessibilityDescription: left ? "Thread list" : "Details") ?? NSImage(),
                             target: self, action: left ? #selector(toggleLeft) : #selector(toggleRight))
            // pushOnPushOff draws the pressed bezel while the side is shown, like Xcode's panel toggles.
            b.setButtonType(.pushOnPushOff)
            b.bezelStyle = .texturedRounded
            if left { leftButton = b } else { rightButton = b }
            let i = NSToolbarItem(itemIdentifier: id)
            i.label = left ? "Thread List" : "Details"
            i.view = b
            showLayout((NSApp.delegate as? AppDelegate)?.layout ?? [:])
            return i
        case .search:
            let i = NSSearchToolbarItem(itemIdentifier: id)
            i.label = "Search"
            i.searchField.placeholderString = "Search transcripts"
            i.searchField.sendsWholeSearchString = true
            i.searchField.target = self
            i.searchField.action = #selector(searchSubmitted(_:))
            return i
        default:
            return nil
        }
    }

    @objc func pickNav(_ s: NSSegmentedControl) {
        if s.selectedSegment == 0 { web.goBack() } else if s.selectedSegment == 1 { web.goForward() }
    }
    @objc func goBack() { web.goBack() }
    @objc func goForward() { web.goForward() }
    @objc func reloadPage() { web.reload() }
    @objc func newThread() { go("/w/new") }
    @objc func toggleLeft() { toggleBar("left") }
    @objc func toggleRight() { toggleBar("right") }
    private func toggleBar(_ b: String) {
        web.evaluateJavaScript("window.__amToggle ? window.__amToggle('\(b)') : null") { r, _ in (NSApp.delegate as? AppDelegate)?.layoutRead(r) }
    }
    // Pressed while that side is shown; the layout maps a side to true when it is hidden.
    func showLayout(_ l: [String: Bool]) {
        let left = l["left"] != true, right = l["right"] != true
        leftButton?.state = left ? .on : .off
        leftButton?.toolTip = left ? "Hide thread list (⌘\\)" : "Show thread list (⌘\\)"
        rightButton?.state = right ? .on : .off
        rightButton?.toolTip = right ? "Hide details (⌥⌘\\)" : "Show details (⌥⌘\\)"
    }
    @objc func pickSection(_ s: NSSegmentedControl) {
        guard s.selectedSegment >= 0, s.selectedSegment < kSections.count else { return }
        go(kSections[s.selectedSegment].path)
    }
    @objc func searchSubmitted(_ f: NSSearchField) {
        let q = f.stringValue.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !q.isEmpty else { return }
        var allowed = CharacterSet.urlQueryAllowed
        allowed.remove(charactersIn: "&+=#?")
        go("/w/search?q=" + (q.addingPercentEncoding(withAllowedCharacters: allowed) ?? q))
    }
    func focusSearch() {
        guard let i = window?.toolbar?.items.first(where: { $0.itemIdentifier == .search }) as? NSSearchToolbarItem else { return }
        i.beginSearchInteraction()
    }

    // MARK: attach

    // Clicks the page's Attach control; beeps when the page has none.
    func attach() {
        web.callAsyncJavaScript(kAttachProbe, arguments: [:], in: nil, in: .page) { r in
            if (try? r.get() as? Bool) != true { NSSound.beep() }
        }
    }

    // MARK: navigation

    // Pages on the daemon stay here; file:// never loads; web and mail links open outside, other schemes are dropped.
    func webView(_ w: WKWebView, decidePolicyFor a: WKNavigationAction, decisionHandler: @escaping (WKNavigationActionPolicy) -> Void) {
        if a.shouldPerformDownload { return decisionHandler(.download) }
        guard let u = a.request.url else { return decisionHandler(.allow) }
        if u.isFileURL { return decisionHandler(.cancel) }
        if ["blob", "data", "about"].contains(u.scheme ?? "") || isDaemon(u) { return decisionHandler(.allow) }
        openExternal(u)
        decisionHandler(.cancel)
    }
    func webView(_ w: WKWebView, decidePolicyFor r: WKNavigationResponse, decisionHandler: @escaping (WKNavigationResponsePolicy) -> Void) {
        if r.isForMainFrame, r.response is HTTPURLResponse { offlineShown = false }
        decisionHandler(r.canShowMIMEType ? .allow : .download)
    }
    func webView(_ w: WKWebView, navigationAction: WKNavigationAction, didBecome d: WKDownload) { d.delegate = self }
    func webView(_ w: WKWebView, navigationResponse: WKNavigationResponse, didBecome d: WKDownload) { d.delegate = self }
    func webView(_ w: WKWebView, didFailProvisionalNavigation n: WKNavigation!, withError e: Error) {
        if (e as NSError).code == NSURLErrorCancelled { return }
        offline(e)
    }
    // A page that crashes on every load would reload forever; after the third crash in 30 s show the offline page.
    func webViewWebContentProcessDidTerminate(_ w: WKWebView) {
        let now = Date()
        crashes = crashes.filter { now.timeIntervalSince($0) < 30 } + [now]
        if crashes.count >= 3 || offlineShown {
            offline(NSError(domain: "agent-monitor", code: 1, userInfo: [NSLocalizedDescriptionKey: "the page stopped working three times"]))
        } else {
            w.reload()
        }
    }
    func webView(_ w: WKWebView, didFail n: WKNavigation!, withError e: Error) {
        if (e as NSError).code == NSURLErrorCancelled { return }
        offline(e)
    }

    // After each load, read the page's appearance and approval-key settings so the menus show them.
    func webView(_ w: WKWebView, didFinish n: WKNavigation!) {
        w.evaluateJavaScript("[window.__amTheme ? window.__amTheme() : '', window.__amApproveKeys ? window.__amApproveKeys() : null, window.__amLayout ? window.__amLayout() : null]") { r, _ in
            guard let a = r as? [Any], let d = NSApp.delegate as? AppDelegate else { return }
            if let t = a.first as? String, !t.isEmpty { d.appearanceRead(t) }
            if a.count > 1, let k = a[1] as? Bool { d.approveKeys = k }
            if a.count > 2 { d.layoutRead(a[2]) }
        }
    }

    func setTheme(_ mode: String) {
        web.evaluateJavaScript("window.__amSetTheme && window.__amSetTheme('\(mode)')", completionHandler: nil)
        applyAppearance(mode)
    }
    func applyAppearance(_ mode: String) {
        window?.appearance = mode == "light" ? NSAppearance(named: .aqua) : mode == "dark" ? NSAppearance(named: .darkAqua) : nil
    }

    private func offline(_ e: Error) {
        // Retry the page that failed; the base URL keeps the retry on the daemon (a nil base reloads about:blank).
        let failed = esc(((e as NSError).userInfo[NSURLErrorFailingURLErrorKey] as? URL)?.absoluteString ?? kBase + "/w")
        let why = esc(e.localizedDescription)
        offlineShown = true
        let html = "<title>agent-monitor</title><meta name='color-scheme' content='light dark'><meta http-equiv='refresh' content='10;url=\(failed)'><body style='font:15px -apple-system;padding:48px'><h2 style='font-family:\"New York\",serif'>The daemon is not reachable</h2><p>\(kBase) did not answer: \(why)</p><p>This page tries again every 10 seconds. <a href='\(failed)'>Try now</a></p></body>"
        web.loadHTMLString(html, baseURL: URL(string: kBase))
    }

    // Downloads (for example an exported .md) go to ~/Downloads under a free name.
    func download(_ d: WKDownload, decideDestinationUsing r: URLResponse, suggestedFilename name: String, completionHandler: @escaping (URL?) -> Void) {
        let dir = FileManager.default.urls(for: .downloadsDirectory, in: .userDomainMask)[0]
        var dest = dir.appendingPathComponent(name)
        let base = dest.deletingPathExtension().lastPathComponent, ext = dest.pathExtension
        var i = 1
        while FileManager.default.fileExists(atPath: dest.path) {
            dest = dir.appendingPathComponent("\(base) \(i)" + (ext.isEmpty ? "" : ".\(ext)"))
            i += 1
        }
        completionHandler(dest)
    }
    func downloadDidFinish(_ d: WKDownload) { NSSound(named: "Glass")?.play() }

    // window.open and target=_blank: daemon pages open in a new tab, the rest in the browser.
    func webView(_ w: WKWebView, createWebViewWith c: WKWebViewConfiguration, for a: WKNavigationAction, windowFeatures f: WKWindowFeatures) -> WKWebView? {
        if let u = a.request.url {
            if isDaemon(u) {
                (NSApp.delegate as? AppDelegate)?.newWindow(path: daemonPath(u), tabOf: window)
            } else {
                openExternal(u)
            }
        }
        return nil
    }
    // The same microphone rule as the Approvals window, so WebKit shows no raw prompt.
    @available(macOS 12.0, *)
    func webView(_ w: WKWebView, requestMediaCapturePermissionFor origin: WKSecurityOrigin, initiatedByFrame frame: WKFrameInfo, type: WKMediaCaptureType, decisionHandler: @escaping (WKPermissionDecision) -> Void) {
        decisionHandler(daemonMicDecision(origin, frame, type))
    }
    func webView(_ w: WKWebView, runOpenPanelWith p: WKOpenPanelParameters, initiatedByFrame f: WKFrameInfo, completionHandler: @escaping ([URL]?) -> Void) {
        let panel = NSOpenPanel()
        panel.allowsMultipleSelection = p.allowsMultipleSelection
        panel.canChooseDirectories = p.allowsDirectories
        panel.beginSheetModal(for: window!) { completionHandler($0 == .OK ? panel.urls : nil) }
    }
    func webView(_ w: WKWebView, runJavaScriptAlertPanelWithMessage m: String, initiatedByFrame f: WKFrameInfo, completionHandler: @escaping () -> Void) {
        let a = NSAlert()
        a.messageText = m
        a.beginSheetModal(for: window!) { _ in completionHandler() }
    }
    func webView(_ w: WKWebView, runJavaScriptConfirmPanelWithMessage m: String, initiatedByFrame f: WKFrameInfo, completionHandler: @escaping (Bool) -> Void) {
        let a = NSAlert()
        a.messageText = m
        a.addButton(withTitle: "OK")
        a.addButton(withTitle: "Cancel")
        a.beginSheetModal(for: window!) { completionHandler($0 == .alertFirstButtonReturn) }
    }
}

private func esc(_ s: String) -> String {
    s.replacingOccurrences(of: "&", with: "&amp;").replacingOccurrences(of: "<", with: "&lt;")
        .replacingOccurrences(of: ">", with: "&gt;").replacingOccurrences(of: "'", with: "&#39;").replacingOccurrences(of: "\"", with: "&quot;")
}
