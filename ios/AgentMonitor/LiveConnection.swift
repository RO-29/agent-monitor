import Foundation

/// WebSocket client for `/ws` with automatic reconnect and a heartbeat ping.
/// Delivers decoded `WSEvent`s and connection-state changes on the main actor.
final class LiveConnection: NSObject, URLSessionWebSocketDelegate {
    private let onEvent: (WSEvent) -> Void
    private let onState: (ConnectionState) -> Void

    // Callbacks come on URLSession's queue and timers; the lock guards the fields below.
    private let lock = NSLock()
    private var task: URLSessionWebSocketTask?
    private var session: URLSession!
    private var stopped = false
    private var attempt = 0
    private var retryScheduled = false

    /// True when t is the socket in use; callbacks of an older socket do nothing.
    private func isCurrent(_ t: URLSessionWebSocketTask) -> Bool {
        lock.lock(); defer { lock.unlock() }
        return !stopped && task === t
    }

    init(onEvent: @escaping (WSEvent) -> Void, onState: @escaping (ConnectionState) -> Void) {
        self.onEvent = onEvent
        self.onState = onState
        super.init()
        let cfg = URLSessionConfiguration.default
        cfg.timeoutIntervalForRequest = 0 // long-lived socket
        session = URLSession(configuration: cfg, delegate: self, delegateQueue: nil)
    }

    func start() {
        stopped = false
        openSocket()
    }

    func stop() {
        lock.lock()
        stopped = true
        let t = task
        task = nil
        lock.unlock()
        t?.cancel(with: .goingAway, reason: nil)
        session.invalidateAndCancel() // the session holds this object as its delegate
    }

    private func openSocket() {
        guard let url = Config.wsURL else {
            emitState(.disconnected("bad URL"))
            return
        }
        var req = URLRequest(url: url)
        if let h = Config.authHeader {
            req.setValue(h, forHTTPHeaderField: "Authorization")
        }
        // Check before creating the task: stop() invalidates the session, and a task on it would fail.
        lock.lock()
        if stopped { lock.unlock(); return }
        let t = session.webSocketTask(with: req)
        // The first snapshot frame is several MB on a busy Mac; the 1 MB default drops the socket.
        t.maximumMessageSize = 64 * 1024 * 1024
        let old = task
        task = t
        retryScheduled = false
        lock.unlock()
        old?.cancel(with: .goingAway, reason: nil)
        t.resume()
        receive(t)
        schedulePing(t)
    }

    private func receive(_ t: URLSessionWebSocketTask) {
        t.receive { [weak self] result in
            guard let self, self.isCurrent(t) else { return }
            switch result {
            case .success(let message):
                switch message {
                case .data(let d):
                    if let ev = WSEvent(data: d) { self.emitEvent(ev) }
                case .string(let s):
                    if let d = s.data(using: .utf8), let ev = WSEvent(data: d) { self.emitEvent(ev) }
                @unknown default:
                    break
                }
                self.receive(t)
            case .failure(let err):
                self.handleDrop(t, err.localizedDescription)
            }
        }
    }

    private func schedulePing(_ t: URLSessionWebSocketTask) {
        DispatchQueue.global().asyncAfter(deadline: .now() + 20) { [weak self] in
            guard let self, self.isCurrent(t) else { return }
            t.sendPing { [weak self] err in
                if let err { self?.handleDrop(t, err.localizedDescription); return }
                self?.schedulePing(t)
            }
        }
    }

    /// One drop of the current socket schedules one reconnect; the receive, ping and close callbacks all report it.
    private func handleDrop(_ t: URLSessionWebSocketTask, _ reason: String) {
        lock.lock()
        guard !stopped, task === t, !retryScheduled else { lock.unlock(); return }
        task = nil
        retryScheduled = true
        attempt += 1
        let delay = min(Double(attempt) * 1.5, 15)
        lock.unlock()
        emitState(.disconnected(reason))
        DispatchQueue.global().asyncAfter(deadline: .now() + delay) { [weak self] in
            guard let self else { return }
            self.lock.lock()
            let go = !self.stopped
            self.lock.unlock()
            guard go else { return }
            self.emitState(.connecting)
            self.openSocket()
        }
    }

    // URLSessionWebSocketDelegate
    func urlSession(_ session: URLSession, webSocketTask: URLSessionWebSocketTask,
                    didOpenWithProtocol protocol: String?) {
        guard isCurrent(webSocketTask) else { return }
        lock.lock(); attempt = 0; lock.unlock()
        emitState(.connected)
    }

    func urlSession(_ session: URLSession, webSocketTask: URLSessionWebSocketTask,
                    didCloseWith closeCode: URLSessionWebSocketTask.CloseCode, reason: Data?) {
        handleDrop(webSocketTask, "closed")
    }

    private func emitEvent(_ ev: WSEvent) { DispatchQueue.main.async { self.onEvent(ev) } }
    private func emitState(_ st: ConnectionState) { DispatchQueue.main.async { self.onState(st) } }
}
