const KEY = "hcp.adminToken";

// sessionStorage can throw (private mode, blocked storage); the app then keeps the token in memory only.
let memory: string | null = null;

export function readToken(): string | null {
  try {
    return window.sessionStorage.getItem(KEY) ?? memory;
  } catch {
    return memory;
  }
}

export function writeToken(token: string): void {
  memory = token;
  try {
    window.sessionStorage.setItem(KEY, token);
  } catch {
    /* memory copy is enough for this tab */
  }
}

export function clearToken(): void {
  memory = null;
  try {
    window.sessionStorage.removeItem(KEY);
  } catch {
    /* nothing persisted */
  }
}
