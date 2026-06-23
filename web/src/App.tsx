import { useEffect, useState } from "react";
import { Loader2 } from "lucide-react";
import LoginScreen from "./LoginScreen";
import Dashboard from "./Dashboard";
import { restoreSession } from "./lib/auth";

export default function App() {
  const [ready, setReady] = useState(false);
  const [authed, setAuthed] = useState(false);

  // On load (and after every refresh) ask the server if our session cookie is
  // still valid. This is what keeps us logged in across reloads.
  useEffect(() => {
    let alive = true;
    restoreSession().then((u) => {
      if (!alive) return;
      setAuthed(u !== null);
      setReady(true);
    });
    return () => {
      alive = false;
    };
  }, []);

  if (!ready) {
    return (
      <div className="flex items-center justify-center min-h-screen bg-surface-900 text-gray-400">
        <Loader2 size={20} className="animate-spin" />
      </div>
    );
  }
  if (!authed) {
    return <LoginScreen onLogin={() => setAuthed(true)} />;
  }
  return <Dashboard onLogout={() => setAuthed(false)} />;
}
