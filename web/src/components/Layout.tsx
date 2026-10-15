import { useRef } from "react";
import { NavLink, Outlet } from "react-router";
import { useSession } from "../api/context";
import { PauseBanner } from "./PauseBanner";

const NAV = [
  { to: "/artifacts", label: "Artifacts" },
  { to: "/settings", label: "Settings" },
];

export function Layout() {
  const { signOut, mock } = useSession();
  const mainRef = useRef<HTMLElement>(null);
  return (
    <div className="shell">
      <a
        className="skip-link"
        href="#main"
        onClick={(e) => {
          // Moves focus itself rather than leaving a hash navigation for the router.
          e.preventDefault();
          mainRef.current?.focus();
        }}
      >
        Skip to main content
      </a>
      <aside className="sidebar">
        <div className="sidebar-inner">
        <div className="brand">
          <span className="brand-mark" aria-hidden="true">H</span>
          <span>Harness Control Plane</span>
        </div>
        <nav className="nav" aria-label="Main">
          {NAV.map((n) => (
            <NavLink key={n.to} to={n.to}>
              {n.label}
            </NavLink>
          ))}
        </nav>
        <div className="sidebar-foot">
          {mock && <span className="tag" title="Requests are served by an in-browser fake">Mock data</span>}
          <button type="button" className="btn btn-sm" onClick={() => signOut("user")}>
            Sign out
          </button>
        </div>
        </div>
      </aside>
      <div className="main">
        <PauseBanner />
        <main className="content" id="main" tabIndex={-1} ref={mainRef}>
          <Outlet />
        </main>
      </div>
    </div>
  );
}
