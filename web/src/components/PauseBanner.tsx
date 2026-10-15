import { Link } from "react-router";
import { useSettings } from "../api/queries";

export function PauseBanner() {
  const { data } = useSettings();
  if (!data?.enforcementPaused) return null;
  return (
    <div className="pause-banner" role="status">
      <strong>Enforcement is paused.</strong>
      <span>Agents make no writes until it is resumed. Nothing has been removed from machines.</span>
      <Link to="/settings">Go to settings</Link>
    </div>
  );
}
