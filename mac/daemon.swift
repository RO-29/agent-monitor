// Daemon supervision for AgentMonitor.app.
//
// The dashboard is served by the agent-monitor Go daemon, so the app ships the
// daemon binary inside its own bundle and starts it when needed. That removes
// the terminal from the loop entirely: open the app, get a dashboard.
//
// Two rules shape everything here:
//
//  1. Never fight a daemon we did not start. If one is already listening, the
//     app adopts it and leaves it completely alone — including on quit. The
//     daemon is often started by hand with its own flags, and it owns the MCP
//     permission servers that agent CLIs depend on; killing someone else's
//     daemon would close those transports unrecoverably.
//  2. Never widen the daemon's exposure by accident. The app starts it on
//     loopback and strips AGENT_MONITOR_BIND, so launching the app cannot
//     publish a pane-driving API onto the network just because a shell profile
//     happened to export that variable. Exposure is opt-in through the
//     separately named AGENT_MONITOR_APP_BIND.

import AppKit
import Foundation

/// Starts, watches, and stops the bundled agent-monitor daemon.
final class DaemonSupervisor {
    /// How the daemon we are talking to came to exist.
    enum Ownership {
        case notStarted
        case adopted   // already running before we launched — hands off
        case owned     // we spawned it, so we stop it on quit
        case failed(String)
    }

    private(set) var ownership: Ownership = .notStarted

    private let healthURL: URL
    private let port: Int
    private var process: Process?
    private var logHandle: FileHandle?
    private var restarts = 0
    private var shuttingDown = false

    private let maxRestarts = 3
    private let probeTimeout: TimeInterval = 1.0

    /// `~/Library/Logs/AgentMonitor/daemon.log` — the macOS convention, and a
    /// fixed place to look when the daemon refuses to come up.
    let logURL: URL = FileManager.default
        .homeDirectoryForCurrentUser
        .appendingPathComponent("Library/Logs/AgentMonitor/daemon.log")

    /// The daemon binary shipped in `Contents/MacOS/agent-monitor`.
    var bundledDaemon: URL? { Bundle.main.url(forAuxiliaryExecutable: "agent-monitor") }

    init(port: Int) {
        self.port = port
        healthURL = URL(string: "http://127.0.0.1:\(port)/api/health")!
    }

    // ── lifecycle ───────────────────────────────────────────────────────────

    /// Adopts a running daemon if there is one, otherwise starts the bundled
    /// binary. `then` reports what happened, on the main queue.
    func ensureRunning(then: @escaping (Ownership) -> Void) {
        probe { [weak self] alive in
            guard let self else { return }
            if alive {
                self.ownership = .adopted
                self.log("adopted the daemon already listening on port \(self.port)")
            } else {
                self.spawn()
            }
            then(self.ownership)
        }
    }

    /// Stops the daemon only if this app started it. SIGTERM, not SIGKILL — the
    /// daemon flushes its session history to SQLite on SIGTERM.
    func stopIfOwned() {
        guard case .owned = ownership, let p = process, p.isRunning else { return }
        shuttingDown = true
        log("stopping the daemon this app started (pid \(p.processIdentifier))")
        p.terminate()
        // Brief, bounded wait so the flush lands before the app disappears.
        let deadline = Date().addingTimeInterval(3)
        while p.isRunning && Date() < deadline { usleep(50_000) }
        if p.isRunning { log("daemon did not exit within 3s — leaving it to the system") }
        try? logHandle?.close()
        logHandle = nil
    }

    /// Restarts a daemon we own. On an adopted one this only re-probes, since
    /// stopping someone else's daemon is not ours to do.
    func restart(then: @escaping (Ownership) -> Void) {
        switch ownership {
        case .owned:
            shuttingDown = true
            process?.terminate()
            process?.waitUntilExit()
            process = nil
            shuttingDown = false
            restarts = 0
            spawn()
            then(ownership)
        case .adopted:
            log("not restarting — this daemon was already running and is not ours to stop")
            then(ownership)
        case .notStarted, .failed:
            restarts = 0
            ensureRunning(then: then)
        }
    }

    // ── internals ───────────────────────────────────────────────────────────

    /// A short GET on /api/health. Any 2xx means something is serving; that is
    /// enough to decide not to start a second one.
    private func probe(_ done: @escaping (Bool) -> Void) {
        var req = URLRequest(url: healthURL)
        req.timeoutInterval = probeTimeout
        req.cachePolicy = .reloadIgnoringLocalAndRemoteCacheData
        URLSession.shared.dataTask(with: req) { _, response, _ in
            let ok = (response as? HTTPURLResponse).map { (200...299).contains($0.statusCode) } ?? false
            DispatchQueue.main.async { done(ok) }
        }.resume()
    }

    private func spawn() {
        guard let exe = bundledDaemon else {
            fail("no agent-monitor binary in the app bundle — rebuild with ./mac/build.sh")
            return
        }

        let p = Process()
        p.executableURL = exe
        p.arguments = []
        // Home, not the app bundle: the daemon writes ~/.agent-monitor/ and
        // resolves repo paths relative to nothing else.
        p.currentDirectoryURL = FileManager.default.homeDirectoryForCurrentUser

        var env = ProcessInfo.processInfo.environment
        env["AGENT_MONITOR_PORT"] = String(port)
        // Rule 2: AGENT_MONITOR_BIND is dropped, however it got into the
        // environment. Exposing the daemon takes the deliberately distinct
        // AGENT_MONITOR_APP_BIND, so a value exported for a CLI session can
        // never make the app publish the API.
        env.removeValue(forKey: "AGENT_MONITOR_BIND")
        if let bind = ProcessInfo.processInfo.environment["AGENT_MONITOR_APP_BIND"],
           !bind.isEmpty {
            env["AGENT_MONITOR_BIND"] = bind
            log("AGENT_MONITOR_APP_BIND=\(bind) — the daemon will also listen off-loopback. "
                + "Anyone who can reach it can drive every registered tmux pane.")
        }
        p.environment = env

        guard let handle = openLog() else {
            fail("cannot open \(logURL.path) for writing")
            return
        }
        p.standardOutput = handle
        p.standardError = handle

        p.terminationHandler = { [weak self] proc in
            DispatchQueue.main.async { self?.daemonExited(status: proc.terminationStatus) }
        }

        do {
            try p.run()
            process = p
            ownership = .owned
            log("started the bundled daemon on port \(port) (pid \(p.processIdentifier))")
        } catch {
            fail("could not start the daemon: \(error.localizedDescription)")
        }
    }

    /// An unexpected exit is retried a few times — a transient failure (the
    /// port briefly held by a daemon that is still shutting down) should not
    /// leave the app permanently blank. A persistent one stops retrying and
    /// says where to look, rather than spinning.
    private func daemonExited(status: Int32) {
        guard !shuttingDown else { return }
        process = nil
        restarts += 1
        guard restarts <= maxRestarts else {
            fail("daemon exited \(restarts) times (last status \(status)) — giving up. See \(logURL.path)")
            return
        }
        log("daemon exited with status \(status) — restart \(restarts) of \(maxRestarts) in 1.5s")
        DispatchQueue.main.asyncAfter(deadline: .now() + 1.5) { [weak self] in
            guard let self, !self.shuttingDown else { return }
            // Re-probe first: the exit may have been a second daemon losing the
            // race for the port, in which case there is one running already.
            self.probe { alive in
                if alive {
                    self.ownership = .adopted
                    self.log("another daemon holds port \(self.port) — adopting it")
                } else {
                    self.spawn()
                }
            }
        }
    }

    private func openLog() -> FileHandle? {
        if let h = logHandle { return h }
        let dir = logURL.deletingLastPathComponent()
        try? FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
        if !FileManager.default.fileExists(atPath: logURL.path) {
            FileManager.default.createFile(atPath: logURL.path, contents: nil)
        }
        // Keep the log from growing without bound across many launches.
        if let size = try? FileManager.default.attributesOfItem(atPath: logURL.path)[.size] as? Int,
           size > 5 * 1024 * 1024 {
            try? Data().write(to: logURL)
        }
        guard let h = try? FileHandle(forWritingTo: logURL) else { return nil }
        h.seekToEndOfFile()
        logHandle = h
        return h
    }

    private func fail(_ msg: String) {
        ownership = .failed(msg)
        log("ERROR " + msg)
    }

    private func log(_ msg: String) {
        FileHandle.standardError.write(Data("AgentMonitor: \(msg)\n".utf8))
    }

    // ── one-time setup ──────────────────────────────────────────────────────

    /// Runs `agent-monitor install`, which wires the hooks and MCP entries into
    /// the agent CLIs' own config files. It is idempotent and backs up what it
    /// edits, but it writes outside this app, so the caller must confirm with
    /// the user first — this method does not ask.
    func runInstall(completion: @escaping (Bool, String) -> Void) {
        guard let exe = bundledDaemon else {
            completion(false, "No agent-monitor binary in the app bundle. Rebuild with ./mac/build.sh.")
            return
        }
        let p = Process()
        p.executableURL = exe
        p.arguments = ["install"]
        p.currentDirectoryURL = FileManager.default.homeDirectoryForCurrentUser
        let pipe = Pipe()
        p.standardOutput = pipe
        p.standardError = pipe

        DispatchQueue.global(qos: .userInitiated).async {
            var output = ""
            do {
                try p.run()
                // Read before waiting: a full pipe buffer would deadlock the
                // child against a parent that is blocked in waitUntilExit.
                let data = pipe.fileHandleForReading.readDataToEndOfFile()
                p.waitUntilExit()
                output = String(decoding: data, as: UTF8.self)
            } catch {
                output = "Could not run the installer: \(error.localizedDescription)"
            }
            let ok = p.terminationStatus == 0
            DispatchQueue.main.async { completion(ok, output) }
        }
    }
}
