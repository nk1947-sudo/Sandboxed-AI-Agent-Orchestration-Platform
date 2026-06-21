import { useCallback, useState } from "react";
import { LogOut, Shield } from "lucide-react";
import Metrics from "./components/Metrics";
import Terminal from "./components/Terminal";
import ApprovalWall from "./components/ApprovalWall";
import { ToastContainer } from "./components/Toast";
import { clearToken } from "./lib/auth";

interface Props {
  onLogout: () => void;
}

interface ToastItem { id: number; message: string }
let _toastSeq = 0;

export default function Dashboard({ onLogout }: Props) {
  const [selectedSandbox, setSelectedSandbox] = useState<string | null>(null);
  const [toasts, setToasts] = useState<ToastItem[]>([]);

  const pushError = useCallback((msg: string) => {
    const id = ++_toastSeq;
    setToasts((t) => [...t, { id, message: msg }]);
  }, []);

  const dismissToast = useCallback((id: number) => {
    setToasts((t) => t.filter((x) => x.id !== id));
  }, []);

  const handleLogout = () => {
    clearToken();
    onLogout();
  };

  const handleSelect = (id: string) => {
    setSelectedSandbox(id || null);
  };

  return (
    <div className="flex flex-col h-full bg-surface-900 text-gray-100">
      {/* Topbar */}
      <header className="flex items-center gap-3 px-4 py-2.5 bg-surface-800 border-b border-surface-600 shrink-0">
        <Shield size={18} className="text-blue-400" />
        <span className="font-semibold text-sm tracking-wide">
          Agent Sandbox Hypervisor
        </span>
        <span className="ml-2 h-2 w-2 rounded-full bg-green-400 shadow-[0_0_6px_#4ade80]" aria-label="online" />
        <div className="flex-1" />
        <button
          onClick={handleLogout}
          className="flex items-center gap-1.5 text-xs text-gray-400 hover:text-gray-200 transition-colors"
          aria-label="Log out"
        >
          <LogOut size={14} />
          Logout
        </button>
      </header>

      {/* Main 3-panel layout */}
      <div className="flex flex-1 min-h-0 gap-px bg-surface-600">
        {/* Left — Metrics + VM list */}
        <aside className="w-72 shrink-0 bg-surface-900 p-3 overflow-hidden">
          <Metrics
            selectedId={selectedSandbox}
            onSelect={handleSelect}
            onError={pushError}
          />
        </aside>

        {/* Centre — Terminal */}
        <main className="flex-1 min-w-0 bg-surface-900 p-3">
          <Terminal sandboxId={selectedSandbox} />
        </main>

        {/* Right — HITL approval wall */}
        <aside className="w-80 shrink-0 bg-surface-900 p-3 overflow-hidden">
          <ApprovalWall onError={pushError} />
        </aside>
      </div>

      {/* Toast notifications */}
      <ToastContainer toasts={toasts} onDismiss={dismissToast} />
    </div>
  );
}
