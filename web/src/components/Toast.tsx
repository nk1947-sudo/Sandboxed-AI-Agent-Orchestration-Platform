import { useEffect, useState } from "react";
import { X, AlertCircle } from "lucide-react";

interface ToastProps {
  message: string;
  onDismiss: () => void;
  durationMs?: number;
}

export function Toast({ message, onDismiss, durationMs = 5000 }: ToastProps) {
  const [visible, setVisible] = useState(true);

  useEffect(() => {
    const t = setTimeout(() => {
      setVisible(false);
      setTimeout(onDismiss, 300);
    }, durationMs);
    return () => clearTimeout(t);
  }, [durationMs, onDismiss]);

  return (
    <div
      role="alert"
      className={`flex items-center gap-2 px-4 py-2.5 rounded-lg bg-red-900/80 border border-red-700/60 text-red-200 text-sm shadow-lg transition-all duration-300 ${
        visible ? "opacity-100 translate-y-0" : "opacity-0 translate-y-2"
      }`}
    >
      <AlertCircle size={14} className="shrink-0" />
      <span className="flex-1">{message}</span>
      <button
        onClick={() => { setVisible(false); setTimeout(onDismiss, 300); }}
        className="shrink-0 text-red-400 hover:text-red-200"
        aria-label="Dismiss"
      >
        <X size={14} />
      </button>
    </div>
  );
}

// ── Toast container ─────────────────────────────────────────────────────────

interface ToastItem { id: number; message: string }

interface ToastContainerProps {
  toasts: ToastItem[];
  onDismiss: (id: number) => void;
}

export function ToastContainer({ toasts, onDismiss }: ToastContainerProps) {
  if (!toasts.length) return null;
  return (
    <div className="fixed bottom-4 right-4 z-50 flex flex-col gap-2 max-w-sm">
      {toasts.map((t) => (
        <Toast key={t.id} message={t.message} onDismiss={() => onDismiss(t.id)} />
      ))}
    </div>
  );
}
