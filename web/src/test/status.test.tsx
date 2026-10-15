import { render, screen } from "@testing-library/react";
import { EnforcementStatus } from "@harness-control-plane/gen/harnessctl/v1/types_pb";
import { StatusBadge } from "../components/StatusBadge";
import { statusMeta, STATUS_ORDER } from "../lib/status";

const EXPECTED: Record<EnforcementStatus, string> = {
  [EnforcementStatus.UNSPECIFIED]: "Unknown",
  [EnforcementStatus.INSTALLED]: "Installed",
  [EnforcementStatus.PENDING]: "Pending",
  [EnforcementStatus.FAILED]: "Failed",
  [EnforcementStatus.EXCLUDED]: "Excluded",
  [EnforcementStatus.REMOVAL_PENDING]: "Removal pending",
  [EnforcementStatus.REMOVED]: "Removed",
  [EnforcementStatus.EXPIRED]: "Expired",
};

const ALL = Object.values(EnforcementStatus).filter((v): v is EnforcementStatus => typeof v === "number");

describe("status badge", () => {
  it("covers every enum value the generated code defines", () => {
    expect(ALL.sort()).toEqual(Object.keys(EXPECTED).map(Number).sort());
  });

  it.each(ALL)("renders a text label and glyph for status %i", (s) => {
    render(<StatusBadge status={s} />);
    const badge = screen.getByText(EXPECTED[s]);
    expect(badge).toBeVisible();
    expect(badge.querySelector("[aria-hidden=true]")?.textContent).toBe(statusMeta(s).glyph);
  });

  it("gives each emitted status a distinct label", () => {
    const labels = STATUS_ORDER.map((s) => statusMeta(s).label);
    expect(new Set(labels).size).toBe(labels.length);
    expect(STATUS_ORDER).not.toContain(EnforcementStatus.UNSPECIFIED);
  });

  it("falls back for a value newer than this build", () => {
    expect(statusMeta(99 as EnforcementStatus).label).toBe("Unknown");
  });
});
