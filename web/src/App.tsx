import { useState } from "react";
import LoginScreen from "./LoginScreen";
import Dashboard from "./Dashboard";
import { isAuthenticated } from "./lib/auth";

export default function App() {
  const [authed, setAuthed] = useState(isAuthenticated);

  if (!authed) {
    return <LoginScreen onLogin={() => setAuthed(true)} />;
  }
  return <Dashboard onLogout={() => setAuthed(false)} />;
}
