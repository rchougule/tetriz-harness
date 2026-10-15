import { Navigate, Route, Routes } from "react-router";
import { useSession } from "./api/context";
import { Layout } from "./components/Layout";
import { ArtifactComposerPage } from "./pages/ArtifactComposer";
import { ArtifactDetailPage } from "./pages/ArtifactDetail";
import { ArtifactsPage } from "./pages/ArtifactsPage";
import { SettingsPage } from "./pages/SettingsPage";
import { SignIn } from "./pages/SignIn";

export function AppRoutes() {
  const { token } = useSession();
  if (!token) return <SignIn />;
  return (
    <Routes>
      <Route element={<Layout />}>
        <Route index element={<Navigate to="/artifacts" replace />} />
        <Route path="artifacts" element={<ArtifactsPage />} />
        <Route path="artifacts/new" element={<ArtifactComposerPage />} />
        <Route path="artifacts/:id" element={<ArtifactDetailPage />} />
        <Route path="artifacts/:id/edit" element={<ArtifactComposerPage />} />
        <Route path="settings" element={<SettingsPage />} />
        <Route path="*" element={<Navigate to="/artifacts" replace />} />
      </Route>
    </Routes>
  );
}
