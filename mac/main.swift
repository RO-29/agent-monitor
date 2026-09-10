// AgentMonitor — a native macOS window hosting the agent-monitor dashboard.
//
// The dashboard lives at the daemon's `/` page. In a browser it is one tab among
// dozens and hard to find; here it is an app you can summon with one keystroke
// from anywhere (⌥⌘A by default), and dismiss with the same keystroke.
//
// It is a thin WKWebView shell — all the UI lives in the daemon's web/index.html,
// so changing the dashboard never needs a recompile here.
//
// The app ships the agent-monitor daemon in its own bundle and starts it when
// nothing is already listening, so opening the app is the only step. See
// daemon.swift for the ownership rules — in short, a daemon the app did not
// start is never touched.
//
// Build:  ./mac/build.sh          (produces AgentMonitor.app)
// Run:    open AgentMonitor.app   (or: ./mac/build.sh --run)
//
// Sibling app: tv/ builds AgentTV.app, the small always-on-top glance widget
// around `/tv`. Both can run at once.
//
// No LaunchAgent / login item — run it by hand (matches the daemon's policy).

import Cocoa
import WebKit

let kPort = Int(ProcessInfo.processInfo.environment["AGENT_MONITOR_PORT"] ?? "") ?? 7777
let kURL = ProcessInfo.processInfo.environment["AGENT_MONITOR_APP_URL"] ?? "http://127.0.0.1:\(kPort)/"
let kDefaultHotKey = "opt+cmd+a"

/// Only supervise a daemon when the dashboard we load is on this machine.
/// Pointed at a Tailscale host, the app must not start a local daemon that
/// nothing will read.
let kManagesDaemon: Bool = {
    guard let host = URL(string: kURL)?.host else { return false }
    return host == "127.0.0.1" || host == "localhost" || host == "::1"
}()

final class AppDelegate: NSObject, NSApplicationDelegate, NSWindowDelegate,
                         WKNavigationDelegate, WKScriptMessageHandler {
    var window: NSWindow!
    var web: WKWebView!
    var statusItem: NSStatusItem!
    var retryTimer: Timer?
    var hotKey: GlobalHotKey?
    var hotKeyLabel: String?
    var daemon: DaemonSupervisor?
    var signalSources: [DispatchSourceSignal] = []

    func applicationDidFinishLaunching(_ note: Notification) {
        // Regular app → Dock icon and ⌘Tab entry, so the dashboard stays
        // reachable even if you forget the hotkey or the menu-bar icon is
        // hidden behind the notch.
        NSApp.setActivationPolicy(.regular)

        makeWindow()
        makeWebView()
        load()
        startDaemon()

        installSignalHandlers()
        installHotKey()   // the point of this app: summon from anywhere
        installMenu()
        installStatusItem()

        window.makeKeyAndOrderFront(nil)
        NSApp.activate(ignoringOtherApps: true)
    }

    // ── window ──────────────────────────────────────────────────────────────

    /// A standard titled window: resizable, minimizable, and remembered across
    /// launches. Unlike the AgentTV widget this is not borderless or floating —
    /// the dashboard is a full working surface, so it should behave like any
    /// other Mac app window (Mission Control, window snapping, ⌘Tab).
    private func makeWindow() {
        let size = NSSize(width: 1200, height: 800)
        window = NSWindow(contentRect: NSRect(origin: .zero, size: size),
                          styleMask: [.titled, .closable, .miniaturizable, .resizable],
                          backing: .buffered, defer: false)
        window.title = "Agent Monitor"
        window.minSize = NSSize(width: 720, height: 480)
        window.delegate = self
        // Closing must not destroy the window — the hotkey and the menu-bar
        // icon both need to bring the same window back.
        window.isReleasedWhenClosed = false
        window.center()
        // AppKit persists position and size under this name in UserDefaults.
        _ = window.setFrameAutosaveName("AgentMonitorMain")
        window.tabbingMode = .disallowed
    }

    private func makeWebView() {
        let cfg = WKWebViewConfiguration()
        cfg.userContentController.add(self, name: "open")   // open URLs in default browser
        cfg.userContentController.add(self, name: "quit")   // quit from JS

        // NOTE: deliberately the *default*, persistent data store — unlike
        // tv/main.swift, which uses .nonPersistent(). The dashboard keeps user
        // settings in localStorage (theme, sound, notification mute, collapsed
        // sections, auto-register blocks); a non-persistent store would reset
        // all of them on every launch. Page freshness is already handled by the
        // daemon, which serves `/` with Cache-Control: no-store.

        web = WKWebView(frame: .zero, configuration: cfg)
        web.navigationDelegate = self
        web.allowsBackForwardNavigationGestures = false
        window.contentView = web
    }

    // ── daemon ──────────────────────────────────────────────────────────────

    /// Brings up the dashboard's server. The web view is already retrying on
    /// its own, so this only has to kick off the work and reload once the
    /// answer is known.
    private func startDaemon() {
        guard kManagesDaemon else {
            warn("pointed at \(kURL) — not a local address, so no daemon is started")
            return
        }
        let sup = DaemonSupervisor(port: kPort)
        daemon = sup
        sup.ensureRunning { [weak self] ownership in
            if case .failed(let msg) = ownership { self?.warn(msg) }
            self?.load()
        }
    }

    /// Only ever stops a daemon this app started; see daemon.swift.
    func applicationWillTerminate(_ note: Notification) {
        daemon?.stopIfOwned()
    }

    /// Route SIGTERM/SIGINT through the normal quit path. A default signal
    /// disposition kills the app outright, skipping applicationWillTerminate
    /// and orphaning a daemon this app started. `kill`, a `pkill`, and a
    /// terminal Ctrl-C all land here.
    private func installSignalHandlers() {
        for sig in [SIGTERM, SIGINT] {
            signal(sig, SIG_IGN)   // hand the signal to the dispatch source
            let src = DispatchSource.makeSignalSource(signal: sig, queue: .main)
            src.setEventHandler { NSApp.terminate(nil) }
            src.resume()
            signalSources.append(src)
        }
    }

    @objc func restartDaemon() {
        guard let sup = daemon else { return }
        sup.restart { [weak self] ownership in
            if case .failed(let msg) = ownership { self?.warn(msg) }
            self?.load()
        }
    }

    @objc func openDaemonLog() {
        guard let sup = daemon else { return }
        NSWorkspace.shared.open(sup.logURL)
    }

    /// `agent-monitor install` edits the agent CLIs' own config files, so it
    /// asks first and reports what happened. Deliberately not run at launch:
    /// writing to another tool's config is not something an app should do
    /// behind the user's back.
    @objc func installHooks() {
        guard let sup = daemon else { return }
        let confirm = NSAlert()
        confirm.messageText = "Wire agent-monitor into your agent CLIs?"
        confirm.informativeText = [
            "This runs `agent-monitor install`, which adds hooks and MCP entries to:",
            "",
            "    ~/.claude/settings.json",
            "    ~/.claude.json",
            "    ~/.codex/config.toml",
            "",
            "Existing files are backed up first, and running it again is harmless.",
        ].joined(separator: "\n")
        confirm.alertStyle = .informational
        confirm.addButton(withTitle: "Install")
        confirm.addButton(withTitle: "Cancel")
        guard confirm.runModal() == .alertFirstButtonReturn else { return }

        sup.runInstall { ok, output in
            let done = NSAlert()
            done.messageText = ok ? "agent-monitor installed" : "Install failed"
            let trimmed = output.trimmingCharacters(in: .whitespacesAndNewlines)
            done.informativeText = trimmed.isEmpty ? (ok ? "Done." : "The installer produced no output.") : trimmed
            done.alertStyle = ok ? .informational : .warning
            done.addButton(withTitle: "OK")
            done.runModal()
        }
    }

    func load() {
        guard let url = URL(string: kURL) else {
            FileHandle.standardError.write(Data("AgentMonitor: invalid URL \(kURL)\n".utf8))
            return
        }
        web.load(URLRequest(url: url, cachePolicy: .reloadIgnoringLocalAndRemoteCacheData))
    }

    // ── show / hide ─────────────────────────────────────────────────────────

    /// True toggle: pressing the hotkey while the dashboard is already in front
    /// puts it away, so one key both summons and dismisses.
    @objc func toggleWindow() {
        if window.isVisible && !window.isMiniaturized && NSApp.isActive {
            NSApp.hide(nil)
        } else {
            showWindow()
        }
    }

    /// Bring the window forward without moving it — the remembered frame is
    /// where the user put it.
    @objc func showWindow() {
        if window.isMiniaturized { window.deminiaturize(nil) }
        window.makeKeyAndOrderFront(nil)
        NSApp.activate(ignoringOtherApps: true)
    }

    @objc func hideWindow() { NSApp.hide(nil) }

    /// The red close button hides the window rather than discarding it.
    func windowShouldClose(_ sender: NSWindow) -> Bool {
        NSApp.hide(nil)
        return false
    }

    /// Re-opening the app (Finder double-click or `open AgentMonitor.app`) while
    /// it is already running brings the window back.
    func applicationShouldHandleReopen(_ sender: NSApplication, hasVisibleWindows flag: Bool) -> Bool {
        showWindow()
        return true
    }

    // ── global hotkey ───────────────────────────────────────────────────────

    /// Registers the summon/dismiss hotkey. A bad spec or a combination already
    /// claimed by another app is reported on stderr and is not fatal: the
    /// menu-bar icon and the Dock icon still work.
    private func installHotKey() {
        let raw = ProcessInfo.processInfo.environment["AGENT_MONITOR_HOTKEY"] ?? kDefaultHotKey
        guard let spec = HotKeySpec.parse(raw) else {
            warn("cannot parse AGENT_MONITOR_HOTKEY=\"\(raw)\" — no global hotkey. "
                 + "Expected something like \"opt+cmd+a\". Use the menu-bar icon instead.")
            return
        }
        guard let key = GlobalHotKey(spec: spec, action: { [weak self] in self?.toggleWindow() }) else {
            warn("could not register hotkey \(spec.display) — no global hotkey. "
                 + "Set AGENT_MONITOR_HOTKEY to a different combination. "
                 + "Use the menu-bar icon meanwhile.")
            return
        }
        hotKey = key
        hotKeyLabel = spec.display
    }

    private func warn(_ msg: String) {
        FileHandle.standardError.write(Data("AgentMonitor: \(msg)\n".utf8))
    }

    // ── menu bar ────────────────────────────────────────────────────────────

    /// Menu-bar presence so the window can always be brought back, whatever
    /// happened to the hotkey. Left-click toggles; right-click opens a menu.
    private func installStatusItem() {
        statusItem = NSStatusBar.system.statusItem(withLength: NSStatusItem.variableLength)
        if let b = statusItem.button {
            // A different glyph from AgentTV's waveform, so the two menu-bar
            // icons are told apart at a glance.
            b.image = NSImage(systemSymbolName: "rectangle.3.group",
                              accessibilityDescription: "Agent Monitor")
            b.toolTip = hotKeyLabel.map { "Agent Monitor — \($0) to show/hide, right-click for menu" }
                ?? "Agent Monitor — click to show/hide, right-click for menu"
            b.target = self
            b.action = #selector(statusClicked)
            b.sendAction(on: [.leftMouseUp, .rightMouseUp])
        }
    }

    @objc func statusClicked() {
        let ev = NSApp.currentEvent
        if ev?.type == .rightMouseUp || ev?.modifierFlags.contains(.control) == true {
            let m = NSMenu()
            let showHide = NSMenuItem(title: window.isVisible && NSApp.isActive ? "Hide" : "Show",
                                      action: #selector(toggleWindow), keyEquivalent: "")
            m.addItem(showHide)
            m.addItem(withTitle: "Reload", action: #selector(reload), keyEquivalent: "")
            if kManagesDaemon {
                m.addItem(withTitle: "Restart Daemon", action: #selector(restartDaemon), keyEquivalent: "")
                m.addItem(withTitle: "Open Daemon Log", action: #selector(openDaemonLog), keyEquivalent: "")
            }
            if let label = hotKeyLabel {
                m.addItem(.separator())
                let hint = NSMenuItem(title: "Shortcut: \(label)", action: nil, keyEquivalent: "")
                hint.isEnabled = false
                m.addItem(hint)
            }
            m.addItem(.separator())
            m.addItem(withTitle: "Quit Agent Monitor",
                      action: #selector(NSApplication.terminate(_:)), keyEquivalent: "q")
            statusItem.menu = m
            statusItem.button?.performClick(nil)
            statusItem.menu = nil            // reset so a left-click toggles again
        } else {
            toggleWindow()
        }
    }

    private func installMenu() {
        let main = NSMenu()

        let appItem = NSMenuItem()
        let appMenu = NSMenu()
        appMenu.addItem(withTitle: "Reload", action: #selector(reload), keyEquivalent: "r")
        if kManagesDaemon {
            let restart = NSMenuItem(title: "Restart Daemon", action: #selector(restartDaemon), keyEquivalent: "r")
            restart.keyEquivalentModifierMask = [.command, .shift]
            appMenu.addItem(restart)
            appMenu.addItem(withTitle: "Open Daemon Log", action: #selector(openDaemonLog), keyEquivalent: "")
            appMenu.addItem(.separator())
            appMenu.addItem(withTitle: "Install Agent Hooks…", action: #selector(installHooks), keyEquivalent: "")
        }
        if let label = hotKeyLabel {
            let hint = NSMenuItem(title: "Show/Hide: \(label)", action: nil, keyEquivalent: "")
            hint.isEnabled = false
            appMenu.addItem(hint)
        }
        appMenu.addItem(.separator())
        appMenu.addItem(withTitle: "Hide Agent Monitor", action: #selector(hideWindow), keyEquivalent: "h")
        appMenu.addItem(withTitle: "Close Window", action: #selector(hideWindow), keyEquivalent: "w")
        appMenu.addItem(.separator())
        appMenu.addItem(withTitle: "Quit Agent Monitor",
                        action: #selector(NSApplication.terminate(_:)), keyEquivalent: "q")
        appItem.submenu = appMenu
        main.addItem(appItem)

        // Edit menu, so copy/paste and select-all work inside the web view.
        let editItem = NSMenuItem()
        let editMenu = NSMenu(title: "Edit")
        editMenu.addItem(withTitle: "Cut", action: #selector(NSText.cut(_:)), keyEquivalent: "x")
        editMenu.addItem(withTitle: "Copy", action: #selector(NSText.copy(_:)), keyEquivalent: "c")
        editMenu.addItem(withTitle: "Paste", action: #selector(NSText.paste(_:)), keyEquivalent: "v")
        editMenu.addItem(withTitle: "Select All", action: #selector(NSText.selectAll(_:)), keyEquivalent: "a")
        editItem.submenu = editMenu
        main.addItem(editItem)

        NSApp.mainMenu = main
    }

    @objc func reload() { load() }

    // ── navigation ──────────────────────────────────────────────────────────

    /// Keep the app on the dashboard. Anything pointing elsewhere (docs links,
    /// a repo URL) opens in the default browser instead, so this shell never
    /// turns into a stray general-purpose browser.
    ///
    /// Only top-level navigations are filtered; subresources such as the
    /// dashboard's web font are unaffected.
    func webView(_ w: WKWebView,
                 decidePolicyFor navigationAction: WKNavigationAction,
                 decisionHandler: @escaping (WKNavigationActionPolicy) -> Void) {
        guard let url = navigationAction.request.url else {
            decisionHandler(.cancel)
            return
        }
        let allowedHost = URL(string: kURL)?.host
        if url.host == nil || url.host == allowedHost {
            decisionHandler(.allow)
        } else if let scheme = url.scheme, scheme == "http" || scheme == "https" {
            NSWorkspace.shared.open(url)
            decisionHandler(.cancel)
        } else {
            decisionHandler(.cancel)
        }
    }

    // If the daemon is not up yet, keep retrying so the app "just works" once
    // agent-monitor starts.
    func webView(_ w: WKWebView, didFailProvisionalNavigation n: WKNavigation!, withError e: Error) {
        scheduleRetry()
    }
    func webView(_ w: WKWebView, didFail n: WKNavigation!, withError e: Error) {
        scheduleRetry()
    }
    private func scheduleRetry() {
        retryTimer?.invalidate()
        retryTimer = Timer.scheduledTimer(withTimeInterval: 1.8, repeats: false) { [weak self] _ in
            self?.load()
        }
    }

    // JS bridge: window.webkit.messageHandlers.open/quit
    func userContentController(_ c: WKUserContentController, didReceive m: WKScriptMessage) {
        switch m.name {
        case "open":
            if let s = m.body as? String, let u = URL(string: s) { NSWorkspace.shared.open(u) }
        case "quit":
            NSApp.terminate(nil)
        default:
            break
        }
    }
}

let app = NSApplication.shared
let delegate = AppDelegate()
app.delegate = delegate
app.run()
