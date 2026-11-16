import { lazy, Suspense, useEffect, useState } from "react";
import { Navigate, Outlet, Route, Routes, useLocation, useNavigate } from "react-router-dom";
import { Layout } from "./components/Layout";
import { ErrorBox, Loading } from "./components/common";
import { completeOIDC, useAuth } from "./lib/auth";
import { usePerms } from "./lib/perms";
import Login from "./pages/Login";

// Pages load on demand: the first paint ships the shell and the page you opened only.
const Dashboard = lazy(() => import("./pages/Dashboard"));
const Alerts = lazy(() => import("./pages/Alerts"));
const Cases = lazy(() => import("./pages/Cases"));
const CaseDetail = lazy(() => import("./pages/CaseDetail"));
const Workflows = lazy(() => import("./pages/Workflows"));
const WorkflowEditor = lazy(() => import("./pages/WorkflowEditor"));
const Executions = lazy(() => import("./pages/Executions"));
const ExecutionDetail = lazy(() => import("./pages/ExecutionDetail"));
const Approvals = lazy(() => import("./pages/Approvals"));
const Sources = lazy(() => import("./pages/Sources"));
const Connections = lazy(() => import("./pages/Connections"));
const Admin = lazy(() => import("./pages/Admin"));

function Callback() {
  const nav = useNavigate();
  const [err, setErr] = useState<unknown>(null);
  useEffect(() => {
    completeOIDC().then((to) => nav(to, { replace: true })).catch(setErr);
  }, [nav]);
  return err ? <div className="mx-auto max-w-lg p-6"><ErrorBox error={err} /></div> : <Loading label="Signing in" />;
}

function Protected({ children }: { children: React.ReactNode }) {
  const token = useAuth((s) => s.token);
  const loc = useLocation();
  if (!token) return <Navigate to={`/login?next=${encodeURIComponent(loc.pathname + loc.search)}`} replace />;
  return <>{children}</>;
}

/** Home sends callers who cannot see the overview to the first page they can. */
function Home() {
  const { can, loading } = usePerms();
  if (loading) return <Loading />;
  if (can("alerts:read")) return <Dashboard />;
  if (can("cases:read")) return <Navigate to="/cases" replace />;
  if (can("workflows:read")) return <Navigate to="/workflows" replace />;
  return <Navigate to="/executions" replace />;
}

export default function App() {
  return (
    <Routes>
      <Route path="/login" element={<Login />} />
      <Route path="/callback" element={<Callback />} />
      <Route element={<Protected><Layout /></Protected>}>
        <Route element={<Suspense fallback={<Loading />}><OutletShim /></Suspense>}>
          <Route index element={<Home />} />
          <Route path="alerts" element={<Alerts />} />
          <Route path="cases" element={<Cases />} />
          <Route path="cases/:id" element={<CaseDetail />} />
          <Route path="workflows" element={<Workflows />} />
          <Route path="workflows/:id" element={<WorkflowEditor />} />
          <Route path="executions" element={<Executions />} />
          <Route path="executions/:id" element={<ExecutionDetail />} />
          <Route path="approvals" element={<Approvals />} />
          <Route path="sources" element={<Sources />} />
          <Route path="connections" element={<Connections />} />
          <Route path="admin" element={<Admin />} />
          <Route path="*" element={<Navigate to="/" replace />} />
        </Route>
      </Route>
    </Routes>
  );
}

function OutletShim() {
  return <Outlet />;
}
