import { useEffect, useRef, useState } from "react";
import { Cpu, MemoryStick, Server, RefreshCw } from "lucide-react";
import { listVMs, VM, terminateVM } from "../lib/api";

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
  const [vms, setVms] = useState<VM[]>([]);
  const [loading, setLoading] = useState(true);
  const [terminating, setTerminating] = useState<string | null>(null);
  const [cpuHistory, setCpuHistory] = useState<CpuSample[]>([]);
  const intervalRef = useRef<ReturnType<typeof setInterval> | null>(null);

  const fetchVMs = async () => {
    try {
      const data = await listVMs();
      setVms(data);
      // Fake CPU sample from VM count (real host metrics need a metrics endpoint).
      setCpuHistory((h) => {
        const pct = Math.min(data.length * 8 + Math.random() * 5, 100);
        const next = [...h, { t: Date.now(), pct }];
        return next.slice(-MAX_SAMPLES);
      });
    } catch (e) {
      onError(e instanceof Error ? e.message : "Failed to fetch VMs");
    } finally {
      setLoading(false);
    }
  };

  useEffect(() => {
    void fetchVMs();
    intervalRef.current = setInterval(fetchVMs, POLL_MS);
    return () => {
      if (intervalRef.current) clearInterval(intervalRef.current);
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const handleTerminate = async (id: string) => {
    setTerminating(id);
    try {
      await terminateVM(id);
      setVms((v) => v.filter((vm) => vm.id !== id));
      if (selectedId === id) onSelect("");
    } catch (e) {
      onError(e instanceof Error ? e.message : "Terminate failed");
    } finally {
      setTerminating(null);
    }
  };

  const totalMem = vms.reduce((s, v) => s + v.mem_mib, 0);
  const maxCpu = cpuHistory.length
    ? Math.max(...cpuHistory.map((s) => s.pct))
    : 0;

  return (
    <div className="flex flex-col h-full gap-3">
      {/* Summary cards */}
      <div className="grid grid-cols-3 gap-2 shrink-0">
        <StatCard
          icon={<Server size={14} />}
          label="Active VMs"
          value={loading ? "…" : String(vms.length)}
          accent="blue"
        />
        <StatCard
          icon={<Cpu size={14} />}
          label="CPU peak"
          value={loading ? "…" : `${maxCpu.toFixed(1)}%`}
          accent="purple"
        />
        <StatCard
          icon={<MemoryStick size={14} />}
          label="Memory"
          value={loading ? "…" : `${totalMem} MiB`}
          accent="teal"
        />
      </div>

      {/* CPU sparkline */}
      {cpuHistory.length > 1 && (
        <div className="shrink-0 bg-surface-800 rounded-lg p-3 border border-surface-600">
          <div className="flex items-center justify-between mb-2">
            <span className="text-xs text-gray-400 font-mono">CPU %</span>
            <RefreshCw
              size={11}
              className="text-gray-600 animate-spin"
              style={{ animationDuration: "4s" }}
            />
          </div>
          <Sparkline samples={cpuHistory} />
        </div>
      )}

      {/* VM list */}
      <div className="flex-1 min-h-0 overflow-y-auto space-y-1">
        {loading && (
          <p className="text-xs text-gray-500 p-2">Loading sandboxes…</p>
        )}
        {!loading && vms.length === 0 && (
          <p className="text-xs text-gray-500 p-2">No active sandboxes.</p>
        )}
        {vms.map((vm) => (
          <VmRow
            key={vm.id}
            vm={vm}
            selected={vm.id === selectedId}
            terminating={terminating === vm.id}
            onSelect={() => onSelect(vm.id)}
            onTerminate={() => void handleTerminate(vm.id)}
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
  const color = {
    blue: "text-blue-400",
    purple: "text-purple-400",
    teal: "text-teal-400",
  }[accent];
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
      <polyline
        points={pts.join(" ")}
        fill="none"
        stroke="#58a6ff"
        strokeWidth="1.5"
      />
    </svg>
  );
}

function VmRow({
  vm,
  selected,
  terminating,
  onSelect,
  onTerminate,
}: {
  vm: VM;
  selected: boolean;
  terminating: boolean;
  onSelect: () => void;
  onTerminate: () => void;
}) {
  const uptime = formatUptime(vm.created_at);
  return (
    <div
      role="button"
      tabIndex={0}
      onClick={onSelect}
      onKeyDown={(e) => e.key === "Enter" && onSelect()}
      className={`group flex items-center gap-2 px-3 py-2 rounded-lg cursor-pointer transition-colors border ${
        selected
          ? "bg-blue-900/30 border-blue-700/50"
          : "bg-surface-800 border-surface-600 hover:bg-surface-700"
      }`}
      aria-current={selected}
    >
      <span
        className={`w-2 h-2 rounded-full shrink-0 ${
          selected ? "bg-green-400" : "bg-surface-600"
        }`}
      />
      <div className="flex-1 min-w-0">
        <div className="text-xs font-mono text-gray-200 truncate">{vm.id}</div>
        <div className="text-xs text-gray-500">
          {vm.vcpus} vCPU · {vm.mem_mib} MiB · {uptime}
        </div>
      </div>
      <button
        onClick={(e) => {
          e.stopPropagation();
          onTerminate();
        }}
        disabled={terminating}
        className="hidden group-hover:flex items-center text-xs text-red-400 hover:text-red-300 disabled:opacity-40 transition-colors"
        aria-label={`Terminate ${vm.id}`}
      >
        {terminating ? "…" : "Kill"}
      </button>
    </div>
  );
}

function formatUptime(createdAt: string): string {
  const secs = Math.floor(
    (Date.now() - new Date(createdAt).getTime()) / 1000
  );
  if (secs < 60) return `${secs}s`;
  if (secs < 3600) return `${Math.floor(secs / 60)}m`;
  return `${Math.floor(secs / 3600)}h ${Math.floor((secs % 3600) / 60)}m`;
}
