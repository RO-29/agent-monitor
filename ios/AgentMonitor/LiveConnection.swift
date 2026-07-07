import Foundation

/// WebSocket client for `/ws` with automatic reconnect and a heartbeat ping.
/// Delivers decoded `WSEvent`s and connection-state changes on the main actor.
final class LiveConnection: NSObject, URLSessionWebSocketDelegate {
    private let onEvent: (WSEvent) -> Void
    private let onState: (ConnectionState) -> Void

    private var task: URLSessionWebSocketTask?
    private var session: URLSession!
    private var stopped = false
    private var attempt = 0

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
        stopped = true
        task?.cancel(with: .goingAway, reason: nil)
        task = nil
    }

    private func openSocket() {
        guard !stopped, let url = Config.wsURL else {
            emitState(.disconnected("bad URL"))
            return
        }
        let t = session.webSocketTask(with: url)
        task = t
        t.resume()
        receive()
        schedulePing()
    }

    private func receive() {
        task?.receive { [weak self] result in
            guard let self else { return }
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
                self.receive()
            case .failure(let err):
                self.handleDrop(err.localizedDescription)
            }
        }
    }

    private func schedulePing() {
        DispatchQueue.global().asyncAfter(deadline: .now() + 20) { [weak self] in
            guard let self, !self.stopped, let t = self.task else { return }
            t.sendPing { [weak self] err in
                if let err { self?.handleDrop(err.localizedDescription); return }
                self?.schedulePing()
            }
        }
    }

    private func handleDrop(_ reason: String) {
        guard !stopped else { return }
        task = nil
        emitState(.disconnected(reason))
        attempt += 1
        let delay = min(Double(attempt) * 1.5, 15)
        DispatchQueue.global().asyncAfter(deadline: .now() + delay) { [weak self] in
            guard let self, !self.stopped else { return }
            self.emitState(.connecting)
            self.openSocket()
        }
    }

    // URLSessionWebSocketDelegate
    func urlSession(_ session: URLSession, webSocketTask: URLSessionWebSocketTask,
                    didOpenWithProtocol protocol: String?) {
        attempt = 0
        emitState(.connected)
    }

    func urlSession(_ session: URLSession, webSocketTask: URLSessionWebSocketTask,
                    didCloseWith closeCode: URLSessionWebSocketTask.CloseCode, reason: Data?) {
        handleDrop("closed")
    }

    private func emitEvent(_ ev: WSEvent) { DispatchQueue.main.async { self.onEvent(ev) } }
    private func emitState(_ st: ConnectionState) { DispatchQueue.main.async { self.onState(st) } }
}
