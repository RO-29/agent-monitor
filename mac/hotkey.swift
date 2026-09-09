// Global (system-wide) hotkey support for AgentMonitor.app.
//
// Carbon's RegisterEventHotKey is used deliberately: it needs no Accessibility
// or Input Monitoring entitlement, so the app never triggers a privacy prompt.
// The AppKit alternative, NSEvent.addGlobalMonitorForEvents, does.

import Carbon.HIToolbox
import Foundation

/// A parsed key combination: modifier mask plus a virtual key code.
struct HotKeySpec {
    let keyCode: UInt32
    let modifiers: UInt32
    let display: String

    /// Parse a combination such as `"opt+cmd+a"` or `"ctrl+shift+j"`.
    ///
    /// Tokens are separated by `+`, order does not matter, and the final token
    /// is the key itself. Returns `nil` on anything unrecognised so the caller
    /// can report the problem rather than guess.
    static func parse(_ raw: String) -> HotKeySpec? {
        let tokens = raw.lowercased()
            .split(separator: "+")
            .map { $0.trimmingCharacters(in: .whitespaces) }
            .filter { !$0.isEmpty }
        guard tokens.count >= 2 else { return nil }

        var mods: UInt32 = 0
        var key: String?

        for token in tokens {
            switch token {
            case "cmd", "command", "meta": mods |= UInt32(cmdKey)
            case "opt", "alt", "option":   mods |= UInt32(optionKey)
            case "ctrl", "control":        mods |= UInt32(controlKey)
            case "shift":                  mods |= UInt32(shiftKey)
            default:
                // Only one non-modifier token is allowed.
                if key != nil { return nil }
                key = token
            }
        }

        guard mods != 0, let key, let code = Self.keyCodes[key] else { return nil }
        return HotKeySpec(keyCode: code, modifiers: mods, display: Self.render(mods: mods, key: key))
    }

    private static func render(mods: UInt32, key: String) -> String {
        var out = ""
        if mods & UInt32(controlKey) != 0 { out += "⌃" }
        if mods & UInt32(optionKey)  != 0 { out += "⌥" }
        if mods & UInt32(shiftKey)   != 0 { out += "⇧" }
        if mods & UInt32(cmdKey)     != 0 { out += "⌘" }
        return out + (Self.keyLabels[key] ?? key.uppercased())
    }

    private static let keyLabels: [String: String] = [
        "space": "Space", "return": "↩", "enter": "↩", "tab": "⇥", "escape": "⎋", "esc": "⎋",
    ]

    private static let keyCodes: [String: UInt32] = [
        "a": UInt32(kVK_ANSI_A), "b": UInt32(kVK_ANSI_B), "c": UInt32(kVK_ANSI_C),
        "d": UInt32(kVK_ANSI_D), "e": UInt32(kVK_ANSI_E), "f": UInt32(kVK_ANSI_F),
        "g": UInt32(kVK_ANSI_G), "h": UInt32(kVK_ANSI_H), "i": UInt32(kVK_ANSI_I),
        "j": UInt32(kVK_ANSI_J), "k": UInt32(kVK_ANSI_K), "l": UInt32(kVK_ANSI_L),
        "m": UInt32(kVK_ANSI_M), "n": UInt32(kVK_ANSI_N), "o": UInt32(kVK_ANSI_O),
        "p": UInt32(kVK_ANSI_P), "q": UInt32(kVK_ANSI_Q), "r": UInt32(kVK_ANSI_R),
        "s": UInt32(kVK_ANSI_S), "t": UInt32(kVK_ANSI_T), "u": UInt32(kVK_ANSI_U),
        "v": UInt32(kVK_ANSI_V), "w": UInt32(kVK_ANSI_W), "x": UInt32(kVK_ANSI_X),
        "y": UInt32(kVK_ANSI_Y), "z": UInt32(kVK_ANSI_Z),
        "0": UInt32(kVK_ANSI_0), "1": UInt32(kVK_ANSI_1), "2": UInt32(kVK_ANSI_2),
        "3": UInt32(kVK_ANSI_3), "4": UInt32(kVK_ANSI_4), "5": UInt32(kVK_ANSI_5),
        "6": UInt32(kVK_ANSI_6), "7": UInt32(kVK_ANSI_7), "8": UInt32(kVK_ANSI_8),
        "9": UInt32(kVK_ANSI_9),
        "space": UInt32(kVK_Space), "return": UInt32(kVK_Return), "enter": UInt32(kVK_Return),
        "tab": UInt32(kVK_Tab), "escape": UInt32(kVK_Escape), "esc": UInt32(kVK_Escape),
    ]
}

/// One registered system-wide hotkey. Holding the instance keeps it live;
/// releasing it unregisters.
final class GlobalHotKey {
    private var hotKeyRef: EventHotKeyRef?
    private var eventHandler: EventHandlerRef?
    private let id: UInt32

    // Carbon hands back only a numeric id, so route it through a table to the
    // Swift closure that should run.
    private static var actions: [UInt32: () -> Void] = [:]
    private static var nextID: UInt32 = 1

    static func fire(_ id: UInt32) { actions[id]?() }

    /// Registers `spec` system-wide. Returns `nil` if Carbon refuses it.
    ///
    /// Note that this does *not* detect a conflict with another application:
    /// `RegisterEventHotKey` happily registers a combination a different
    /// process already holds, and macOS then decides who receives the key. A
    /// silent shortcut means something else is taking it, not that this failed.
    init?(spec: HotKeySpec, action: @escaping () -> Void) {
        id = Self.nextID
        Self.nextID += 1

        var eventType = EventTypeSpec(eventClass: OSType(kEventClassKeyboard),
                                      eventKind: UInt32(kEventHotKeyPressed))
        let installed = InstallEventHandler(GetApplicationEventTarget(), { _, event, _ -> OSStatus in
            var received = EventHotKeyID()
            let status = GetEventParameter(event,
                                           EventParamName(kEventParamDirectObject),
                                           EventParamType(typeEventHotKeyID),
                                           nil,
                                           MemoryLayout<EventHotKeyID>.size,
                                           nil,
                                           &received)
            guard status == noErr else { return status }
            GlobalHotKey.fire(received.id)
            return noErr
        }, 1, &eventType, nil, &eventHandler)
        guard installed == noErr else { return nil }

        // 'AGMN' — an arbitrary four-char signature identifying our hotkeys.
        let hotKeyID = EventHotKeyID(signature: OSType(0x4147_4D4E), id: id)
        let registered = RegisterEventHotKey(spec.keyCode, spec.modifiers, hotKeyID,
                                             GetApplicationEventTarget(), 0, &hotKeyRef)
        guard registered == noErr, hotKeyRef != nil else {
            RemoveEventHandler(eventHandler)
            return nil
        }

        Self.actions[id] = action
    }

    deinit {
        if let hotKeyRef { UnregisterEventHotKey(hotKeyRef) }
        if let eventHandler { RemoveEventHandler(eventHandler) }
        Self.actions[id] = nil
    }
}
