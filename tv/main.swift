// AgentTV — a tiny always-on-top macOS window that hosts the agent-monitor
// /tv glance board. A "peek-a-boo" widget: borderless, floating above every
// app and on every Space, dockless, draggable by its background.
//
// It's a thin WKWebView shell — all the UI lives in the daemon's /tv page, so
// rebuilding the web view never needs a recompile here.
//
// Build:  ./tv/build.sh        (produces AgentTV.app)
// Run:    open AgentTV.app     (or: ./tv/build.sh --run)
//
// No LaunchAgent / login item — run it by hand (matches the daemon's policy).

import Cocoa
import WebKit

let kURL = ProcessInfo.processInfo.environment["AGENT_TV_URL"] ?? "http://127.0.0.1:7777/tv"

// A non-activating floating NSPanel. Two reasons it's a panel, not a window:
//  1. Borderless windows refuse key status by default, which would stop the web
//     view from receiving clicks & keyboard — canBecomeKey fixes that.
//  2. A regular app's *main window* gets bound by macOS to the Space it opened
//     on, so .canJoinAllSpaces is ignored (it only shows on its origin desktop).
//     A panel is an auxiliary window that never becomes main, so it honors
//     .canJoinAllSpaces and spans every Space — while the app keeps its Dock
//     icon + ⌘Tab entry (.regular policy).
final class KeyableWindow: NSPanel {
    override var canBecomeKey: Bool { true }
    override var canBecomeMain: Bool { false }
}

// A grab strip: any click-drag on it moves the window. We drive the drag
// explicitly with performDrag so it works regardless of the webview below.
final class DragBar: NSView {
    override var mouseDownCanMoveWindow: Bool { false }   // ensure mouseDown reaches us
    override func mouseDown(with event: NSEvent) { window?.performDrag(with: event) }
}

// Decorative subview that lets clicks fall through to its parent (the DragBar),
// so the centered grabber doesn't create a dead zone in the drag strip.
final class Passthrough: NSView {
    override func hitTest(_ point: NSPoint) -> NSView? { nil }
}

final class AppDelegate: NSObject, NSApplicationDelegate, WKNavigationDelegate, WKScriptMessageHandler {
    var window: KeyableWindow!
    var web: WKWebView!
    var retryTimer: Timer?
    var statusItem: NSStatusItem!

    func applicationDidFinishLaunching(_ note: Notification) {
        // Regular app → Dock icon + ⌘Tab entry, so it's always launchable even
        // when the menu-bar status item hides behind the notch. All-Spaces
        // coverage is preserved by making the window a non-activating panel
        // (see KeyableWindow) rather than by dropping to accessory policy.
        NSApp.setActivationPolicy(.regular)

        // ── window: borderless, floating, on all Spaces ──
        let size = NSSize(width: 384, height: 560)
        let screen = NSScreen.main?.visibleFrame ?? NSRect(x: 0, y: 0, width: 1440, height: 900)
        let origin = NSPoint(x: screen.maxX - size.width - 24, y: screen.maxY - size.height - 24)

        window = KeyableWindow(
            contentRect: NSRect(origin: origin, size: size),
            // .nonactivatingPanel keeps the panel auxiliary (never main) so it
            // isn't pinned to one Space and doesn't steal focus from the app you
            // click away to — the key to spanning all Spaces under .regular.
            styleMask: [.borderless, .resizable, .fullSizeContentView, .nonactivatingPanel],
            backing: .buffered, defer: false)
        window.isReleasedWhenClosed = false
        window.isFloatingPanel = true            // auxiliary panel semantics
        window.hidesOnDeactivate = false         // stay put when app deactivates
        window.becomesKeyOnlyIfNeeded = false    // any click makes it key so WKWebView gets clicks/keys
        window.level = .floating                 // always on top
        // Show on every Space and over fullscreen apps. (No .stationary — that
        // pins it to the desktop like a widget and stops it floating over other
        // Spaces' windows, which is why it only showed on an empty desktop.)
        // As a non-activating panel this is honored under .regular policy.
        window.collectionBehavior = [.canJoinAllSpaces, .fullScreenAuxiliary]
        window.isMovableByWindowBackground = true                  // drag anywhere
        window.isOpaque = false
        window.backgroundColor = .clear
        window.hasShadow = true
        window.minSize = NSSize(width: 300, height: 240)

        // Rounded-corner container.
        let container = NSView(frame: NSRect(origin: .zero, size: size))
        container.wantsLayer = true
        container.layer?.cornerRadius = 14
        container.layer?.masksToBounds = true
        container.layer?.borderWidth = 1
        container.layer?.borderColor = NSColor(white: 1, alpha: 0.10).cgColor
        container.layer?.backgroundColor = NSColor(red: 0.035, green: 0.043, blue: 0.063, alpha: 1).cgColor
        window.contentView = container

        // ── drag bar ── The WKWebView fills the window and eats every click, so
        // `isMovableByWindowBackground` never sees a drag. This native strip is
        // the grab handle: click-drag it to move the window anywhere.
        let bar = DragBar()
        bar.translatesAutoresizingMaskIntoConstraints = false
        bar.wantsLayer = true
        bar.layer?.backgroundColor = NSColor(red: 0.06, green: 0.07, blue: 0.10, alpha: 1).cgColor
        container.addSubview(bar)

        let grip = Passthrough()   // centered grabber affordance; clicks fall through to the bar
        grip.translatesAutoresizingMaskIntoConstraints = false
        grip.wantsLayer = true
        grip.layer?.backgroundColor = NSColor(white: 1, alpha: 0.34).cgColor
        grip.layer?.cornerRadius = 2.5
        bar.addSubview(grip)

        let close = NSButton(title: "", target: self, action: #selector(hideWindow))
        close.translatesAutoresizingMaskIntoConstraints = false
        close.isBordered = false
        close.bezelStyle = .regularSquare
        close.image = NSImage(systemSymbolName: "xmark", accessibilityDescription: "Hide")
        close.contentTintColor = NSColor(white: 1, alpha: 0.5)
        close.toolTip = "Hide (bring back from the menu-bar icon)"
        bar.addSubview(close)

        // ── web view ──
        let cfg = WKWebViewConfiguration()
        cfg.userContentController.add(self, name: "open")   // open URLs in default browser
        cfg.userContentController.add(self, name: "quit")   // quit from JS
        // Never cache the /tv page — otherwise the widget keeps showing a stale
        // build after the daemon updates. Fresh fetch on every (re)load.
        cfg.websiteDataStore = .nonPersistent()
        web = WKWebView(frame: .zero, configuration: cfg)
        web.translatesAutoresizingMaskIntoConstraints = false
        web.navigationDelegate = self
        web.setValue(false, forKey: "drawsBackground")      // transparent → rounded corners show
        container.addSubview(web)

        NSLayoutConstraint.activate([
            bar.topAnchor.constraint(equalTo: container.topAnchor),
            bar.leadingAnchor.constraint(equalTo: container.leadingAnchor),
            bar.trailingAnchor.constraint(equalTo: container.trailingAnchor),
            bar.heightAnchor.constraint(equalToConstant: 30),
            grip.centerXAnchor.constraint(equalTo: bar.centerXAnchor),
            grip.centerYAnchor.constraint(equalTo: bar.centerYAnchor),
            grip.widthAnchor.constraint(equalToConstant: 54),
            grip.heightAnchor.constraint(equalToConstant: 5),
            close.centerYAnchor.constraint(equalTo: bar.centerYAnchor),
            close.trailingAnchor.constraint(equalTo: bar.trailingAnchor, constant: -6),
            close.widthAnchor.constraint(equalToConstant: 16),
            close.heightAnchor.constraint(equalToConstant: 16),
            web.topAnchor.constraint(equalTo: bar.bottomAnchor),
            web.leadingAnchor.constraint(equalTo: container.leadingAnchor),
            web.trailingAnchor.constraint(equalTo: container.trailingAnchor),
            web.bottomAnchor.constraint(equalTo: container.bottomAnchor),
        ])

        load()
        showHere()            // place on the screen the mouse is on, then show

        installStatusItem()   // menu-bar icon — the always-available way to bring it back
        installMenu()         // ⌘Q / ⌘R / ⌘W even as an accessory app

        // Keep the widget on whichever Space you switch to. .canJoinAllSpaces is
        // sometimes ignored for a .regular app's window (it stays on its origin
        // desktop); re-asserting the behavior + ordering front on each Space
        // change forces it onto the new active Space, so it's present on all.
        NSWorkspace.shared.notificationCenter.addObserver(
            self, selector: #selector(reassertAllSpaces),
            name: NSWorkspace.activeSpaceDidChangeNotification, object: nil)
    }

    @objc func reassertAllSpaces() {
        guard window != nil, window.isVisible else { return }
        window.collectionBehavior = [.canJoinAllSpaces, .fullScreenAuxiliary]
        window.orderFrontRegardless()   // pull onto the current Space without stealing focus
    }

    // Menu-bar presence so a dockless widget can always be re-shown after it's
    // hidden. Left-click toggles the window; right-click opens a menu.
    func installStatusItem() {
        statusItem = NSStatusBar.system.statusItem(withLength: NSStatusItem.variableLength)
        if let b = statusItem.button {
            b.image = NSImage(systemSymbolName: "waveform.path.ecg", accessibilityDescription: "AgentTV")
            b.toolTip = "AgentTV — live agent monitor · click to show/hide, right-click for menu"
            b.target = self
            b.action = #selector(statusClicked)
            b.sendAction(on: [.leftMouseUp, .rightMouseUp])
        }
    }

    @objc func statusClicked() {
        let ev = NSApp.currentEvent
        if ev?.type == .rightMouseUp || ev?.modifierFlags.contains(.control) == true {
            let m = NSMenu()
            m.addItem(withTitle: window.isVisible ? "Hide" : "Show", action: #selector(toggleWindow), keyEquivalent: "")
            m.addItem(withTitle: "Reload", action: #selector(reload), keyEquivalent: "")
            m.addItem(.separator())
            m.addItem(withTitle: "Quit AgentTV", action: #selector(NSApplication.terminate(_:)), keyEquivalent: "q")
            statusItem.menu = m
            statusItem.button?.performClick(nil)
            statusItem.menu = nil            // reset so a left-click toggles again
        } else {
            toggleWindow()
        }
    }

    @objc func toggleWindow() {
        if window.isVisible { window.orderOut(nil) }
        else { showHere() }
    }
    @objc func hideWindow() { window.orderOut(nil) }

    /// Bring the window to the screen the mouse is currently on (top-right), so
    /// it appears where you're working — key on multi-monitor / multi-Space.
    private func showHere() {
        let mouse = NSEvent.mouseLocation
        let screen = NSScreen.screens.first { NSMouseInRect(mouse, $0.frame, false) } ?? NSScreen.main ?? NSScreen.screens.first!
        let vf = screen.visibleFrame
        let s = window.frame.size
        window.setFrameOrigin(NSPoint(x: vf.maxX - s.width - 24, y: vf.maxY - s.height - 24))
        // Smooth fade-in so it slides into view rather than popping.
        window.alphaValue = 0
        window.makeKeyAndOrderFront(nil)
        NSApp.activate(ignoringOtherApps: true)
        NSAnimationContext.runAnimationGroup { ctx in
            ctx.duration = 0.20
            ctx.timingFunction = CAMediaTimingFunction(name: .easeOut)
            window.animator().alphaValue = 1
        }
    }

    // Re-opening the app (Finder double-click or `open AgentTV.app`) while it's
    // already running re-shows the window — another way back if it was hidden.
    func applicationShouldHandleReopen(_ sender: NSApplication, hasVisibleWindows flag: Bool) -> Bool {
        showHere(); return true
    }

    func load() { web.load(URLRequest(url: URL(string: kURL)!, cachePolicy: .reloadIgnoringLocalAndRemoteCacheData)) }

    // If the daemon isn't up yet, keep retrying so the widget "just works"
    // once agent-monitor starts.
    func webView(_ w: WKWebView, didFailProvisionalNavigation n: WKNavigation!, withError e: Error) { scheduleRetry() }
    func webView(_ w: WKWebView, didFail n: WKNavigation!, withError e: Error) { scheduleRetry() }
    func scheduleRetry() {
        retryTimer?.invalidate()
        retryTimer = Timer.scheduledTimer(withTimeInterval: 1.8, repeats: false) { [weak self] _ in self?.load() }
    }

    // JS bridge: window.webkit.messageHandlers.open/quit
    func userContentController(_ c: WKUserContentController, didReceive m: WKScriptMessage) {
        switch m.name {
        case "open":
            if let s = m.body as? String, let u = URL(string: s) { NSWorkspace.shared.open(u) }
        case "quit":
            NSApp.terminate(nil)
        default: break
        }
    }

    // Minimal menu so keyboard shortcuts work in an accessory app.
    func installMenu() {
        let main = NSMenu()
        let appItem = NSMenuItem()
        let appMenu = NSMenu()
        appMenu.addItem(withTitle: "Reload", action: #selector(reload), keyEquivalent: "r")
        appMenu.addItem(.separator())
        appMenu.addItem(withTitle: "Hide", action: #selector(NSApp.hide(_:)), keyEquivalent: "w")
        appMenu.addItem(withTitle: "Quit AgentTV", action: #selector(NSApplication.terminate(_:)), keyEquivalent: "q")
        appItem.submenu = appMenu
        main.addItem(appItem)
        NSApp.mainMenu = main
    }
    @objc func reload() { load() }
    @objc func quitApp() { NSApp.terminate(nil) }
}

let app = NSApplication.shared
let delegate = AppDelegate()
app.delegate = delegate
app.run()
