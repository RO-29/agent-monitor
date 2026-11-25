import Foundation

// Polls each other machine while the app is open; a closed app gets that machine's own Bark alerts.
extension AppStore {
    var remoteNeedCount: Int { remoteNeeds.values.reduce(0) { $0 + $1.count } }

    func reloadMachines() {
        machines = Machines.all
        let ids = Set(machines.map(\.id))
        remoteNeeds = remoteNeeds.filter { ids.contains($0.key) }
        remoteDown = remoteDown.filter { ids.contains($0.key) }
        remoteDownSince = remoteDownSince.filter { ids.contains($0.key) }
        setRemoteForeground(true)
    }

    func setRemoteForeground(_ active: Bool) {
        remotePoll?.cancel()
        remotePoll = nil
        let list = machines
        guard !list.isEmpty else { return }
        guard active else {
            for m in list { Task { _ = try? await m.client.post("/api/push/presence", body: ["active": false]) } }
            return
        }
        // One awaited loop per machine: a slow Mac never piles up requests, and never delays the others.
        remotePoll = Task { [weak self] in
            guard let self else { return }
            await withTaskGroup(of: Void.self) { g in
                for m in list {
                    g.addTask { @MainActor in
                        var tick = 0
                        while !Task.isCancelled {
                            await self.loadRemote(m, presence: tick % 6 == 0)
                            tick += 1
                            try? await Task.sleep(for: .seconds(5))
                        }
                    }
                }
            }
        }
    }

    func loadRemote(_ m: Machine, presence: Bool = false) async {
        if presence { _ = try? await m.client.post("/api/push/presence", body: ["active": true]) }
        do {
            let items = try await m.client.needsYou().items ?? []
            guard machines.contains(where: { $0.id == m.id }) else { return }
            notifyRemote(m, items, first: remoteNeeds[m.id] == nil)
            remoteNeeds[m.id] = items
            remoteDown[m.id] = nil
            remoteDownSince[m.id] = nil
        } catch {
            remoteDown[m.id] = error.localizedDescription
            if remoteDownSince[m.id] == nil { remoteDownSince[m.id] = Date() }
        }
    }

    /// Alerts once per new item; the first load after launch only records what is already there.
    private func notifyRemote(_ m: Machine, _ items: [NeedsYouItem], first: Bool) {
        // Keys repeat ("tp:<session>" for each prompt): forget the ones that closed, so the next one alerts.
        let prefix = "m-\(m.id)-"
        let now = Set(items.map { prefix + $0.key })
        remoteSeen = remoteSeen.filter { !$0.hasPrefix(prefix) || now.contains($0) }
        for it in items {
            let id = prefix + it.key
            guard remoteSeen.insert(id).inserted, !first else { continue }
            let who = agentName(it.agent ?? "")
            Notifier.shared.approvalAsk(id: id, title: "[\(m.name)] \(it.kind == "question" ? "\(who) is asking" : "\(who) needs you")",
                                        body: it.displayTitle)
        }
    }

    /// Allow or Deny on the machine that owns the item; a question or an error is answered on that Mac.
    func decideRemote(_ m: Machine, _ it: NeedsYouItem, allow: Bool) async throws {
        let c = m.client
        switch it.kind {
        case "approval":
            _ = try await c.decide(it.approval?.id ?? it.key, allow ? .allow() : .deny())
        case "mcp":
            guard let id = it.mcp?.id else { throw APIError(message: "No request id") }
            try await c.respondPermission(id, allow: allow)
        case "prompt":
            guard let sid = it.sessionId else { throw APIError(message: "No session id") }
            try await c.answerPrompt(sid, yes: allow, key: it.promptKey)
        default:
            throw APIError(message: "Answer this one on \(m.name).")
        }
        remoteNeeds[m.id]?.removeAll { $0.key == it.key }
        await loadRemote(m)
    }
}

extension AppStore {
    /// Machines whose requests Home shows: all of them, or the one the machine filter picks.
    var remoteForHome: [Machine] {
        let only = manyMachines && machines.contains { $0.name == machineFilter } ? machineFilter : ""
        return machines.filter { only.isEmpty || $0.name == only }
    }

    /// "offline since 14:02" for a machine that failed, else nil.
    func offlineSince(_ id: String) -> String? {
        guard let d = remoteDownSince[id] else { return nil }
        return "offline since " + d.formatted(date: .omitted, time: .shortened)
    }
}
