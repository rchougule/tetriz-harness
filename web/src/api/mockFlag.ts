const KEY = "hcp.mock";

// Compile-time gate: production builds fold this to false so the mock module is never bundled.
export const MOCK_AVAILABLE: boolean = import.meta.env.DEV || import.meta.env.VITE_ENABLE_MOCK === "1";

export function mockRequested(): boolean {
  if (!MOCK_AVAILABLE) return false;
  const param = new URLSearchParams(window.location.search).get("mock");
  try {
    if (param === "1") window.sessionStorage.setItem(KEY, "1");
    if (param === "0") window.sessionStorage.removeItem(KEY);
    return window.sessionStorage.getItem(KEY) === "1";
  } catch {
    return param === "1";
  }
}
