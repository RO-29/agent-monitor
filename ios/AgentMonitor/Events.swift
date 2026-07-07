import Foundation

/// Every message on `/ws` is one of four Go event types, multiplexed and
/// distinguished by its `kind` string:
///   sessions: snapshot | upsert | remove          (ServerEvent)
///   perms:    perm-add | perm-remove              (PermEvent)
///   panes:    pane-register | pane-forget | pane-name (PaneEvent)
///   talks:    talk-request | talk-resolved        (TalkEvent)
enum WSEvent {
    case snapshot([Session])
    case upsert(Session)
    case remove(String)

    case permAdd(PermissionRequest)
    case permRemove(String)

    case paneRegister(PaneRegistration)
    case paneForget(String)
    case paneName(PaneRegistration)

    case talkRequest(Talk)
    case talkResolved(Talk)

    case unknown(String)

    private struct KindPeek: Codable { let kind: String }

    private struct ServerEvent: Codable {
        let kind: String
        let sessions: [Session]?
        let session: Session?
        let id: String?
    }
    private struct PermEvent: Codable {
        let kind: String
        let request: PermissionRequest?
        let id: String?
    }
    private struct PaneEvent: Codable {
        let kind: String
        let registration: PaneRegistration?
        let agentId: String?
    }
    private struct TalkEvent: Codable {
        let kind: String
        let talk: Talk?
    }

    init?(data: Data) {
        let dec = JSONDecoder()
        guard let peek = try? dec.decode(KindPeek.self, from: data) else { return nil }
        switch peek.kind {
        case "snapshot":
            let e = try? dec.decode(ServerEvent.self, from: data)
            self = .snapshot(e?.sessions ?? [])
        case "upsert":
            guard let s = (try? dec.decode(ServerEvent.self, from: data))?.session else { return nil }
            self = .upsert(s)
        case "remove":
            self = .remove((try? dec.decode(ServerEvent.self, from: data))?.id ?? "")
        case "perm-add":
            guard let r = (try? dec.decode(PermEvent.self, from: data))?.request else { return nil }
            self = .permAdd(r)
        case "perm-remove":
            self = .permRemove((try? dec.decode(PermEvent.self, from: data))?.id ?? "")
        case "pane-register":
            guard let r = (try? dec.decode(PaneEvent.self, from: data))?.registration else { return nil }
            self = .paneRegister(r)
        case "pane-name":
            guard let r = (try? dec.decode(PaneEvent.self, from: data))?.registration else { return nil }
            self = .paneName(r)
        case "pane-forget":
            self = .paneForget((try? dec.decode(PaneEvent.self, from: data))?.agentId ?? "")
        case "talk-request":
            guard let t = (try? dec.decode(TalkEvent.self, from: data))?.talk else { return nil }
            self = .talkRequest(t)
        case "talk-resolved":
            guard let t = (try? dec.decode(TalkEvent.self, from: data))?.talk else { return nil }
            self = .talkResolved(t)
        default:
            self = .unknown(peek.kind)
        }
    }
}
