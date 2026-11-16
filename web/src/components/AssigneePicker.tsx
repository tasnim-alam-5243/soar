import { useState } from "react";
import { Command } from "cmdk";
import { useQuery } from "@tanstack/react-query";
import { Check, UserMinus, UserPlus } from "lucide-react";
import { api, unwrap } from "@/lib/api";
import { usePerms } from "@/lib/perms";
import { Popover, PopoverContent, PopoverTrigger } from "./ui/menu";
import { Avatar } from "./common";

/** AssigneePicker offers "assign to me", known names (GET /assignees) and free text. */
export function AssigneePicker({ value, onChange, children, disabled }: {
  value?: string | null;
  onChange: (name: string) => void;
  children: React.ReactNode; // the trigger
  disabled?: boolean;
}) {
  const [open, setOpen] = useState(false);
  const [q, setQ] = useState("");
  const { me, can } = usePerms();
  const names = useQuery({
    queryKey: ["assignees"],
    enabled: open && can("alerts:write"),
    queryFn: () => unwrap(api().GET("/assignees")),
    staleTime: 60_000,
  });
  const pick = (n: string) => {
    setOpen(false);
    setQ("");
    onChange(n);
  };
  const typed = q.trim();
  const list = (names.data?.items ?? []).filter((n) => n !== me?.name);
  return (
    <Popover open={open} onOpenChange={(o) => !disabled && setOpen(o)}>
      <PopoverTrigger asChild disabled={disabled}>{children}</PopoverTrigger>
      <PopoverContent className="w-64" onClick={(e) => e.stopPropagation()}>
        <Command loop>
          <Command.Input
            value={q}
            onValueChange={setQ}
            placeholder="Assign to…"
            className="h-9 w-full border-b border-line bg-transparent px-3 text-sm placeholder:text-fg-4 focus:outline-none"
          />
          <Command.List className="max-h-64 overflow-y-auto p-1">
            <Command.Empty className="px-2 py-3 text-xs text-fg-3">{names.isLoading ? "Loading…" : "No match"}</Command.Empty>
            {me?.name && (
              <Row value={`me ${me.name}`} onSelect={() => pick(me.name)} selected={value === me.name}>
                <Avatar name={me.name} size={18} /> Assign to me <span className="text-fg-4">({me.name})</span>
              </Row>
            )}
            {value && (
              <Row value="unassign nobody" onSelect={() => pick("")}>
                <UserMinus className="size-4 text-fg-3" /> Unassign
              </Row>
            )}
            {list.map((n) => (
              <Row key={n} value={n} onSelect={() => pick(n)} selected={value === n}>
                <Avatar name={n} size={18} /> {n}
              </Row>
            ))}
            {typed && !list.includes(typed) && typed !== me?.name && (
              <Row value={`assign ${typed}`} onSelect={() => pick(typed)}>
                <UserPlus className="size-4 text-fg-3" /> Assign to “{typed}”
              </Row>
            )}
          </Command.List>
        </Command>
      </PopoverContent>
    </Popover>
  );
}

function Row({ children, onSelect, value, selected }: { children: React.ReactNode; onSelect: () => void; value: string; selected?: boolean }) {
  return (
    <Command.Item
      value={value}
      onSelect={onSelect}
      className="flex h-8 cursor-default select-none items-center gap-2 rounded-md px-2 text-sm data-[selected=true]:bg-hover"
    >
      {children}
      {selected && <Check className="ml-auto size-3.5 text-accent" />}
    </Command.Item>
  );
}
