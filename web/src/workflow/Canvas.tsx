import { useCallback, useMemo } from "react";
import {
  Background, BackgroundVariant, Controls, Handle, MarkerType, MiniMap, Position, ReactFlow, ReactFlowProvider,
  type Connection, type Edge, type EdgeChange, type Node, type NodeChange, type NodeProps,
} from "@xyflow/react";
import "@xyflow/react/dist/style.css";
import { GitBranch, Hourglass, ListTree, Play, ShieldQuestion, Split, Square, Variable, Workflow, Zap } from "lucide-react";
import { type Definition, type FlowNodeData, connect, disconnect, graphFrom, moveNode, nodeType, removeNode } from "@/lib/dsl";
import { cn } from "@/lib/utils";

const icons: Record<string, typeof Zap> = {
  action: Zap, branch: GitBranch, parallel: Split, foreach: ListTree, wait: Hourglass, approval: ShieldQuestion,
  set: Variable, subworkflow: Workflow, end: Square,
};

// node state during a run: border + a small status line, never color alone
const stateStyle: Record<string, { ring: string; dot: string }> = {
  running: { ring: "border-accent shadow-[0_0_0_3px_rgb(var(--accent)/0.18)]", dot: "bg-accent animate-live" },
  retry_wait: { ring: "border-warn shadow-[0_0_0_3px_rgb(var(--warn)/0.15)]", dot: "bg-warn" },
  waiting: { ring: "border-warn shadow-[0_0_0_3px_rgb(var(--warn)/0.15)]", dot: "bg-warn animate-live" },
  succeeded: { ring: "border-good/60", dot: "bg-good" },
  failed: { ring: "border-crit shadow-[0_0_0_3px_rgb(var(--crit)/0.15)]", dot: "bg-crit" },
  skipped: { ring: "opacity-50", dot: "bg-fg-4" },
  cancelled: { ring: "opacity-50", dot: "bg-fg-4" },
};

function SoarNode({ data, selected }: NodeProps<Node<FlowNodeData>>) {
  const n = data.node;
  const t = nodeType(n);
  const Icon = icons[t] ?? Play;
  const sub = n.action ?? (t === "subworkflow" ? String(n.workflow ?? "") : t === "wait" ? String(n.duration ?? n.until ?? "") : t);
  const st = data.status ? stateStyle[data.status] : undefined;
  return (
    <div
      className={cn(
        "w-[208px] rounded-lg border bg-raised px-3 py-2 text-xs transition-shadow",
        st?.ring ?? "border-line-strong",
        data.invalid && "border-crit shadow-[0_0_0_3px_rgb(var(--crit)/0.18)]",
        selected && "border-accent shadow-[0_0_0_3px_rgb(var(--accent)/0.25)]",
      )}
    >
      <Handle type="target" position={Position.Top} className="!size-2 !border-2" />
      <div className="flex items-center gap-2">
        <span className={cn("flex size-6 shrink-0 items-center justify-center rounded-md", t === "approval" ? "bg-warn/15 text-warn" : t === "branch" || t === "parallel" ? "bg-accent/15 text-accent" : t === "end" ? "bg-hover text-fg-3" : "bg-accent/10 text-accent")}>
          <Icon className="size-3.5" />
        </span>
        <div className="min-w-0 flex-1">
          <div className="truncate font-semibold text-fg">{n.id}</div>
          <div className="truncate font-mono text-[10.5px] text-fg-3">{sub}</div>
        </div>
      </div>
      {data.status && (
        <div className="mt-1.5 flex items-center gap-1.5 border-t border-line pt-1.5 text-[10.5px] text-fg-3">
          <span className={cn("size-1.5 rounded-full", st?.dot ?? "bg-fg-4")} />
          {data.status.replace("_", " ")}
          {data.attempt && data.attempt > 1 ? <span className="ml-auto">attempt {data.attempt}</span> : null}
        </div>
      )}
      <Handle type="source" position={Position.Bottom} className="!size-2 !border-2" />
    </div>
  );
}

const nodeTypes = { soar: SoarNode };

interface Props {
  def: Definition;
  onChange?: (d: Definition) => void; // undefined: read-only
  statuses?: Record<string, { status?: string; attempt?: number }>;
  invalid?: Set<string>;
  selected?: string;
  onSelect?: (id?: string) => void;
}

function Inner({ def, onChange, statuses, invalid, selected, onSelect }: Props) {
  const g = useMemo(() => graphFrom(def, statuses, invalid), [def, statuses, invalid]);
  const nodes = useMemo(() => g.nodes.map((n) => ({ ...n, selected: n.id === selected })), [g.nodes, selected]);
  const editable = !!onChange;
  const onNodesChange = useCallback((changes: NodeChange[]) => {
    if (!onChange) return;
    let d = def;
    for (const c of changes) {
      if (c.type === "position" && c.position && !c.dragging) d = moveNode(d, c.id, c.position.x, c.position.y);
      if (c.type === "remove") d = removeNode(d, c.id);
    }
    if (d !== def) onChange(d);
  }, [def, onChange]);
  const onEdgesChange = useCallback((changes: EdgeChange[]) => {
    if (!onChange) return;
    let d = def;
    for (const c of changes) if (c.type === "remove") {
      const e = g.edges.find((x) => x.id === c.id);
      if (e) d = disconnect(d, e);
    }
    if (d !== def) onChange(d);
  }, [def, onChange, g.edges]);
  const onConnect = useCallback((c: Connection) => { if (onChange && c.source && c.target) onChange(connect(def, c.source, c.target)); }, [def, onChange]);
  const edges = useMemo(() => g.edges.map((e: Edge) => ({
    ...e,
    markerEnd: { type: MarkerType.ArrowClosed, width: 16, height: 16, color: "rgb(var(--fg-4))" },
    style: { strokeWidth: 1.5, ...(e.style ?? {}) },
  })), [g.edges]);
  return (
    <ReactFlow
      nodes={nodes}
      edges={edges}
      nodeTypes={nodeTypes}
      onNodesChange={onNodesChange}
      onEdgesChange={onEdgesChange}
      onConnect={onConnect}
      onNodeClick={(_, n) => onSelect?.(n.id)}
      onPaneClick={() => onSelect?.(undefined)}
      nodesDraggable={editable}
      nodesConnectable={editable}
      elementsSelectable
      deleteKeyCode={editable ? ["Backspace", "Delete"] : null}
      fitView
      fitViewOptions={{ padding: 0.2, maxZoom: 1.1 }}
      minZoom={0.2}
      proOptions={{ hideAttribution: true }}
    >
      <Background variant={BackgroundVariant.Dots} gap={18} size={1} />
      <Controls showInteractive={false} position="bottom-left" />
      <MiniMap pannable zoomable position="bottom-right" nodeColor="rgb(var(--line-strong))" nodeStrokeWidth={0} className="!rounded-md !border !border-line" />
    </ReactFlow>
  );
}

export function Canvas(p: Props) {
  return (
    <ReactFlowProvider>
      <Inner {...p} />
    </ReactFlowProvider>
  );
}
