import { useQuery } from "@tanstack/react-query";
import { api, unwrap } from "./api";

// The caller and its permissions (/me). The UI hides pages the caller cannot read and
// disables actions it cannot take; the API stays the real gate.

export function useMe() {
  return useQuery({ queryKey: ["me"], queryFn: () => unwrap(api().GET("/me")), staleTime: 5 * 60_000 });
}

export function usePerms() {
  const me = useMe();
  const perms = new Set(me.data?.permissions ?? []);
  const can = (p: string) => perms.has(p);
  return { me: me.data, loading: me.isLoading, can, canAny: (...ps: string[]) => ps.some(can) };
}

/** requires is the tooltip text of an action the caller cannot take. */
export const requires = (p: string) => `Requires the ${p} permission`;
