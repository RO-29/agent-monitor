// agent-monitor Web Push: shows a notification per push and opens its link on click.
self.addEventListener("install", () => self.skipWaiting());
self.addEventListener("activate", (e) => e.waitUntil(self.clients.claim()));

self.addEventListener("push", (e) => {
  let n = {};
  try {
    n = e.data ? e.data.json() : {};
  } catch {
    n = { title: "agent-monitor", body: e.data ? e.data.text() : "" };
  }
  const url = n.url || (n.threadId ? `/w/${encodeURIComponent(n.threadId)}` : "/notifications");
  e.waitUntil(
    // Every push shows a notification: Safari and iOS revoke a subscription whose pushes show nothing.
    Promise.resolve().then(() => {
      return self.registration.showNotification(n.title || "agent-monitor", {
        body: n.body || "",
        tag: n.id ? `am-${n.id}` : undefined,
        data: { url },
        requireInteraction: n.kind === "needs-you",
      });
    }),
  );
});

// Only a same-origin relative path is opened; anything else falls back to the notifications page.
function safeUrl(raw) {
  if (typeof raw !== "string" || !raw.startsWith("/") || raw.startsWith("//") || raw.startsWith("/\\")) return "/notifications";
  try {
    const u = new URL(raw, self.location.origin);
    return u.origin === self.location.origin ? u.pathname + u.search + u.hash : "/notifications";
  } catch {
    return "/notifications";
  }
}

self.addEventListener("notificationclick", (e) => {
  e.notification.close();
  const url = safeUrl(e.notification.data && e.notification.data.url);
  const full = new URL(url, self.location.origin).href;
  e.waitUntil(
    self.clients.matchAll({ type: "window", includeUncontrolled: true }).then(async (list) => {
      const same = list.find((c) => c.url === full && "focus" in c);
      if (same) return same.focus();
      const c = list.find((x) => "navigate" in x && "focus" in x);
      if (c) {
        try {
          const n = await c.navigate(url);
          if (n) return n.focus();
        } catch {
          /* not controlled by this worker */
        }
      }
      return self.clients.openWindow(url);
    }),
  );
});
