import { useEffect, useRef, useState } from "react";
import { AlertTriangle, CheckCircle, XCircle, Clock, RefreshCw } from "lucide-react";
import { ApprovalRecord, listApprovals, approveScript, rejectScript } from "../lib/api";

interface Props {
  onError: (msg: string) => void;
}

const POLL_MS = 3_000;

export default function ApprovalWall({ onError }: Props) {
  const [records, setRecords] = useState<ApprovalRecord[]>([]);
  const [loading, setLoading] = useState(true);
  const [acting, setActing] = useState<Record<string, "approving" | "rejecting">>({});
  const intervalRef = useRef<ReturnType<typeof setInterval> | null>(null);

  const fetchPending = async () => {
    try {
      const data = await listApprovals();
      setRecords(data);
    } catch (e) {
      onError(e instanceof Error ? e.message : "Failed to fetch approvals");
    } finally {
      setLoading(false);
    }
  };

  useEffect(() => {
    void fetchPending();
    intervalRef.current = setInterval(fetchPending, POLL_MS);
    return () => {
      if (intervalRef.current) clearInterval(intervalRef.current);
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const decide = async (id: string, action: "approve" | "reject") => {
    setActing((a) => ({ ...a, [id]: action === "approve" ? "approving" : "rejecting" }));
    try {
      if (action === "approve") await approveScript(id);
      else await rejectScript(id);
      // Optimistic remove — server will confirm on next poll.
      setRecords((r) => r.filter((rec) => rec.id !== id));
    } catch (e) {
      onError(e instanceof Error ? e.message : "Action failed");
      // Refetch so stale state doesn't linger.
      void fetchPending();
    } finally {
      setActing((a) => {
        const next = { ...a };
        delete next[id];
        return next;
      });
    }
  };

  return (
    <div className="flex flex-col h-full">
      <div className="flex items-center justify-between mb-3 shrink-0">
        <h2 className="text-sm font-semibold text-gray-200 flex items-center gap-2">
          <AlertTriangle size={14} className="text-yellow-400" />
          HITL Approval Queue
        </h2>
        <span className="flex items-center gap-1 text-xs text-gray-500">
          <RefreshCw size={11} className="animate-spin" style={{ animationDuration: "3s" }} />
          live
        </span>
      </div>

      {loading && (
        <p className="text-xs text-gray-500">Checking queue…</p>
      )}

      {!loading && records.length === 0 && (
        <div className="flex flex-col items-center justify-center flex-1 text-gray-600 gap-2">
          <CheckCircle size={24} />
          <span className="text-sm">No pending approvals</span>
        </div>
      )}

      <div className="flex-1 min-h-0 overflow-y-auto space-y-3">
        {records.map((rec) => (
          <ApprovalCard
            key={rec.id}
            record={rec}
            acting={acting[rec.id]}
            onApprove={() => void decide(rec.id, "approve")}
            onReject={() => void decide(rec.id, "reject")}
          />
        ))}
      </div>
    </div>
  );
}

// ── ApprovalCard ────────────────────────────────────────────────────────────

function ApprovalCard({
  record,
  acting,
  onApprove,
  onReject,
}: {
  record: ApprovalRecord;
  acting?: "approving" | "rejecting";
  onApprove: () => void;
  onReject: () => void;
}) {
  const age = formatAge(record.submitted_at);

  return (
    <div className="bg-surface-800 border border-yellow-700/40 rounded-lg p-3 space-y-2">
      {/* Meta row */}
      <div className="flex items-center gap-2 text-xs text-gray-400">
        <span className="font-mono text-gray-300 truncate flex-1">
          {record.sandbox_id}
        </span>
        <span className="flex items-center gap-1 shrink-0">
          <Clock size={11} />
          {age}
        </span>
      </div>

      {/* Reason badge */}
      <div className="text-xs text-yellow-300 bg-yellow-900/30 rounded px-2 py-0.5 w-fit">
        {record.reason}
      </div>

      {/* Script — rendered as text/code, never innerHTML */}
      <pre className="text-xs font-mono text-gray-300 bg-surface-900 rounded p-2 max-h-28 overflow-y-auto whitespace-pre-wrap break-all">
        {record.script}
      </pre>

      {/* Actions */}
      <div className="flex gap-2">
        <button
          onClick={onApprove}
          disabled={!!acting}
          className="flex items-center gap-1.5 flex-1 justify-center text-xs py-1.5 rounded bg-green-700 hover:bg-green-600 disabled:opacity-40 disabled:cursor-not-allowed text-white font-medium transition-colors"
          aria-label={`Approve script for ${record.sandbox_id}`}
        >
          <CheckCircle size={13} />
          {acting === "approving" ? "Approving…" : "Approve"}
        </button>
        <button
          onClick={onReject}
          disabled={!!acting}
          className="flex items-center gap-1.5 flex-1 justify-center text-xs py-1.5 rounded bg-red-700 hover:bg-red-600 disabled:opacity-40 disabled:cursor-not-allowed text-white font-medium transition-colors"
          aria-label={`Reject script for ${record.sandbox_id}`}
        >
          <XCircle size={13} />
          {acting === "rejecting" ? "Rejecting…" : "Reject"}
        </button>
      </div>
    </div>
  );
}

function formatAge(iso: string): string {
  const secs = Math.floor((Date.now() - new Date(iso).getTime()) / 1000);
  if (secs < 60) return `${secs}s ago`;
  if (secs < 3600) return `${Math.floor(secs / 60)}m ago`;
  return `${Math.floor(secs / 3600)}h ago`;
}
