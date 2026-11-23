import Foundation

/// One machine of the primary server's fleet: this Mac (`self`) or a peer it can see.
struct FleetMachine: Decodable, Hashable, Identifiable {
    var name: String
    var level: String?
    var status: String?
    private var selfFlag: Bool?
    var id: String { name }
    var isSelf: Bool { selfFlag == true }
    var online: Bool { isSelf || status == "online" }
    enum CodingKeys: String, CodingKey { case name, level, status, selfFlag = "self" }
}

struct FleetThreads: Decodable {
    var machines: [FleetMachine]?
    var threads: [V3Thread]?
}

// Threads of other machines come through GET /api/fleet/threads on the primary server.
extension AppStore {
    var manyMachines: Bool { fleetMachines.count > 1 }
    var selfMachine: String { fleetMachines.first { $0.isSelf }?.name ?? "" }

    func loadFleet() async {
        do {
            let r = try await APIClient.shared.get("/api/fleet/threads", as: FleetThreads.self)
            fleetMachines = r.machines ?? []
            peerThreads = (r.threads ?? []).compactMap { t in
                guard let m = t.machine else { return nil }
                var p = t
                p.peerID = t.id
                p.id = "@\(m):\(t.id)"
                return p
            }
        } catch {
            // An older daemon, or a device grant below admin: this Mac's threads still show.
        }
    }
}
