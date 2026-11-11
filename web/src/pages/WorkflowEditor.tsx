import { useEffect, useMemo, useRef, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useNavigate, useParams } from "react-router-dom";
import Editor, { loader } from "@monaco-editor/react";
import * as monaco from "monaco-editor/esm/vs/editor/editor.api";
import "monaco-editor/esm/vs/basic-languages/yaml/yaml.contribution";
import editorWorker from "monaco-editor/esm/vs/editor/editor.worker?worker";
import yaml from "js-yaml";
import { CheckCircle2, CopyPlus, FlaskConical, Plus, Rocket, Save, Trash2 } from "lucide-react";
import { api, unwrap, type FieldError, type Workflow } from "@/lib/api";
import { addNode, type Definition, nodeKinds, nodeOfPath, removeNode } from "@/lib/dsl";
import { Canvas } from "@/workflow/Canvas";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { Select, Textarea } from "@/components/ui/input";
import { Dialog, DialogContent } from "@/components/ui/dialog";
import { ErrorBox, Loading, PageHeader, StatusBadge } from "@/components/common";

// Monaco runs from the bundle (no CDN): SOC networks are often closed.
self.MonacoEnvironment = { getWorker: () => new editorWorker() };
loader.config({ monaco });

export default function WorkflowEditor() {
  const { id = "" } = useParams();
  const q = useQuery({ queryKey: ["workflow", id], queryFn: () => unwrap(api().GET("/workflows/{id}", { params: { path: { id } } })) });
  if (q.isLoading) return <Loading />;
  if (q.error) return <ErrorBox error={q.error} />;
  return <EditorBody key={q.data!.id + q.data!.checksum} wf={q.data!} />;
}

function EditorBody({ wf }: { wf: Workflow }) {
  const qc = useQueryClient();
  const nav = useNavigate();
  const readOnly = wf.status !== "draft";
  const initial: Definition = useMemo(() => ({ ...(wf.definition as Definition), ...(wf.triggers?.length ? { triggers: wf.triggers } : {}) }), [wf]);
  const [def, setDef] = useState<Definition>(initial);
  const [text, setText] = useState(() => yaml.dump(initial, { lineWidth: 120, noRefs: true }));
  const [yamlError, setYamlError] = useState<string | null>(null);
  const [errors, setErrors] = useState<FieldError[]>([]);
  const [selected, setSelected] = useState<string>();
  const [dirty, setDirty] = useState(false);
  const [running, setRunning] = useState(false);
  const fromYaml = useRef(false);

  // canvas -> YAML
  const updateDef = (d: Definition) => {
    fromYaml.current = false;
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
      fromYaml.current = true;
      setDef(parsed as Definition);
      setYamlError(null);
      setDirty(true);
    } catch (e) {
      setYamlError(e instanceof Error ? e.message.split("\n")[0] : String(e));
    }
  };

  // the server is the only validator (spec 12): validate on every change, debounced
  useEffect(() => {
    const t = setTimeout(async () => {
      const { triggers: _t, ...body } = def as Definition & { triggers?: unknown };
      void _t;
      try {
        const r = await unwrap(api().POST("/workflows/validate", { body: { definition: { ...body, ...(def.triggers ? { triggers: def.triggers } : {}) } } }));
        setErrors(r.errors);
      } catch {
        /* the save will show it */
      }
    }, 500);
    return () => clearTimeout(t);
  }, [def]);
  const invalid = useMemo(() => new Set(errors.map((e) => nodeOfPath(def, e.path)).filter(Boolean) as string[]), [errors, def]);

  const save = useMutation({
    mutationFn: async () => {
      const { triggers, ...definition } = def as Definition & { triggers?: Workflow["triggers"] };
      return unwrap(api().PUT("/workflows/{id}", { params: { path: { id: wf.id } }, body: { name: wf.name, description: wf.description, definition, triggers } }));
    },
    onSuccess: () => { setDirty(false); qc.invalidateQueries({ queryKey: ["workflows"] }); },
  });
  const publish = useMutation({
    mutationFn: async () => {
      if (dirty) await save.mutateAsync();
      return unwrap(api().POST("/workflows/{id}/publish", { params: { path: { id: wf.id } } }));
    },
    onSuccess: () => qc.invalidateQueries({ queryKey: ["workflow", wf.id] }),
  });
  const newVersion = useMutation({
    mutationFn: () => unwrap(api().POST("/workflows/{id}/versions", { params: { path: { id: wf.id } }, body: {} })),
    onSuccess: (w) => nav(`/workflows/${w.id}`),
  });

  const sel = def.nodes?.find((n) => n.id === selected);
  return (
    <div className="flex h-[calc(100vh-3rem)] flex-col">
      <PageHeader
        title={`${wf.name} · v${wf.version}`}
        subtitle={readOnly ? "This version is immutable. Create a new version to change it." : dirty ? "Unsaved changes" : "Saved"}
        actions={
          <>
            <StatusBadge status={wf.status} />
            {errors.length === 0 && !yamlError ? <span className="flex items-center gap-1 text-xs text-green-600"><CheckCircle2 className="h-4 w-4" /> valid</span>
              : <span className="text-xs text-red-600">{yamlError ? "YAML error" : `${errors.length} problem(s)`}</span>}
            <Button variant="outline" onClick={() => setRunning(true)}><FlaskConical className="h-4 w-4" /> Test run</Button>
            {readOnly ? (
              <Button onClick={() => newVersion.mutate()} disabled={newVersion.isPending}><CopyPlus className="h-4 w-4" /> New version</Button>
            ) : (
              <>
                <Button variant="outline" onClick={() => save.mutate()} disabled={!dirty || !!yamlError || save.isPending}><Save className="h-4 w-4" /> Save</Button>
                <Button onClick={() => publish.mutate()} disabled={errors.length > 0 || !!yamlError || publish.isPending}><Rocket className="h-4 w-4" /> Publish</Button>
              </>
            )}
          </>
        }
      />
      <ErrorBox error={save.error ?? publish.error ?? newVersion.error} />
      <div className="grid min-h-0 flex-1 grid-cols-1 gap-3 lg:grid-cols-2">
        <Card className="relative min-h-[400px] overflow-hidden">
          {!readOnly && (
            <div className="absolute left-2 top-2 z-10 flex flex-wrap gap-1">
              <Select className="h-8 w-36 text-xs" value="" onChange={(e) => { if (!e.target.value) return; const r = addNode(def, e.target.value); updateDef(r.def); setSelected(r.id); }}>
                <option value="">+ Add node…</option>
                {nodeKinds.map((k) => <option key={k} value={k}>{k}</option>)}
              </Select>
              {selected && <Button size="sm" variant="outline" onClick={() => { updateDef(removeNode(def, selected)); setSelected(undefined); }}><Trash2 className="h-3.5 w-3.5" /> {selected}</Button>}
            </div>
          )}
          <Canvas def={def} onChange={readOnly ? undefined : updateDef} invalid={invalid} selected={selected} onSelect={setSelected} />
        </Card>
        <div className="flex min-h-0 flex-col gap-3">
          <Card className="min-h-[300px] flex-1 overflow-hidden">
            <Editor height="100%" language="yaml" value={text} onChange={onText} theme={document.documentElement.classList.contains("dark") ? "vs-dark" : "light"}
              options={{ readOnly, minimap: { enabled: false }, fontSize: 12, tabSize: 2, scrollBeyondLastLine: false }} />
          </Card>
          {(yamlError || errors.length > 0) && (
            <Card className="max-h-40 overflow-auto p-3 text-xs">
              {yamlError && <div className="text-red-600">YAML: {yamlError}</div>}
              {errors.map((e, i) => (
                <button key={i} className="block w-full text-left hover:bg-accent" onClick={() => setSelected(nodeOfPath(def, e.path))}>
                  <code className="text-red-600">{e.path}</code> {e.message}
                </button>
              ))}
            </Card>
          )}
          {sel && <Card className="p-3 text-xs"><span className="font-semibold">{sel.id}</span>: edit this node in the YAML. Drag from a node's bottom handle to another node to connect them; select an edge and press Delete to remove it.</Card>}
        </div>
      </div>
      {running && <TestRun wf={wf} onClose={() => setRunning(false)} />}
      {!readOnly && <div className="hidden"><Plus /></div>}
    </div>
  );
}

function TestRun({ wf, onClose }: { wf: Workflow; onClose: () => void }) {
  const nav = useNavigate();
  const [inputs, setInputs] = useState("{}");
  const [alertID, setAlertID] = useState("");
  const [dry, setDry] = useState(true);
  const m = useMutation({
    mutationFn: () => unwrap(api().POST("/workflows/{id}/run", { params: { path: { id: wf.id } }, body: { inputs: JSON.parse(inputs || "{}"), dry_run: dry, ...(alertID ? { alert_id: alertID } : {}) } })),
    onSuccess: (e) => nav(`/executions/${e.id}`),
  });
  return (
    <Dialog open onOpenChange={(o) => !o && onClose()}>
      <DialogContent title={`Run ${wf.name} v${wf.version}`}>
        <div className="flex flex-col gap-3 text-sm">
          <label className="flex items-center gap-2"><input type="checkbox" checked={dry} onChange={(e) => setDry(e.target.checked)} disabled={wf.status !== "published"} />
            Dry run (write and destructive actions only report what they would do){wf.status !== "published" && " — drafts always run dry"}</label>
          <div><div className="mb-1 text-xs text-muted-foreground">Inputs (JSON)</div><Textarea value={inputs} onChange={(e) => setInputs(e.target.value)} /></div>
          <div><div className="mb-1 text-xs text-muted-foreground">Alert id (optional, available as alert.*)</div>
            <input className="h-9 w-full rounded-md border bg-background px-2 font-mono text-xs" value={alertID} onChange={(e) => setAlertID(e.target.value)} placeholder="alt_…" /></div>
          <ErrorBox error={m.error} />
          <Button onClick={() => m.mutate()} disabled={m.isPending}>Start</Button>
        </div>
      </DialogContent>
    </Dialog>
  );
}
