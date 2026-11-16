import { useEffect, useMemo, useRef, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, useNavigate, useParams } from "react-router-dom";
import Editor, { loader } from "@monaco-editor/react";
import * as monaco from "monaco-editor/esm/vs/editor/editor.api";
import "monaco-editor/esm/vs/basic-languages/yaml/yaml.contribution";
import editorWorker from "monaco-editor/esm/vs/editor/editor.worker?worker";
import yaml from "js-yaml";
import { toast } from "sonner";
import {
  AlertCircle, ArrowLeft, CheckCircle2, ChevronDown, CopyPlus, FileCode2, FlaskConical, GitBranch, Hourglass, ListTree, Loader2, MousePointerClick,
  Play, Plus, Rocket, Save, ShieldQuestion, Split, Square, Trash2, Variable, Workflow as WorkflowIcon, Zap,
} from "lucide-react";
import { api, unwrap, type FieldError, type Workflow } from "@/lib/api";
import { addNode, type Definition, nodeKinds, nodeOfPath, removeNode } from "@/lib/dsl";
import { useHotkeys, modKey } from "@/lib/hotkeys";
import { requires, usePerms } from "@/lib/perms";
import { usePrefs } from "@/lib/prefs";
import { cn } from "@/lib/utils";
import { Canvas } from "@/workflow/Canvas";
import { Button } from "@/components/ui/button";
import { Dialog, DialogContent } from "@/components/ui/dialog";
import { FormField, Input, Switch, Textarea } from "@/components/ui/input";
import { Menu, MenuContent, MenuItem, MenuLabel, MenuTrigger } from "@/components/ui/menu";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { ErrorBox, JsonView, Loading, StatusBadge } from "@/components/common";

// Monaco runs from the bundle (no CDN): SOC networks are often closed, and the CSP allows only 'self'.
self.MonacoEnvironment = { getWorker: () => new editorWorker() };
loader.config({ monaco });
monaco.editor.defineTheme("soar-dark", {
  base: "vs-dark", inherit: true, rules: [{ token: "type", foreground: "a5adff" }, { token: "string", foreground: "9ece6a" }, { token: "number", foreground: "ff9e64" }],
  colors: { "editor.background": "#111316", "editorGutter.background": "#111316", "editor.lineHighlightBackground": "#171a1e", "editorLineNumber.foreground": "#525963", "editor.selectionBackground": "#7c86ff40" },
});
monaco.editor.defineTheme("soar-light", {
  base: "vs", inherit: true, rules: [],
  colors: { "editor.background": "#ffffff", "editorGutter.background": "#ffffff", "editor.lineHighlightBackground": "#f6f7f9", "editor.selectionBackground": "#4f56d630" },
});

const kindMeta: Record<string, { icon: React.ElementType; text: string }> = {
  action: { icon: Zap, text: "Run an integration or built-in action" },
  branch: { icon: GitBranch, text: "Choose a path with CEL conditions" },
  parallel: { icon: Split, text: "Run branches at the same time" },
  foreach: { icon: ListTree, text: "Repeat an action over a list" },
  wait: { icon: Hourglass, text: "Pause for a duration or until a time" },
  approval: { icon: ShieldQuestion, text: "Ask a human to approve" },
  set: { icon: Variable, text: "Set workflow variables" },
  subworkflow: { icon: WorkflowIcon, text: "Call another workflow" },
  end: { icon: Square, text: "Finish with a status" },
};

export default function WorkflowEditor() {
  const { id = "" } = useParams();
  const q = useQuery({ queryKey: ["workflow", id], queryFn: () => unwrap(api().GET("/workflows/{id}", { params: { path: { id } } })) });
  if (q.isLoading) return <Loading />;
  if (q.error) return <div className="p-6"><ErrorBox error={q.error} /></div>;
  return <EditorBody key={q.data!.id + q.data!.checksum} wf={q.data!} />;
}

function EditorBody({ wf }: { wf: Workflow }) {
  const qc = useQueryClient();
  const nav = useNavigate();
  const { can } = usePerms();
  const theme = usePrefs((s) => s.theme);
  const canWrite = can("workflows:write");
  const readOnly = wf.status !== "draft" || !canWrite;
  const initial: Definition = useMemo(() => ({ ...(wf.definition as Definition), ...(wf.triggers?.length ? { triggers: wf.triggers } : {}) }), [wf]);
  const [def, setDef] = useState<Definition>(initial);
  const [text, setText] = useState(() => yaml.dump(initial, { lineWidth: 120, noRefs: true }));
  const [yamlError, setYamlError] = useState<string | null>(null);
  const [errors, setErrors] = useState<FieldError[]>([]);
  const [validating, setValidating] = useState(false);
  const [selected, setSelected] = useState<string>();
  const [dirty, setDirty] = useState(false);
  const [running, setRunning] = useState(false);
  const [tab, setTab] = useState("yaml");
  const editorRef = useRef<monaco.editor.IStandaloneCodeEditor | null>(null);

  // canvas -> YAML
  const updateDef = (d: Definition) => {
    setDef(d);
    setText(yaml.dump(d, { lineWidth: 120, noRefs: true }));
    setDirty(true);
  };
  // YAML -> canvas
  const onText = (t?: string) => {
    setText(t ?? "");
    try {
      const parsed = yaml.load(t ?? "");
      if (!parsed || typeof parsed !== "object" || Array.isArray(parsed)) throw new Error("the definition must be a YAML mapping");
      setDef(parsed as Definition);
      setYamlError(null);
      setDirty(true);
    } catch (e) {
      setYamlError(e instanceof Error ? e.message.split("\n")[0] : String(e));
    }
  };

  // the server is the only validator (spec 12): validate on every change, debounced
  useEffect(() => {
    setValidating(true);
    const t = setTimeout(async () => {
      try {
        const r = await unwrap(api().POST("/workflows/validate", { body: { definition: def } }));
        setErrors(r.errors);
      } catch {
        /* the save shows it */
      } finally {
        setValidating(false);
      }
    }, 450);
    return () => clearTimeout(t);
  }, [def]);
  const invalid = useMemo(() => new Set(errors.map((e) => nodeOfPath(def, e.path)).filter(Boolean) as string[]), [errors, def]);

  const save = useMutation({
    mutationFn: async () => {
      const { triggers, ...definition } = def as Definition & { triggers?: Workflow["triggers"] };
      return unwrap(api().PUT("/workflows/{id}", { params: { path: { id: wf.id } }, body: { name: wf.name, description: wf.description, definition, triggers } }));
    },
    onSuccess: () => { setDirty(false); qc.invalidateQueries({ queryKey: ["workflows"] }); toast.success("Draft saved"); },
  });
  const publish = useMutation({
    mutationFn: async () => {
      if (dirty) await save.mutateAsync();
      return unwrap(api().POST("/workflows/{id}/publish", { params: { path: { id: wf.id } } }));
    },
    onSuccess: () => { qc.invalidateQueries({ queryKey: ["workflow", wf.id] }); qc.invalidateQueries({ queryKey: ["workflows"] }); toast.success(`${wf.name} v${wf.version} published`); },
  });
  const newVersion = useMutation({
    mutationFn: () => unwrap(api().POST("/workflows/{id}/versions", { params: { path: { id: wf.id } }, body: {} })),
    onSuccess: (w) => nav(`/workflows/${w.id}`),
  });
  useHotkeys({ "mod+s": () => !readOnly && dirty && !yamlError && save.mutate() }, [readOnly, dirty, yamlError]);
  useEffect(() => {
    const warn = (e: BeforeUnloadEvent) => { if (dirty) e.preventDefault(); };
    window.addEventListener("beforeunload", warn);
    return () => window.removeEventListener("beforeunload", warn);
  }, [dirty]);

  const sel = def.nodes?.find((n) => n.id === selected);
  const problems = errors.length + (yamlError ? 1 : 0);
  const jumpTo = (path: string) => {
    const nodeId = nodeOfPath(def, path);
    if (nodeId) setSelected(nodeId);
    const ed = editorRef.current;
    if (ed && nodeId) {
      const m = ed.getModel()?.findMatches(`id: ${nodeId}`, false, false, true, null, false)[0];
      if (m) { setTab("yaml"); ed.revealLineInCenter(m.range.startLineNumber); ed.setPosition({ lineNumber: m.range.startLineNumber, column: 1 }); }
    }
  };
  const dark = theme === "dark" || (theme === "system" && window.matchMedia?.("(prefers-color-scheme: dark)").matches);

  return (
    <div className="flex h-full flex-col">
      <div className="flex h-14 shrink-0 items-center gap-3 border-b border-line bg-panel px-4">
        <Link to="/workflows" className="rounded-md p-1.5 text-fg-3 hover:bg-hover hover:text-fg" aria-label="Back to workflows"><ArrowLeft className="size-4" /></Link>
        <div className="min-w-0">
          <div className="flex items-center gap-2">
            <h1 className="truncate text-[15px] font-semibold">{wf.name}</h1>
            <span className="rounded bg-hover px-1.5 font-mono text-2xs text-fg-2">v{wf.version}</span>
            <StatusBadge status={wf.status} />
          </div>
          <div className="text-xs text-fg-3">
            {wf.status !== "draft" ? "Published versions are immutable. Create a new version to change it." : !canWrite ? requires("workflows:write") : dirty ? "Unsaved changes" : "All changes saved"}
          </div>
        </div>
        <div className="ml-auto flex items-center gap-2">
          <span className={cn("hidden items-center gap-1.5 text-xs md:inline-flex", problems ? "text-crit" : "text-good")}>
            {validating ? <Loader2 className="size-3.5 animate-spin text-fg-3" /> : problems ? <AlertCircle className="size-3.5" /> : <CheckCircle2 className="size-3.5" />}
            {validating ? <span className="text-fg-3">Validating</span> : problems ? <button onClick={() => setTab("problems")} className="hover:underline">{problems} problem{problems > 1 ? "s" : ""}</button> : "Valid"}
          </span>
          <Button onClick={() => setRunning(true)} disabled={!can("workflows:run")} tip={can("workflows:run") ? undefined : requires("workflows:run")}><FlaskConical /> Test run</Button>
          {wf.status !== "draft" ? (
            <Button variant="primary" onClick={() => newVersion.mutate()} loading={newVersion.isPending} disabled={!canWrite} tip={canWrite ? undefined : requires("workflows:write")}><CopyPlus /> New version</Button>
          ) : (
            <>
              <Button onClick={() => save.mutate()} disabled={readOnly || !dirty || !!yamlError} loading={save.isPending} kbd={`${modKey}S`}><Save /> Save</Button>
              <Button variant="primary" onClick={() => publish.mutate()} disabled={readOnly || problems > 0} loading={publish.isPending} tip={problems ? "Fix the problems before publishing" : undefined}><Rocket /> Publish</Button>
            </>
          )}
        </div>
      </div>
      {(save.error || publish.error || newVersion.error) && <div className="border-b border-line p-3"><ErrorBox error={save.error ?? publish.error ?? newVersion.error} /></div>}

      <div className="flex min-h-0 flex-1">
        <div className="relative min-w-0 flex-1 bg-bg">
          {!readOnly && (
            <div className="absolute left-3 top-3 z-10 flex items-center gap-1.5">
              <Menu>
                <MenuTrigger asChild><Button size="sm" className="shadow-pop"><Plus /> Add node <ChevronDown className="size-3.5 text-fg-3" /></Button></MenuTrigger>
                <MenuContent align="start" className="w-72">
                  <MenuLabel>Node types</MenuLabel>
                  {nodeKinds.map((k) => {
                    const m = kindMeta[k] ?? { icon: Zap, text: "" };
                    return (
                      <MenuItem key={k} icon={<m.icon />} onSelect={() => { const r = addNode(def, k); updateDef(r.def); setSelected(r.id); setTab("node"); }} className="h-auto py-1.5">
                        <span className="flex flex-col"><span className="font-medium capitalize">{k}</span><span className="text-xs text-fg-3">{m.text}</span></span>
                      </MenuItem>
                    );
                  })}
                </MenuContent>
              </Menu>
              {selected && <Button size="sm" variant="danger-ghost" className="bg-raised shadow-pop" onClick={() => { updateDef(removeNode(def, selected)); setSelected(undefined); }}><Trash2 /> Delete {selected}</Button>}
            </div>
          )}
          <Canvas def={def} onChange={readOnly ? undefined : updateDef} invalid={invalid} selected={selected} onSelect={(id) => { setSelected(id); if (id) setTab("node"); }} />
        </div>

        <div className="flex w-[min(560px,44vw)] shrink-0 flex-col border-l border-line bg-panel">
          <Tabs value={tab} onValueChange={setTab} className="flex min-h-0 flex-1 flex-col">
            <TabsList className="px-3">
              <TabsTrigger value="yaml"><FileCode2 className="size-3.5" /> Definition</TabsTrigger>
              <TabsTrigger value="problems" count={problems}>Problems</TabsTrigger>
              <TabsTrigger value="node"><MousePointerClick className="size-3.5" /> {sel ? sel.id : "Node"}</TabsTrigger>
            </TabsList>
            <TabsContent value="yaml" forceMount className="min-h-0 flex-1 pt-0 data-[state=inactive]:hidden">
              <Editor
                height="100%"
                language="yaml"
                value={text}
                onChange={onText}
                onMount={(ed) => { editorRef.current = ed; }}
                theme={dark ? "soar-dark" : "soar-light"}
                options={{ readOnly, minimap: { enabled: false }, fontSize: 12.5, fontFamily: "'JetBrains Mono Variable', ui-monospace, monospace", tabSize: 2, scrollBeyondLastLine: false, padding: { top: 12 }, renderLineHighlight: "line", smoothScrolling: true }}
              />
            </TabsContent>
            <TabsContent value="problems" className="min-h-0 flex-1 overflow-y-auto p-3 pt-3">
              {!problems ? (
                <div className="flex flex-col items-center gap-2 py-10 text-center text-sm text-fg-3"><CheckCircle2 className="size-6 text-good" /> The server accepts this definition.</div>
              ) : (
                <div className="flex flex-col gap-1">
                  {yamlError && <div className="rounded-md border border-crit/30 bg-crit/5 px-3 py-2 text-xs"><span className="font-medium text-crit">YAML</span> {yamlError}</div>}
                  {errors.map((e, i) => (
                    <button key={i} onClick={() => jumpTo(e.path)} className="flex flex-col items-start gap-0.5 rounded-md px-3 py-2 text-left hover:bg-hover">
                      <code className="text-xs text-crit">{e.path}</code>
                      <span className="text-sm text-fg-2">{e.message}</span>
                    </button>
                  ))}
                </div>
              )}
            </TabsContent>
            <TabsContent value="node" className="min-h-0 flex-1 overflow-y-auto p-4 pt-4">
              {sel ? (
                <div className="flex flex-col gap-4">
                  <div className="flex items-center gap-2">
                    {(() => { const m = kindMeta[sel.type ?? "action"] ?? kindMeta.action; return <span className="flex size-8 items-center justify-center rounded-lg bg-accent/10 text-accent"><m.icon className="size-4" /></span>; })()}
                    <div>
                      <div className="font-semibold">{sel.id}</div>
                      <div className="text-xs text-fg-3">{sel.type ?? "action"}{sel.action ? ` · ${sel.action}` : ""}</div>
                    </div>
                  </div>
                  {invalid.has(sel.id) && (
                    <div className="flex flex-col gap-1 rounded-md border border-crit/30 bg-crit/5 p-3 text-xs">
                      {errors.filter((e) => nodeOfPath(def, e.path) === sel.id).map((e, i) => <div key={i}><code className="text-crit">{e.path.replace(/^definition\.nodes\[\d+\]\.?/, "")}</code> {e.message}</div>)}
                    </div>
                  )}
                  <JsonView value={sel} />
                  <p className="text-xs leading-5 text-fg-3">
                    Edit this node in the Definition tab. Drag from a node's bottom handle to another node to connect them; select an edge and press Delete to remove it.
                  </p>
                </div>
              ) : (
                <div className="flex flex-col items-center gap-2 py-10 text-center text-sm text-fg-3"><MousePointerClick className="size-6" /> Select a node on the canvas to inspect it.</div>
              )}
            </TabsContent>
          </Tabs>
        </div>
      </div>
      {running && <TestRun wf={wf} onClose={() => setRunning(false)} />}
    </div>
  );
}

function TestRun({ wf, onClose }: { wf: Workflow; onClose: () => void }) {
  const nav = useNavigate();
  const [inputs, setInputs] = useState("{}");
  const [alertID, setAlertID] = useState("");
  const [dry, setDry] = useState(true);
  let parseError: string | null = null;
  try { JSON.parse(inputs || "{}"); } catch (e) { parseError = e instanceof Error ? e.message : String(e); }
  const m = useMutation({
    mutationFn: () => unwrap(api().POST("/workflows/{id}/run", { params: { path: { id: wf.id } }, body: { inputs: JSON.parse(inputs || "{}"), dry_run: dry, ...(alertID ? { alert_id: alertID.trim() } : {}) } })),
    onSuccess: (e) => nav(`/executions/${e.id}`),
  });
  return (
    <Dialog open onOpenChange={(o) => !o && onClose()}>
      <DialogContent
        title={`Run ${wf.name} v${wf.version}`}
        description="Starts an execution now and opens its live view."
        footer={<><Button variant="ghost" onClick={onClose}>Cancel</Button><Button variant="primary" onClick={() => m.mutate()} loading={m.isPending} disabled={!!parseError}><Play /> Start {dry ? "dry run" : "run"}</Button></>}
      >
        <div className="flex items-start justify-between gap-4 rounded-lg border border-line p-3">
          <div>
            <div className="text-sm font-medium">Dry run</div>
            <div className="text-xs text-fg-3">Write and destructive actions only report what they would do.{wf.status !== "published" && " Drafts always run dry."}</div>
          </div>
          <Switch checked={dry} onChange={setDry} disabled={wf.status !== "published"} label="Dry run" />
        </div>
        <FormField label="Inputs (JSON)" error={parseError ?? undefined}><Textarea value={inputs} onChange={(e) => setInputs(e.target.value)} className="min-h-[110px]" spellCheck={false} /></FormField>
        <FormField label="Alert id" hint="Optional: the run sees this alert as alert.*"><Input value={alertID} onChange={(e) => setAlertID(e.target.value)} placeholder="alt_…" className="font-mono text-xs" /></FormField>
        <ErrorBox error={m.error} />
      </DialogContent>
    </Dialog>
  );
}
