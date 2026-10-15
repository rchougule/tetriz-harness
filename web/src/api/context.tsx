import { createContext, useCallback, useContext, useMemo, useState, type ReactNode } from "react";
import { createClient, type Client, type Transport } from "@connectrpc/connect";
import { MutationCache, QueryCache, QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { AdminService } from "@harness-control-plane/gen/harnessctl/v1/admin_pb";
import { clearToken, readToken, writeToken } from "./token";
import { isUnauthenticated } from "./errors";

export type AdminClient = Client<typeof AdminService>;

export type SignOutReason = "user" | "rejected";

interface Session {
  token: string | null;
  signedOutReason: SignOutReason | null;
  signIn: (token: string) => void;
  signOut: (reason: SignOutReason) => void;
  mock: boolean;
}

const SessionContext = createContext<Session | null>(null);
const ClientContext = createContext<AdminClient | null>(null);

export function useSession(): Session {
  const s = useContext(SessionContext);
  if (!s) throw new Error("useSession outside AppProviders");
  return s;
}

export function useAdmin(): AdminClient {
  const c = useContext(ClientContext);
  if (!c) throw new Error("useAdmin outside AppProviders");
  return c;
}

interface Props {
  // Built once per app; reads the token lazily so a sign-in needs no new transport.
  makeTransport: (getToken: () => string | null) => Transport;
  mock?: boolean;
  children: ReactNode;
}

export function AppProviders({ makeTransport, mock = false, children }: Props) {
  const [token, setToken] = useState<string | null>(() => readToken());
  const [signedOutReason, setReason] = useState<SignOutReason | null>(null);

  const signOut = useCallback((reason: SignOutReason) => {
    clearToken();
    setToken(null);
    setReason(reason);
  }, []);

  const queryClient = useMemo(() => {
    const onError = (err: unknown) => {
      if (isUnauthenticated(err)) signOut("rejected");
    };
    return new QueryClient({
      queryCache: new QueryCache({ onError }),
      mutationCache: new MutationCache({ onError }),
      defaultOptions: {
        queries: {
          retry: (count, err) => !isUnauthenticated(err) && count < 2,
          refetchOnWindowFocus: false,
          staleTime: 5_000,
        },
      },
    });
  }, [signOut]);

  const client = useMemo(() => createClient(AdminService, makeTransport(readToken)), [makeTransport]);

  const signIn = useCallback(
    (t: string) => {
      writeToken(t);
      queryClient.clear();
      setReason(null);
      setToken(t);
    },
    [queryClient],
  );

  const session = useMemo<Session>(
    () => ({ token, signedOutReason, signIn, signOut, mock }),
    [token, signedOutReason, signIn, signOut, mock],
  );

  return (
    <SessionContext.Provider value={session}>
      <ClientContext.Provider value={client}>
        <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>
      </ClientContext.Provider>
    </SessionContext.Provider>
  );
}
