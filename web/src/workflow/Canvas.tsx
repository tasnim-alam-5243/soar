import { useCallback, useMemo } from "react";
import {
  Background, Controls, Handle, MarkerType, MiniMap, Position, ReactFlow, ReactFlowProvider,
  type Connection, type Edge, type Node, type NodeProps, type NodeChange, type EdgeChange,
} from "@xyflow/react";
import "@xyflow/react/dist/style.css";
import { GitBranch, Hourglass, ListTree, Play, ShieldQuestion, Split, Square, Variable, Workflow, Zap } from "lucide-react";
import { type Definition, type FlowNodeData, connect, disconnect, graphFrom, moveNode, nodeType, removeNode } from "@/lib/dsl";
import { cn } from "@/lib/utils";

const icons: Record<string, typeof Zap> = {
  action: Zap, branch: GitBranch, parallel: Split, foreach: ListTree, wait: Hourglass, approval: ShieldQuestion,
  set: Variable, subworkflow: Workflow, end: Square,
};

const statusRing: Record<string, string> = {
  running: "ring-2 ring-blue-500", retry_wait: "ring-2 ring-amber-500", waiting: "ring-2 ring-amber-500",
  succeeded: "ring-2 ring-green-500", failed: "ring-2 ring-red-500", skipped: "opacity-50", cancelled: "opacity-50",
};

function SoarNode({ data, selected }: NodeProps<Node<FlowNodeData>>) {
  const n = data.node;
  const t = nodeType(n);
  const Icon = icons[t] ?? Play;
  const sub = n.action ?? (t === "subworkflow" ? String(n.workflow ?? "") : t === "wait" ? String(n.duration ?? n.until ?? "") : t);
  return (
    <div className={cn("w-[200px] rounded-md border bg-card px-3 py-2 text-xs shadow-sm",
      data.status && statusRing[data.status], data.invalid && "ring-2 ring-red-500", selected && "border-primary")}>
      <Handle type="target" position={Position.Top} />
      <div className="flex items-center gap-1.5 font-semibold">
        <Icon className="h-3.5 w-3.5 text-primary" /> <span className="truncate">{n.id}</span>
        {data.status && <span className="ml-auto text-[10px] font-normal text-muted-foreground">{data.status}{data.attempt && data.attempt > 1 ? ` #${data.attempt}` : ""}</span>}
      </div>
      <div className="truncate font-mono text-[11px] text-muted-foreground">{sub}</div>
      <Handle type="source" position={Position.Bottom} />
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
  return (
    <ReactFlow
      nodes={nodes}
      edges={g.edges.map((e: Edge) => ({ ...e, markerEnd: { type: MarkerType.ArrowClosed } }))}
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
      proOptions={{ hideAttribution: true }}
    >
      <Background />
      <Controls showInteractive={false} />
      <MiniMap pannable zoomable className="!bg-card" />
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
