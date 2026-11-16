import dagre from "@dagrejs/dagre";
import type { Edge, Node } from "@xyflow/react";

// The workflow definition is plain JSON (the API validates it). The canvas and the YAML
// editor both read and write this same object, so they never disagree.
export interface DSLNode {
  id: string;
  type?: string;
  action?: string;
  next?: string[] | string;
  on_error?: string[] | string;
  on_reject?: string[] | string;
  branches?: string[];
  cases?: { when: string; next?: string[] | string }[];
  default?: string[] | string;
  ui?: { x: number; y: number };
  [k: string]: unknown;
}

export interface Definition {
  nodes?: DSLNode[];
  edges?: string[];
  [k: string]: unknown;
}

export type EdgeKind = "next" | "edge" | "case" | "default" | "branch" | "error" | "reject";

export interface FlowNodeData extends Record<string, unknown> {
  node: DSLNode;
  status?: string;
  attempt?: number;
  invalid?: boolean;
  selected?: boolean;
}

const list = (v: unknown): string[] => (Array.isArray(v) ? (v as string[]) : typeof v === "string" ? [v] : []);

export const nodeType = (n: DSLNode) => n.type ?? (n.action ? "action" : "action");

export function graphFrom(def: Definition, statuses: Record<string, { status?: string; attempt?: number }> = {}, invalid: Set<string> = new Set()) {
  const nodes = def.nodes ?? [];
  const ids = new Set(nodes.map((n) => n.id));
  const edges: Edge[] = [];
  const add = (src: string, dst: string, kind: EdgeKind, label?: string, idx?: number) => {
    if (!ids.has(dst)) return;
    edges.push({
      id: `${src}|${kind}|${idx ?? ""}|${dst}`,
      source: src,
      target: dst,
      label,
      type: "smoothstep",
      animated: statuses[src]?.status === "running",
      style: kind === "error" ? { stroke: "rgb(var(--crit))", strokeDasharray: "5 4" } : kind === "reject" ? { stroke: "rgb(var(--serious))", strokeDasharray: "5 4" } : undefined,
      data: { kind, idx },
    });
  };
  for (const n of nodes) {
    list(n.next).forEach((t) => add(n.id, t, "next"));
    (n.cases ?? []).forEach((c, i) => list(c.next).forEach((t) => add(n.id, t, "case", c.when.length > 28 ? c.when.slice(0, 26) + "…" : c.when, i)));
    list(n.default).forEach((t) => add(n.id, t, "default", "default"));
    list(n.branches).forEach((t) => add(n.id, t, "branch"));
    list(n.on_error).forEach((t) => add(n.id, t, "error", "on error"));
    list(n.on_reject).forEach((t) => add(n.id, t, "reject", "rejected"));
  }
  (def.edges ?? []).forEach((e, i) => {
    const [a, b] = e.split("->").map((s) => s.trim());
    if (a && b && ids.has(a)) add(a, b, "edge", undefined, i);
  });
  const placed = layout(nodes, edges);
  const rf: Node<FlowNodeData>[] = nodes.map((n) => ({
    id: n.id,
    type: "soar",
    position: n.ui ?? placed[n.id] ?? { x: 0, y: 0 },
    data: { node: n, status: statuses[n.id]?.status, attempt: statuses[n.id]?.attempt, invalid: invalid.has(n.id) },
  }));
  return { nodes: rf, edges };
}

function layout(nodes: DSLNode[], edges: Edge[]): Record<string, { x: number; y: number }> {
  const g = new dagre.graphlib.Graph();
  g.setGraph({ rankdir: "TB", nodesep: 40, ranksep: 60 });
  g.setDefaultEdgeLabel(() => ({}));
  nodes.forEach((n) => g.setNode(n.id, { width: 200, height: 64 }));
  edges.forEach((e) => g.setEdge(e.source, e.target));
  dagre.layout(g);
  const out: Record<string, { x: number; y: number }> = {};
  nodes.forEach((n) => {
    const p = g.node(n.id);
    if (p) out[n.id] = { x: p.x - 100, y: p.y - 32 };
  });
  return out;
}

const clone = <T,>(v: T): T => JSON.parse(JSON.stringify(v));

// connect adds src -> dst: branch nodes get it as their default, others as an edge.
export function connect(def: Definition, src: string, dst: string): Definition {
  const d = clone(def);
  const n = d.nodes?.find((x) => x.id === src);
  if (!n || src === dst) return def;
  if (nodeType(n) === "branch") n.default = [...list(n.default).filter((t) => t !== dst), dst];
  else if (nodeType(n) === "end") return def;
  else {
    const e = `${src} -> ${dst}`;
    d.edges = [...(d.edges ?? []).filter((x) => x.replace(/\s/g, "") !== e.replace(/\s/g, "")), e];
  }
  return d;
}

// disconnect removes one edge identified by its kind (and case index).
export function disconnect(def: Definition, edge: Edge): Definition {
  const d = clone(def);
  const n = d.nodes?.find((x) => x.id === edge.source);
  const { kind, idx } = (edge.data ?? {}) as { kind: EdgeKind; idx?: number };
  const drop = (v: unknown) => list(v).filter((t) => t !== edge.target);
  if (kind === "edge") d.edges = (d.edges ?? []).filter((_, i) => i !== idx);
  if (!n) return d;
  if (kind === "next") n.next = drop(n.next);
  if (kind === "default") n.default = drop(n.default);
  if (kind === "branch") n.branches = drop(n.branches);
  if (kind === "error") n.on_error = drop(n.on_error);
  if (kind === "reject") n.on_reject = drop(n.on_reject);
  if (kind === "case" && n.cases && idx !== undefined) n.cases[idx].next = drop(n.cases[idx].next);
  return tidy(d);
}

export function removeNode(def: Definition, id: string): Definition {
  let d = clone(def);
  d.nodes = (d.nodes ?? []).filter((n) => n.id !== id);
  d.edges = (d.edges ?? []).filter((e) => !e.split("->").map((s) => s.trim()).includes(id));
  for (const n of d.nodes) {
    for (const k of ["next", "default", "branches", "on_error", "on_reject"] as const) if (n[k] !== undefined) n[k] = list(n[k]).filter((t) => t !== id);
    n.cases?.forEach((c) => (c.next = list(c.next).filter((t) => t !== id)));
  }
  d = tidy(d);
  return d;
}

export function moveNode(def: Definition, id: string, x: number, y: number): Definition {
  const d = clone(def);
  const n = d.nodes?.find((m) => m.id === id);
  if (n) n.ui = { x: Math.round(x), y: Math.round(y) };
  return d;
}

const templates: Record<string, Partial<DSLNode>> = {
  action: { action: "notify.log", params: { message: '"hello"' } },
  branch: { type: "branch", cases: [{ when: "true", next: [] }], default: [] },
  parallel: { type: "parallel", branches: [] },
  foreach: { type: "foreach", items: "[]", action: "notify.log", concurrency: 4, params: { message: "item" } },
  wait: { type: "wait", duration: "5m" },
  approval: { type: "approval", message: '"Please approve"', timeout: "24h" },
  set: { type: "set", vars: { name: '"value"' } },
  subworkflow: { type: "subworkflow", workflow: "other_workflow" },
  end: { type: "end", status: "succeeded" },
};

export const nodeKinds = Object.keys(templates);

export function addNode(def: Definition, kind: string, at?: { x: number; y: number }): { def: Definition; id: string } {
  const d = clone(def);
  d.nodes = d.nodes ?? [];
  let i = d.nodes.length + 1;
  let id = `${kind}_${i}`;
  while (d.nodes.some((n) => n.id === id)) id = `${kind}_${++i}`;
  d.nodes.push({ id, ...clone(templates[kind] ?? templates.action), ...(at ? { ui: at } : {}) } as DSLNode);
  return { def: d, id };
}

// tidy drops empty lists so the YAML stays readable.
function tidy(d: Definition): Definition {
  for (const n of d.nodes ?? []) {
    for (const k of ["next", "on_error", "on_reject", "branches"] as const) if (Array.isArray(n[k]) && (n[k] as string[]).length === 0) delete n[k];
  }
  if (d.edges && d.edges.length === 0) delete d.edges;
  return d;
}

// nodeOfPath maps a validation path (definition.nodes[3].params.url) to a node id.
export function nodeOfPath(def: Definition, path: string): string | undefined {
  const m = /definition\.nodes\[(\d+)\]/.exec(path);
  return m ? def.nodes?.[Number(m[1])]?.id : undefined;
}
