// Passkey approvals for a paired browser: the daemon answers 428 {bio:true}, and this signs the request and sends it again.
const KEY = "am.passkey";

const b64 = (b: ArrayBuffer | Uint8Array) => {
  let s = "";
  for (const x of b instanceof Uint8Array ? b : new Uint8Array(b)) s += String.fromCharCode(x);
  return btoa(s);
};
const b64u = (b: ArrayBuffer | Uint8Array) => b64(b).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
const fromB64u = (s: string) => Uint8Array.from(atob(s.replace(/-/g, "+").replace(/_/g, "/")), (c) => c.charCodeAt(0));
const sha256 = async (b: BufferSource) => new Uint8Array(await crypto.subtle.digest("SHA-256", b));
const hex = (u: Uint8Array) => Array.from(u, (x) => x.toString(16).padStart(2, "0")).join("");

export const passkeySupported = () => typeof window !== "undefined" && window.isSecureContext && !!window.PublicKeyCredential;
export const passkeyId = () => {
  try {
    return localStorage.getItem(KEY) || "";
  } catch {
    return "";
  }
};

// The challenge is sha256 of the same message the iPhone signs, so the daemon checks both the same way.
async function header(method: string, url: string, body: string): Promise<string> {
  const id = passkeyId();
  const ts = Date.now();
  const path = new URL(url, location.href).pathname;
  const msg = `agent-monitor-bio\n${method}\n${path}\n${ts}\n${hex(await sha256(new TextEncoder().encode(body)))}`;
  const cred = (await navigator.credentials.get({
    publicKey: {
      challenge: await sha256(new TextEncoder().encode(msg)),
      rpId: location.hostname,
      allowCredentials: [{ type: "public-key", id: fromB64u(id) }],
      userVerification: "required",
      timeout: 60_000,
    },
  })) as PublicKeyCredential | null;
  if (!cred) throw new Error("passkey cancelled");
  const r = cred.response as AuthenticatorAssertionResponse;
  const j = { ts, id, authData: b64u(r.authenticatorData), clientData: b64u(r.clientDataJSON), sig: b64u(r.signature) };
  return b64u(new TextEncoder().encode(JSON.stringify(j)));
}

let installed = false;
export function installPasskeyFetch() {
  if (installed || typeof window === "undefined") return;
  installed = true;
  const orig = window.fetch.bind(window);
  window.fetch = async (input: RequestInfo | URL, init?: RequestInit) => {
    const res = await orig(input, init);
    if (res.status !== 428 || !passkeyId() || input instanceof Request) return res;
    const body = init?.body ?? "";
    if (typeof body !== "string") return res;
    const j = await res.clone().json().catch(() => null);
    if (!j?.bio) return res;
    const method = (init?.method || "GET").toUpperCase();
    const h = new Headers(init?.headers);
    h.set("X-AM-Passkey", await header(method, String(input), body));
    return orig(input, { ...init, headers: h });
  };
}

/** Makes a passkey on this browser and gives its public key to the Mac. */
export async function enrollPasskey(name: string) {
  const cred = (await navigator.credentials.create({
    publicKey: {
      challenge: crypto.getRandomValues(new Uint8Array(32)),
      rp: { id: location.hostname, name: "agent-monitor" },
      user: { id: crypto.getRandomValues(new Uint8Array(16)), name: name || "browser", displayName: name || "browser" },
      pubKeyCredParams: [{ type: "public-key", alg: -7 }],
      authenticatorSelection: { userVerification: "required", residentKey: "discouraged" },
      timeout: 60_000,
    },
  })) as PublicKeyCredential | null;
  if (!cred) throw new Error("passkey cancelled");
  const r = cred.response as AuthenticatorAttestationResponse;
  if (r.getPublicKeyAlgorithm?.() !== -7) throw new Error("this browser made a key type the Mac does not check (it needs ES256)");
  const spki = r.getPublicKey?.();
  if (!spki) throw new Error("this browser cannot give the public key; use a current Safari, Chrome or Firefox");
  const id = b64u(cred.rawId);
  const res = await fetch("/api/devices/me/passkey", {
    method: "POST",
    credentials: "same-origin",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ passkey: { id, spki: b64(spki), rp: location.hostname } }),
  });
  if (!res.ok) throw new Error((await res.json().catch(() => null))?.error || `HTTP ${res.status}`);
  localStorage.setItem(KEY, id);
}

export async function removePasskey() {
  const res = await fetch("/api/devices/me/passkey", { method: "DELETE", credentials: "same-origin", body: "" });
  if (!res.ok) throw new Error((await res.json().catch(() => null))?.error || `HTTP ${res.status}`);
  localStorage.removeItem(KEY);
}
