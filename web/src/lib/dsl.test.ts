import { describe, expect, it } from "vitest";
import { addNode, connect, disconnect, graphFrom, moveNode, nodeOfPath, removeNode, type Definition } from "./dsl";

const def: Definition = {
  nodes: [
    { id: "check", action: "http.request" },
    { id: "decide", type: "branch", cases: [{ when: "steps.check.output.score > 70", next: ["block"] }], default: ["close"] },
    { id: "block", action: "notify.log", on_error: ["close"] },
    { id: "close", action: "case.close" },
  ],
  edges: ["check -> decide"],
};

describe("graphFrom", () => {
  it("turns every kind of link into an edge", () => {
    const g = graphFrom(def, { check: { status: "running" } }, new Set(["block"]));
    expect(g.nodes.map((n) => n.id)).toEqual(["check", "decide", "block", "close"]);
    const kinds = g.edges.map((e) => `${e.source}>${e.target}:${(e.data as { kind: string }).kind}`).sort();
    expect(kinds).toEqual(["block>close:error", "check>decide:edge", "decide>block:case", "decide>close:default"]);
    expect(g.nodes.find((n) => n.id === "block")!.data.invalid).toBe(true);
    expect(g.edges.find((e) => e.source === "check")!.animated).toBe(true);
  });
  it("ignores links to unknown nodes", () => {
    const g = graphFrom({ nodes: [{ id: "a", action: "x", next: ["ghost"] }] });
    expect(g.edges).toHaveLength(0);
  });
});

describe("editing", () => {
  it("connect adds an edge, or a default for branches; no duplicates", () => {
    let d = connect(def, "close", "check");
    expect(d.edges).toContain("close -> check");
    d = connect(d, "close", "check");
    expect(d.edges!.filter((e) => e === "close -> check")).toHaveLength(1);
    d = connect(def, "decide", "check");
    expect(d.nodes![1].default).toEqual(["close", "check"]);
    expect(def.nodes![1].default).toEqual(["close"]); // the input is not mutated
  });
  it("disconnect removes exactly one link", () => {
    const g = graphFrom(def);
    const caseEdge = g.edges.find((e) => (e.data as { kind: string }).kind === "case")!;
    const d = disconnect(def, caseEdge);
    expect(d.nodes![1].cases![0].next).toEqual([]);
    const errEdge = g.edges.find((e) => (e.data as { kind: string }).kind === "error")!;
    expect(disconnect(def, errEdge).nodes![2].on_error).toBeUndefined();
    const plain = g.edges.find((e) => (e.data as { kind: string }).kind === "edge")!;
    expect(disconnect(def, plain).edges).toBeUndefined();
  });
  it("removeNode drops the node and every reference to it", () => {
    const d = removeNode(def, "close");
    expect(d.nodes!.map((n) => n.id)).toEqual(["check", "decide", "block"]);
    expect(d.nodes![1].default).toEqual([]);
    expect(d.nodes![2].on_error).toBeUndefined();
  });
  it("addNode picks a unique id and moveNode stores the position", () => {
    const a = addNode(def, "wait");
    const b = addNode(a.def, "wait");
    expect(a.id).not.toEqual(b.id);
    expect(b.def.nodes!.at(-1)!.duration).toBe("5m");
    const m = moveNode(b.def, b.id, 10.4, 20.6);
    expect(m.nodes!.at(-1)!.ui).toEqual({ x: 10, y: 21 });
  });
  it("nodeOfPath maps validation paths to node ids", () => {
    expect(nodeOfPath(def, "definition.nodes[2].params.url")).toBe("block");
    expect(nodeOfPath(def, "definition.edges[0]")).toBeUndefined();
  });
});
