// AgentMonitor: the Workspace in its own Mac window, with a Dock badge and native notifications.
import Cocoa
import WebKit

let kBase: String = {
    let env = ProcessInfo.processInfo.environment["AGENT_MONITOR_URL"] ?? ""
    return env.isEmpty ? "http://127.0.0.1:7777" : env.trimmingCharacters(in: CharacterSet(charactersIn: "/"))
}()
// AGENT_MONITOR_TOKEN is a device token (amd_...) for a remote daemon; sent as a Bearer header and the am_device cookie.
let kToken = (ProcessInfo.processInfo.environment["AGENT_MONITOR_TOKEN"] ?? "").trimmingCharacters(in: .whitespacesAndNewlines)
let kBaseURL = URL(string: kBase)
let kHost = kBaseURL?.host ?? "127.0.0.1"
// AGENT_MONITOR_DEBUG=1 turns on the Web Inspector.
let kDebug = ProcessInfo.processInfo.environment["AGENT_MONITOR_DEBUG"] == "1"

private func port(_ u: URL) -> Int? { u.port ?? ["http": 80, "https": 443][u.scheme?.lowercased() ?? ""] }
private func loopback(_ h: String?) -> Bool { h == "localhost" || h == "127.0.0.1" }

// A daemon URL has kBase's scheme, host and port; localhost and 127.0.0.1 count as one host only on that port.
func isDaemon(_ u: URL) -> Bool {
    guard let b = kBaseURL, u.scheme?.lowercased() == b.scheme?.lowercased(), port(u) == port(b) else { return false }
    let h = u.host?.lowercased(), bh = b.host?.lowercased()
    return h == bh || (loopback(h) && loopback(bh))
}

// Path, query and fragment of a URL, still percent-encoded.
func daemonPath(_ u: URL) -> String {
    guard let c = URLComponents(url: u, resolvingAgainstBaseURL: false) else { return u.path }
    let p = c.percentEncodedPath.isEmpty ? "/" : c.percentEncodedPath
    return p + (c.percentEncodedQuery.map { "?" + $0 } ?? "") + (c.percentEncodedFragment.map { "#" + $0 } ?? "")
}

// Only web and mail links leave the app; any other scheme is dropped.
func openExternal(_ u: URL) {
    guard ["http", "https", "mailto"].contains(u.scheme?.lowercased() ?? "") else { return }
    NSWorkspace.shared.open(u)
}

// The web rail's More menu as native menus: (title, path) pairs.
let kOtherViews: [(String, String)] = [("Desk", "/desk"), ("Control Room", "/control"), ("Messages", "/messages"), ("Timeline", "/timeline")]
let kTools: [(String, String)] = [("Board", "/board"), ("Explore Spans", "/explore"), ("Errors", "/errors"), ("Tracing", "/tracing"),
                                  ("Monitors", "/monitors"), ("Intelligence", "/intel"), ("Rules", "/rules")]
let kSettingsPages: [(String, String)] = [("Devices", "/settings/devices"), ("Notification Rules", "/settings/notifications"), ("Limits", "/settings/limits"), ("Voice", "/settings/voice"),
                                          ("Templates", "/settings/templates"), ("Schedules", "/settings/schedules")]
let kAppearances: [(String, String)] = [("System", "system"), ("Light", "light"), ("Dark", "dark")]

enum MenuKind { case more, appearance }


final class AppDelegate: NSObject, NSApplicationDelegate, NSMenuItemValidation, NSMenuDelegate, WKScriptMessageHandler {
    static let sessionKey = "LastSession"
    static let maxRestore = 8
    var windows: [WebWindow] = []
    var timer: Timer?
    var quitting = false
    let notifier = Notifier()
    // Mirrors the page's localStorage settings; the web default with nothing stored is dark.
    var appearance = "dark"
    var approveKeys = true
    // Mirrors the Workspace layout (true = hidden) for the View menu titles.
    var layout: [String: Bool] = [:]
    var layoutItems: [String: NSMenuItem] = [:]
    // One configuration for every window: one process pool, one data store, one drop script.
    let config: WKWebViewConfiguration = {
        let c = WKWebViewConfiguration()
        c.websiteDataStore = .default()
        if kDebug { c.preferences.setValue(true, forKey: "developerExtrasEnabled") }
        c.userContentController.addUserScript(WKUserScript(source: kShellScript, injectionTime: .atDocumentStart, forMainFrameOnly: true))
        c.userContentController.addUserScript(WKUserScript(source: kDropScript, injectionTime: .atDocumentEnd, forMainFrameOnly: true))
        c.userContentController.addScriptMessageHandler(TouchIDGate(), contentWorld: .page, name: "amTouchID")
        c.userContentController.addUserScript(WKUserScript(source: kTouchIDScript, injectionTime: .atDocumentStart, forMainFrameOnly: true))
        if !kToken.isEmpty, let ck = HTTPCookie(properties: [.domain: kHost, .path: "/", .name: "am_device", .value: kToken, .secure: kBase.hasPrefix("https") ? "TRUE" : "FALSE",
                                                         HTTPCookiePropertyKey("HttpOnly"): "TRUE"]) { // no page script reads it
            c.websiteDataStore.httpCookieStore.setCookie(ck)
        }
        return c
    }()
    lazy var approvals = ApprovalsWindow(config: config)

    func applicationDidFinishLaunching(_ n: Notification) {
        buildMenu()
        // The page posts its layout on every change, so the toolbar side buttons follow keyboard shortcuts too.
        config.userContentController.add(self, name: "layout")
        restoreSession()
        approvals.start()
        NSApp.activate(ignoringOtherApps: true)
        // The notify stream refreshes the badge on each item; the slow timer covers a dropped stream.
        timer = Timer.scheduledTimer(withTimeInterval: 60, repeats: true) { [weak self] _ in self?.badge() }
        timer?.tolerance = 10
        badge()
        notifier.open = { [weak self] in self?.open($0) }
        notifier.currentPath = { [weak self] in self?.front?.path }
        notifier.changed = { [weak self] in
            self?.badgeSoon()
            self?.approvals.refreshSoon()
        }
        notifier.start()
    }
    func applicationShouldHandleReopen(_ s: NSApplication, hasVisibleWindows v: Bool) -> Bool {
        // The Approvals panel counts as a visible window, so the check is on the main windows.
        if windows.isEmpty { newWindow(path: "/w", tabOf: nil, autosave: "AgentMonitorMain") }
        return true
    }
    func applicationShouldTerminateAfterLastWindowClosed(_ s: NSApplication) -> Bool { false }
    func applicationWillTerminate(_ n: Notification) {
        saveSession()
        quitting = true
        timer?.invalidate()
        notifier.stop()
    }

    @discardableResult
    func newWindow(path: String, tabOf: NSWindow?, autosave: String? = nil) -> WebWindow {
        let w = WebWindow(path: path, config: config, autosave: autosave)
        w.applyAppearance(appearance)
        w.onClose = { [weak self] c in
            guard let self = self else { return }
            self.windows.removeAll { $0 === c }
            // Closing the last window keeps the session it had, so the next launch brings it back.
            if !self.quitting && !self.windows.isEmpty { self.saveSession() }
        }
        w.onChange = { [weak self] in self?.saveSession() }
        windows.append(w)
        if let host = tabOf, let win = w.window { host.addTabbedWindow(win, ordered: .above) }
        w.showWindow(nil)
        w.window?.makeKeyAndOrderFront(nil)
        return w
    }

    var front: WebWindow? { windows.first { $0.window?.isKeyWindow == true } ?? windows.first { $0.window?.isMainWindow == true } ?? windows.last }

    // open shows a daemon path (or a daemon URL) in the front window; other URLs go to the browser.
    func open(_ link: String) {
        var path = link
        if let u = URL(string: link), u.scheme != nil {
            guard isDaemon(u) else { openExternal(u); return }
            path = daemonPath(u)
        }
        if !path.hasPrefix("/") { path = "/" + path }
        if let w = front { w.go(path); w.window?.makeKeyAndOrderFront(nil) } else { newWindow(path: path, tabOf: nil, autosave: "AgentMonitorMain") }
        NSApp.activate(ignoringOtherApps: true)
    }

    // MARK: session

    // The session is a list of tab groups, each a list of page paths, in window order.
    func saveSession() {
        var groups: [[String]] = []
        var seen = Set<ObjectIdentifier>()
        for w in windows {
            guard let win = w.window else { continue }
            let members = win.tabGroup?.windows ?? [win]
            guard let first = members.first, seen.insert(ObjectIdentifier(first)).inserted else { continue }
            let paths = members.compactMap { m in windows.first { $0.window === m }?.path }
            if !paths.isEmpty { groups.append(paths) }
        }
        if !groups.isEmpty { UserDefaults.standard.set(groups, forKey: AppDelegate.sessionKey) }
    }

    func restoreSession() {
        let groups = (UserDefaults.standard.array(forKey: AppDelegate.sessionKey) as? [[String]])?.filter { !$0.isEmpty } ?? []
        guard !groups.isEmpty else { newWindow(path: "/w", tabOf: nil, autosave: "AgentMonitorMain"); return }
        var budget = AppDelegate.maxRestore
        for (i, g) in groups.enumerated() where budget > 0 {
            let host = newWindow(path: g[0], tabOf: nil, autosave: i == 0 ? "AgentMonitorMain" : "AgentMonitorWindow\(i)")
            budget -= 1
            for p in g.dropFirst() where budget > 0 {
                newWindow(path: p, tabOf: host.window)
                budget -= 1
            }
        }
        windows.first?.window?.makeKeyAndOrderFront(nil)
    }

    // MARK: badge

    private var badgeQueued = false

    // badgeSoon folds a burst of stream items into one refresh.
    func badgeSoon() {
        guard !badgeQueued else { return }
        badgeQueued = true
        DispatchQueue.main.asyncAfter(deadline: .now() + 1) { [weak self] in
            self?.badgeQueued = false
            self?.badge()
        }
    }

    // Dock badge: GET /api/needs-you count plus unread alerts.
    func badge() {
        guard let u = URL(string: kBase + "/api/needs-you") else { return }
        var r = URLRequest(url: u, timeoutInterval: 5)
        authorized(&r)
        URLSession.shared.dataTask(with: r) { d, _, _ in
            guard let a = d.flatMap({ try? JSONDecoder().decode(NeedsYouList.self, from: $0) }) else { return }
            DispatchQueue.main.async { AppDelegate.setBadge(a.count ?? a.items.count) }
        }.resume()
    }

    /// The Dock badge is the needs-you count, as the iPhone's Approvals badge; unread alerts have their own page.
    static func setBadge(_ n: Int) {
        NSApp.dockTile.badgeLabel = n > 0 ? String(n) : nil
    }

    // MARK: actions

    @objc func newMain() { newWindow(path: "/w", tabOf: nil) }
    @objc func newTab() { newWindow(path: "/w", tabOf: front?.window ?? NSApp.keyWindow) }
    @objc func reload() { front?.web.reload() }
    @objc func hardReload() { front?.web.reloadFromOrigin() }
    @objc func back() { front?.web.goBack() }
    @objc func forward() { front?.web.goForward() }
    @objc func zoomIn() { if let w = front { w.web.pageZoom = min(2, w.web.pageZoom + 0.1) } }
    @objc func zoomOut() { if let w = front { w.web.pageZoom = max(0.6, w.web.pageZoom - 0.1) } }
    @objc func zoomReset() { front?.web.pageZoom = 1 }
    @objc func goThreads() { front?.go("/w") }
    @objc func goSearch() { front?.go("/w/search") }
    @objc func findInToolbar() { front?.focusSearch() }
    @objc func goNew() { front?.go("/w/new") }
    @objc func goAlerts() { front?.go("/needs") }
    @objc func goSettings() { front?.go("/settings") }
    @objc func attach() { if let w = front { w.attach() } else { NSSound.beep() } }
    @objc func toggleNotifications() { notifier.enabled.toggle() }
    @objc func goPath(_ i: NSMenuItem) { if let p = i.representedObject as? String { front?.go(p) } }
    @objc func pickAppearance(_ i: NSMenuItem) {
        guard let m = i.representedObject as? String else { return }
        appearance = m
        windows.forEach { $0.setTheme(m) }
        approvals.setTheme(m)
    }
    @objc func toggleApprovals() { approvals.toggle(fromHotkey: false) }
    @objc func toggleApprovalsAuto() { approvals.autoShow.toggle() }
    @objc func toggleTouchID() { TouchIDGate.enabled.toggle() }
    @objc func toggleApproveKeys() {
        approveKeys.toggle()
        let on = approveKeys ? "true" : "false"
        windows.forEach { $0.web.evaluateJavaScript("window.__amApproveKeys && window.__amApproveKeys(\(on))", completionHandler: nil) }
    }
    @objc func toggleBar(_ i: NSMenuItem) {
        guard let b = i.representedObject as? String, let w = front else { return NSSound.beep() }
        w.web.evaluateJavaScript("window.__amToggle ? window.__amToggle('\(b)') : null") { [weak self] r, _ in self?.layoutRead(r) }
    }
    func layoutRead(_ r: Any?) {
        guard let d = r as? [String: Any] else { return }
        for (k, v) in d { if let on = v as? Bool { layout[k] = on } }
        for (k, i) in layoutItems { i.title = barTitle(k) }
        windows.forEach { $0.showLayout(layout) }
    }
    func userContentController(_ c: WKUserContentController, didReceive m: WKScriptMessage) {
        if m.name == "layout" { layoutRead(m.body) }
    }
    func barTitle(_ k: String) -> String {
        let name = k == "left" ? "Thread List" : k == "right" ? "Details" : "Top Bar"
        return (layout[k] == true ? "Show " : "Hide ") + name
    }
    func appearanceRead(_ m: String) {
        appearance = m
        windows.forEach { $0.applyAppearance(m) }
    }

    // Fresh items each call: a menu item can live in one menu only.
    func menuItems(_ k: MenuKind) -> [NSMenuItem] {
        func path(_ t: String, _ p: String) -> NSMenuItem {
            let i = NSMenuItem(title: t, action: #selector(goPath(_:)), keyEquivalent: "")
            i.representedObject = p
            i.target = self
            return i
        }
        func header(_ t: String) -> NSMenuItem {
            if #available(macOS 14.0, *) { return .sectionHeader(title: t) }
            let i = NSMenuItem(title: t, action: nil, keyEquivalent: "")
            i.isEnabled = false
            return i
        }
        switch k {
        case .appearance:
            return kAppearances.map { t, m in
                let i = NSMenuItem(title: t, action: #selector(pickAppearance(_:)), keyEquivalent: "")
                i.representedObject = m
                i.target = self
                return i
            }
        case .more:
            let keys = NSMenuItem(title: "Single-Key Approvals (a, d)", action: #selector(toggleApproveKeys), keyEquivalent: "")
            keys.target = self
            return [header("Other Views")] + kOtherViews.map(path) + [.separator(), header("Tools")] + kTools.map(path)
                + [.separator(), header("Settings")] + kSettingsPages.map(path)
                + [.separator(), keys]
        }
    }

    // The page's shortcuts change the layout too, so read it again each time the View menu opens.
    func menuNeedsUpdate(_ m: NSMenu) {
        front?.web.evaluateJavaScript("window.__amLayout ? window.__amLayout() : null") { [weak self] r, _ in self?.layoutRead(r) }
    }

    @objc func openInBrowser() { if let u = front?.web.url { openExternal(u) } }
    @objc func copyLink() {
        guard let u = front?.web.url else { return }
        NSPasteboard.general.clearContents()
        NSPasteboard.general.setString(u.absoluteString, forType: .string)
    }

    func validateMenuItem(_ item: NSMenuItem) -> Bool {
        switch item.action {
        case #selector(toggleNotifications):
            item.state = notifier.enabled ? .on : .off
            return Bundle.main.bundleIdentifier != nil
        case #selector(pickAppearance(_:)):
            item.state = (item.representedObject as? String) == appearance ? .on : .off
            return true
        case #selector(toggleApproveKeys):
            item.state = approveKeys ? .on : .off
            return true
        case #selector(toggleApprovals):
            item.state = approvals.isShown ? .on : .off
            return true
        case #selector(toggleTouchID):
            item.state = TouchIDGate.enabled ? .on : .off
            return true
        case #selector(toggleApprovalsAuto):
            item.state = approvals.autoShow ? .on : .off
            return true
        case #selector(toggleBar(_:)):
            if let k = item.representedObject as? String { item.title = barTitle(k) }
            return front != nil
        case #selector(back): return front?.web.canGoBack ?? false
        case #selector(forward): return front?.web.canGoForward ?? false
        default: return true
        }
    }

    func buildMenu() {
        let main = NSMenu()
        func menu(_ title: String, _ items: [NSMenuItem]) -> NSMenu {
            let top = NSMenuItem()
            let m = NSMenu(title: title)
            items.forEach { m.addItem($0) }
            top.submenu = m
            main.addItem(top)
            return m
        }
        func mine(_ t: String, _ s: Selector, _ k: String, _ mods: NSEvent.ModifierFlags = .command) -> NSMenuItem {
            let i = NSMenuItem(title: t, action: s, keyEquivalent: k)
            i.keyEquivalentModifierMask = mods
            i.target = self
            return i
        }
        func submenu(_ t: String, _ items: [NSMenuItem]) -> NSMenuItem {
            let i = NSMenuItem(title: t, action: nil, keyEquivalent: "")
            let m = NSMenu(title: t)
            items.forEach { m.addItem($0) }
            i.submenu = m
            return i
        }
        // Standard actions go up the responder chain (web view, window, app).
        func std(_ t: String, _ s: String, _ k: String, _ mods: NSEvent.ModifierFlags = .command) -> NSMenuItem {
            let i = NSMenuItem(title: t, action: Selector(s), keyEquivalent: k)
            i.keyEquivalentModifierMask = mods
            return i
        }
        _ = menu("agent-monitor", [
            std("About agent-monitor", "orderFrontStandardAboutPanel:", ""),
            .separator(),
            mine("Settings…", #selector(goSettings), ","),
            mine("Show Notifications", #selector(toggleNotifications), ""),
            mine("Show Approvals Automatically", #selector(toggleApprovalsAuto), ""),
            mine("Touch ID for Approvals", #selector(toggleTouchID), ""),
            .separator(),
            std("Hide agent-monitor", "hide:", "h"),
            std("Hide Others", "hideOtherApplications:", "h", [.command, .option]),
            std("Show All", "unhideAllApplications:", ""),
            .separator(),
            std("Quit agent-monitor", "terminate:", "q"),
        ])
        _ = menu("File", [
            mine("New Window", #selector(newMain), "n"),
            mine("New Tab", #selector(newTab), "t"),
            mine("New Thread", #selector(goNew), "n", [.command, .shift]),
            .separator(),
            mine("Attach…", #selector(attach), "a", [.command, .shift]),
            .separator(),
            mine("Open in Browser", #selector(openInBrowser), "o", [.command, .shift]),
            mine("Copy Link", #selector(copyLink), "c", [.command, .shift]),
            .separator(),
            std("Close", "performClose:", "w"),
        ])
        _ = menu("Edit", [
            std("Undo", "undo:", "z"), std("Redo", "redo:", "z", [.command, .shift]), .separator(),
            std("Cut", "cut:", "x"), std("Copy", "copy:", "c"), std("Paste", "paste:", "v"),
            std("Select All", "selectAll:", "a"),
            .separator(),
            mine("Search Transcripts", #selector(findInToolbar), "f", [.command, .option]),
        ])
        func bar(_ k: String, _ mods: NSEvent.ModifierFlags) -> NSMenuItem {
            let i = mine(barTitle(k), #selector(toggleBar(_:)), "\\", mods)
            i.representedObject = k
            layoutItems[k] = i
            return i
        }
        let view = menu("View", [
            mine("Reload", #selector(reload), "r"),
            mine("Reload Ignoring Cache", #selector(hardReload), "r", [.command, .shift]),
            .separator(),
            mine("Actual Size", #selector(zoomReset), "0"),
            mine("Zoom In", #selector(zoomIn), "+"),
            mine("Zoom Out", #selector(zoomOut), "-"),
            .separator(),
            submenu("Appearance", menuItems(.appearance)),
            .separator(),
            bar("left", .command),
            bar("right", [.command, .option]),
            bar("top", [.command, .shift]),
            .separator(),
            std("Show Toolbar", "toggleToolbarShown:", "t", [.command, .option]),
            std("Enter Full Screen", "toggleFullScreen:", "f", [.command, .control]),
        ])
        view.delegate = self
        _ = menu("Go", [
            mine("Back", #selector(back), "["),
            mine("Forward", #selector(forward), "]"),
            .separator(),
            mine("Threads", #selector(goThreads), "1"),
            mine("Needs you", #selector(goAlerts), "2"),
            mine("Search", #selector(goSearch), "3"),
            mine("Settings", #selector(goSettings), "4"),
            .separator(),
        ] + menuItems(.more))
        NSApp.windowsMenu = menu("Window", [
            std("Minimize", "performMiniaturize:", "m"),
            std("Zoom", "performZoom:", ""),
            .separator(),
            std("Show Previous Tab", "selectPreviousTab:", "{"),
            std("Show Next Tab", "selectNextTab:", "}"),
            .separator(),
            mine("Approvals", #selector(toggleApprovals), " ", [.control, .option]),
        ])
        NSApp.mainMenu = main
    }
}

let app = NSApplication.shared
let delegate = AppDelegate()
app.delegate = delegate
app.setActivationPolicy(.regular)
app.run()
