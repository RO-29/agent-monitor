// Touch ID before an approval leaves this app; a guard against a person at an unlocked Mac, not against local programs.
import Cocoa
import LocalAuthentication
import WebKit

let kTouchIDKey = "touchIDApprovals"

/// Wraps fetch in the page: an approval that is not a deny waits for amTouchID to answer true.
let kTouchIDScript = """
(() => {
  const h = window.webkit && window.webkit.messageHandlers && window.webkit.messageHandlers.amTouchID;
  if (!h || window.__amTouchID) return;
  window.__amTouchID = true;
  const gated = /(approvals\\/[^/]+\\/decide|permission\\/[^/]+\\/respond|hosts\\/[^/]+\\/answer|session\\/[^/]+\\/answer)$/;
  const orig = window.fetch.bind(window);
  window.fetch = async (input, init) => {
    const method = ((init && init.method) || (input && input.method) || "GET").toUpperCase();
    const url = new URL(typeof input === "string" ? input : (input && input.url) || String(input), location.href);
    if (method !== "POST" || !gated.test(url.pathname)) return orig(input, init);
    let body = {};
    try { body = JSON.parse((init && typeof init.body === "string") ? init.body : "{}"); } catch (e) {}
    if (body.behavior === "deny" || body.decision === "deny") return orig(input, init);
    const ok = await h.postMessage("approve an agent request");
    if (ok !== true) throw new Error("Touch ID was not confirmed");
    return orig(input, init);
  };
})();
"""

final class TouchIDGate: NSObject, WKScriptMessageHandlerWithReply {
    static var enabled: Bool {
        get { UserDefaults.standard.object(forKey: kTouchIDKey) as? Bool ?? true }
        set { UserDefaults.standard.set(newValue, forKey: kTouchIDKey) }
    }

    func userContentController(_ c: WKUserContentController, didReceive m: WKScriptMessage, replyHandler: @escaping (Any?, String?) -> Void) {
        guard Self.enabled else { return replyHandler(true, nil) }
        let ctx = LAContext()
        var err: NSError?
        // Without Touch ID this falls back to the Mac password.
        guard ctx.canEvaluatePolicy(.deviceOwnerAuthentication, error: &err) else { return replyHandler(true, nil) }
        ctx.evaluatePolicy(.deviceOwnerAuthentication, localizedReason: (m.body as? String) ?? "approve an agent request") { ok, _ in
            DispatchQueue.main.async { replyHandler(ok, nil) }
        }
    }
}
