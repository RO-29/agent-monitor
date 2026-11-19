import { NavLink } from "react-router-dom";

const TABS = [
  { to: "/settings/devices", label: "Devices" },
  { to: "/settings/machines", label: "Machines" },
  { to: "/settings/notifications", label: "Notifications" },
  { to: "/settings/limits", label: "Limits" },
  { to: "/settings/voice", label: "Voice" },
  { to: "/settings/storage", label: "Storage" },
  { to: "/settings/agents", label: "Agents" },
  { to: "/settings/templates", label: "Templates" },
  { to: "/settings/schedules", label: "Schedules" },
];

export function SettingsTabs() {
  return (
    <nav className="sx-tabs" aria-label="Settings">
      {TABS.map((t) => (
        <NavLink key={t.to} to={t.to} className={({ isActive }) => (isActive ? "on" : "")}>
          {t.label}
        </NavLink>
      ))}
    </nav>
  );
}

/** The on/off switch used across Settings pages. */
export function Switch({ on, label, onChange, disabled, hint }: { on: boolean; label: string; onChange: (v: boolean) => void; disabled?: boolean; hint?: string }) {
  return (
    <label className={`sx-switch ${disabled ? "dis" : ""}`}>
      <input type="checkbox" role="switch" checked={on} aria-checked={on} disabled={disabled} onChange={(e) => onChange(e.target.checked)} />
      <span className="sx-track" aria-hidden />
      <span className="sx-sw-t">
        {label}
        {hint && <small>{hint}</small>}
      </span>
    </label>
  );
}
