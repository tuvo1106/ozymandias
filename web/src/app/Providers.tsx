import { QueryClientProvider } from "@tanstack/react-query";
import { useState, type ReactNode } from "react";
import { createQueryClient } from "../lib/queryClient";

/**
 * App-wide context providers. Mounted inside the route tree (not in
 * main.tsx) so tests that render `routes` in a memory router get the same
 * providers as the real app. The QueryClient is created once per mount;
 * a module-level client would leak cached responses between tests.
 */
export function Providers({ children }: { children: ReactNode }) {
  const [client] = useState(createQueryClient);
  return <QueryClientProvider client={client}>{children}</QueryClientProvider>;
}
