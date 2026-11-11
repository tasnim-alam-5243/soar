import { lazy, Suspense, useEffect, useState } from "react";
import { Navigate, Route, Routes, useLocation, useNavigate } from "react-router-dom";
import { Layout } from "./components/Layout";
import { Loading, ErrorBox } from "./components/common";
import { completeOIDC, useAuth } from "./lib/auth";
import Login from "./pages/Login";
import Dashboard from "./pages/Dashboard";
import Alerts from "./pages/Alerts";
import Cases from "./pages/Cases";
import CaseDetail from "./pages/CaseDetail";
import Workflows from "./pages/Workflows";
import Executions from "./pages/Executions";
import ExecutionDetail from "./pages/ExecutionDetail";
import Approvals from "./pages/Approvals";
import Sources from "./pages/Sources";
import Connections from "./pages/Connections";
import Admin from "./pages/Admin";

// The editor pulls in Monaco and React Flow; load it on demand.
const WorkflowEditor = lazy(() => import("./pages/WorkflowEditor"));

function Callback() {
  const nav = useNavigate();
  const [err, setErr] = useState<unknown>(null);
  useEffect(() => {
    completeOIDC().then((to) => nav(to, { replace: true })).catch(setErr);
  }, [nav]);
  return err ? <div className="p-6"><ErrorBox error={err} /></div> : <Loading label="Signing in" />;
}

function Protected({ children }: { children: React.ReactNode }) {
  const token = useAuth((s) => s.token);
  const loc = useLocation();
  if (!token) return <Navigate to={`/login?next=${encodeURIComponent(loc.pathname + loc.search)}`} replace />;
  return <>{children}</>;
}

export default function App() {
  return (
    <Routes>
      <Route path="/login" element={<Login />} />
      <Route path="/callback" element={<Callback />} />
      <Route element={<Protected><Layout /></Protected>}>
        <Route index element={<Dashboard />} />
        <Route path="alerts" element={<Alerts />} />
        <Route path="cases" element={<Cases />} />
        <Route path="cases/:id" element={<CaseDetail />} />
        <Route path="workflows" element={<Workflows />} />
        <Route path="workflows/:id" element={<Suspense fallback={<Loading />}><WorkflowEditor /></Suspense>} />
        <Route path="executions" element={<Executions />} />
        <Route path="executions/:id" element={<ExecutionDetail />} />
        <Route path="approvals" element={<Approvals />} />
        <Route path="sources" element={<Sources />} />
        <Route path="connections" element={<Connections />} />
        <Route path="admin" element={<Admin />} />
        <Route path="*" element={<Navigate to="/" replace />} />
      </Route>
    </Routes>
  );
}
