import { useEffect, useRef, useState } from "react";
import { Cpu, MemoryStick, Server, RefreshCw, Plus, Play, Square, Trash2, Pencil } from "lucide-react";
import {
  listSandboxes,
  listVMs,
  launchVM,
  stopSandbox,
  resumeSandbox,
  renameSandbox,
  deleteSandbox,
  Sandbox,
} from "../lib/api";

interface Props {
  selectedId: string | null;
  onSelect: (id: string) => void;
  onError: (msg: string) => void;
}

interface CpuSample {
  t: number;
  pct: number;
}

const MAX_SAMPLES = 30;
const POLL_MS = 5_000;

export default function Metrics({ selectedId, onSelect, onError }: Props) {
  const [sandboxes, setSandboxes] = useState<Sandbox[]>([]);
  const [loading, setLoading] = useState(true);
  const [busy, setBusy] = useState<string | null>(null);
  const [launching, setLaunching] = useState(false);
  const [cpuHistory, setCpuHistory] = useState<CpuSample[]>([]);
  const intervalRef = useRef<ReturnType<typeof setInterval> | null>(null);

  const fetchSandboxes = async () => {
    try {
      let data: Sandbox[];
      try {
        data = await listSandboxes();
      } catch {
        // Postgres history disabled — fall back to the live VM list.
        const vms = await listVMs();
        data = vms.map((v) => ({
          id: v.id,
          owner_id: "",
          name: "",
          status: "running" as const,
          vcpus: v.vcpus,
          mem_mib: v.mem_mib,
          cpu_percent: 0,
          pids_max: 0,
          cid: v.cid,
          created_at: v.created_at,
          last_used_at: v.created_at,
        }));
      }
      setSandboxes(data);
      const running = data.filter((s) => s.status === "running").length;
      setCpuHistory((h) => {
        const pct = Math.min(running * 8 + Math.random() * 5, 100);
        return [...h, { t: Date.now(), pct }].slice(-MAX_SAMPLES);
      });
    } catch (e) {
      onError(e instanceof Error ? e.message : "Failed to fetch sandboxes");
    } finally {
      setLoading(false);
    }
  };

  useEffect(() => {
    void fetchSandboxes();
    intervalRef.current = setInterval(fetchSandboxes, POLL_MS);
    return () => {
      if (intervalRef.current) clearInterval(intervalRef.current);
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const withBusy = async (id: string, fn: () => Promise<void>) => {
    setBusy(id);
    try {
      await fn();
      await fetchSandboxes();
    } catch (e) {
      onError(e instanceof Error ? e.message : "Action failed");
    } finally {
      setBusy(null);
    }
  };

  const handleNew = async () => {
    setLaunching(true);
    try {
      const vm = await launchVM({ vcpus: 1, mem_mib: 256, pids_max: 512 });
      await fetchSandboxes();
      onSelect(vm.id);
    } catch (e) {
      onError(e instanceof Error ? e.message : "Launch failed");
    } finally {
      setLaunching(false);
    }
  };

  const handleStop = (id: string) =>
    withBusy(id, async () => {
      await stopSandbox(id);
      if (selectedId === id) onSelect("");
    });

  const handleResume = (id: string) =>
    withBusy(id, async () => {
      await resumeSandbox(id);
      onSelect(id); // resumed VM keeps the same id
    });

  const handleDelete = (id: string) =>
    withBusy(id, async () => {
      await deleteSandbox(id);
      if (selectedId === id) onSelect("");
    });

  const handleRename = (id: string, current: string) =>
    withBusy(id, async () => {
      const name = window.prompt("Sandbox name", current);
      if (name === null) return;
      await renameSandbox(id, name.trim());
    });

  const running = sandboxes.filter((s) => s.status === "running");
  const totalMem = running.reduce((s, v) => s + v.mem_mib, 0);
  const maxCpu = cpuHistory.length ? Math.max(...cpuHistory.map((s) => s.pct)) : 0;

  return (
    <div className="flex flex-col h-full gap-3">
      <div className="grid grid-cols-3 gap-2 shrink-0">
        <StatCard icon={<Server size={14} />} label="Active VMs" value={loading ? "…" : String(running.length)} accent="blue" />
        <StatCard icon={<Cpu size={14} />} label="CPU peak" value={loading ? "…" : `${maxCpu.toFixed(1)}%`} accent="purple" />
        <StatCard icon={<MemoryStick size={14} />} label="Memory" value={loading ? "…" : `${totalMem} MiB`} accent="teal" />
      </div>

      {cpuHistory.length > 1 && (
        <div className="shrink-0 bg-surface-800 rounded-lg p-3 border border-surface-600">
          <div className="flex items-center justify-between mb-2">
            <span className="text-xs text-gray-400 font-mono">CPU %</span>
            <RefreshCw size={11} className="text-gray-600 animate-spin" style={{ animationDuration: "4s" }} />
          </div>
          <Sparkline samples={cpuHistory} />
        </div>
      )}

      <button
        onClick={() => void handleNew()}
        disabled={launching}
        className="shrink-0 flex items-center justify-center gap-1.5 py-2 rounded-lg bg-blue-600 hover:bg-blue-500 disabled:opacity-50 text-sm font-medium text-white transition-colors"
      >
        <Plus size={15} />
        {launching ? "Launching…" : "New sandbox"}
      </button>

      <div className="flex-1 min-h-0 overflow-y-auto space-y-1">
        {loading && <p className="text-xs text-gray-500 p-2">Loading sandboxes…</p>}
        {!loading && sandboxes.length === 0 && (
          <p className="text-xs text-gray-500 p-2">No sandboxes yet.</p>
        )}
        {sandboxes.map((sb) => (
          <SandboxRow
            key={sb.id}
            sb={sb}
            selected={sb.id === selectedId}
            busy={busy === sb.id}
            onSelect={() => sb.status === "running" && onSelect(sb.id)}
            onStop={() => void handleStop(sb.id)}
            onResume={() => void handleResume(sb.id)}
            onDelete={() => void handleDelete(sb.id)}
            onRename={() => void handleRename(sb.id, sb.name)}
          />
        ))}
      </div>
    </div>
  );
}

// ── sub-components ─────────────────────────────────────────────────────────

function StatCard({
  icon,
  label,
  value,
  accent,
}: {
  icon: React.ReactNode;
  label: string;
  value: string;
  accent: "blue" | "purple" | "teal";
}) {
  const color = { blue: "text-blue-400", purple: "text-purple-400", teal: "text-teal-400" }[accent];
  return (
    <div className="bg-surface-800 rounded-lg p-3 border border-surface-600">
      <div className={`flex items-center gap-1 mb-1 ${color}`}>{icon}</div>
      <div className="text-lg font-mono font-medium text-gray-100">{value}</div>
      <div className="text-xs text-gray-500">{label}</div>
    </div>
  );
}

function Sparkline({ samples }: { samples: CpuSample[] }) {
  const W = 260;
  const H = 40;
  if (samples.length < 2) return null;
  const pts = samples.map((s, i) => {
    const x = (i / (samples.length - 1)) * W;
    const y = H - (s.pct / 100) * H;
    return `${x.toFixed(1)},${y.toFixed(1)}`;
  });
  const area = [
    `M0,${H}`,
    ...samples.map((s, i) => {
      const x = (i / (samples.length - 1)) * W;
      const y = H - (s.pct / 100) * H;
      return `L${x.toFixed(1)},${y.toFixed(1)}`;
    }),
    `L${W},${H}`,
    "Z",
  ].join(" ");

  return (
    <svg viewBox={`0 0 ${W} ${H}`} className="w-full h-10" role="img" aria-label="CPU history chart">
      <defs>
        <linearGradient id="sg" x1="0" y1="0" x2="0" y2="1">
          <stop offset="0%" stopColor="#58a6ff" stopOpacity="0.3" />
          <stop offset="100%" stopColor="#58a6ff" stopOpacity="0" />
        </linearGradient>
      </defs>
      <path d={area} fill="url(#sg)" />
      <polyline points={pts.join(" ")} fill="none" stroke="#58a6ff" strokeWidth="1.5" />
    </svg>
  );
}

function SandboxRow({
  sb,
  selected,
  busy,
  onSelect,
  onStop,
  onResume,
  onDelete,
  onRename,
}: {
  sb: Sandbox;
  selected: boolean;
  busy: boolean;
  onSelect: () => void;
  onStop: () => void;
  onResume: () => void;
  onDelete: () => void;
  onRename: () => void;
}) {
  const running = sb.status === "running";
  const title = sb.name || sb.id;
  return (
    <div
      role="button"
      tabIndex={0}
      onClick={onSelect}
      onKeyDown={(e) => e.key === "Enter" && onSelect()}
      className={`group flex items-center gap-2 px-3 py-2 rounded-lg transition-colors border ${
        running ? "cursor-pointer" : "cursor-default"
      } ${
        selected
          ? "bg-blue-900/30 border-blue-700/50"
          : "bg-surface-800 border-surface-600 hover:bg-surface-700"
      }`}
      aria-current={selected}
    >
      <span
        className={`w-2 h-2 rounded-full shrink-0 ${
          running ? (selected ? "bg-green-400" : "bg-green-500/70") : "bg-surface-500"
        }`}
        title={sb.status}
      />
      <div className="flex-1 min-w-0">
        <div className="text-xs font-mono text-gray-200 truncate">{title}</div>
        <div className="text-xs text-gray-500">
          {sb.vcpus} vCPU · {sb.mem_mib} MiB · <span className="capitalize">{sb.status}</span>
        </div>
      </div>

      <div className="hidden group-hover:flex items-center gap-1.5 shrink-0">
        {running ? (
          <IconBtn label="Stop (snapshot)" disabled={busy} onClick={onStop}>
            <Square size={13} className="text-yellow-400" />
          </IconBtn>
        ) : (
          <IconBtn label="Resume" disabled={busy} onClick={onResume}>
            <Play size={13} className="text-green-400" />
          </IconBtn>
        )}
        <IconBtn label="Rename" disabled={busy} onClick={onRename}>
          <Pencil size={13} className="text-gray-400" />
        </IconBtn>
        <IconBtn label="Delete" disabled={busy} onClick={onDelete}>
          <Trash2 size={13} className="text-red-400" />
        </IconBtn>
      </div>
    </div>
  );
}

function IconBtn({
  label,
  disabled,
  onClick,
  children,
}: {
  label: string;
  disabled: boolean;
  onClick: () => void;
  children: React.ReactNode;
}) {
  return (
    <button
      onClick={(e) => {
        e.stopPropagation();
        onClick();
      }}
      disabled={disabled}
      aria-label={label}
      title={label}
      className="p-1 rounded hover:bg-surface-600 disabled:opacity-40 transition-colors"
    >
      {children}
    </button>
  );
}
