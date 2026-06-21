import {
  useEffect,
  useRef,
  useCallback,
  KeyboardEvent,
  useState,
} from "react";
import { Terminal as XTerm } from "@xterm/xterm";
import { FitAddon } from "@xterm/addon-fit";
import "@xterm/xterm/css/xterm.css";
import { useTerminalSocket, WsFrame } from "../hooks/useTerminalSocket";
import { Loader2, Wifi, WifiOff, AlertCircle } from "lucide-react";

interface Props {
  sandboxId: string | null;
}

const THEME = {
  background: "#0d1117",
  foreground: "#e6edf3",
  cursor: "#58a6ff",
  selectionBackground: "#388bfd33",
  black: "#484f58",
  red: "#ff7b72",
  green: "#3fb950",
  yellow: "#d29922",
  blue: "#58a6ff",
  magenta: "#bc8cff",
  cyan: "#39c5cf",
  white: "#b1bac4",
  brightBlack: "#6e7681",
  brightRed: "#ffa198",
  brightGreen: "#56d364",
  brightYellow: "#e3b341",
  brightBlue: "#79c0ff",
  brightMagenta: "#d2a8ff",
  brightCyan: "#56d4dd",
  brightWhite: "#f0f6fc",
};

export default function Terminal({ sandboxId }: Props) {
  const containerRef = useRef<HTMLDivElement>(null);
  const xtermRef = useRef<XTerm | null>(null);
  const fitRef = useRef<FitAddon | null>(null);
  const [cmd, setCmd] = useState("");
  const [pendingApproval, setPendingApproval] = useState<{
    id: string;
    reason: string;
  } | null>(null);

  // Initialise xterm once.
  useEffect(() => {
    if (!containerRef.current) return;
    const term = new XTerm({
      theme: THEME,
      fontFamily: '"JetBrains Mono", ui-monospace, monospace',
      fontSize: 13,
      lineHeight: 1.4,
      cursorBlink: true,
      scrollback: 5000,
      convertEol: true,
    });
    const fit = new FitAddon();
    term.loadAddon(fit);
    term.open(containerRef.current);
    fit.fit();
    xtermRef.current = term;
    fitRef.current = fit;

    if (sandboxId) {
      term.writeln(`\x1b[1;34mConnecting to sandbox \x1b[1;37m${sandboxId}\x1b[0m…`);
    } else {
      term.writeln("\x1b[2mSelect a sandbox to start a session.\x1b[0m");
    }

    return () => {
      term.dispose();
      xtermRef.current = null;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  // Resize observer.
  useEffect(() => {
    const el = containerRef.current;
    if (!el) return;
    const ro = new ResizeObserver(() => {
      fitRef.current?.fit();
    });
    ro.observe(el);
    return () => ro.disconnect();
  }, []);

  const handleFrame = useCallback((f: WsFrame) => {
    const term = xtermRef.current;
    if (!term) return;
    switch (f.kind) {
      case "output":
        term.write(f.data ?? "");
        break;
      case "exit":
        if ((f.exit_code ?? 0) === 0) {
          term.writeln("\r\n\x1b[2m[exit 0]\x1b[0m");
        } else {
          term.writeln(`\r\n\x1b[31m[exit ${f.exit_code}]\x1b[0m`);
        }
        break;
      case "error":
        term.writeln(`\r\n\x1b[31m[error] ${f.message ?? ""}\x1b[0m`);
        break;
      case "pending":
        setPendingApproval({ id: f.approval_id ?? "", reason: f.reason ?? "" });
        term.writeln(
          `\r\n\x1b[33m[HITL] Script held for approval: ${f.reason ?? ""}\x1b[0m`
        );
        break;
      case "approved":
        setPendingApproval(null);
        term.writeln("\x1b[32m[HITL] Approved — executing…\x1b[0m\r\n");
        break;
      case "rejected":
        setPendingApproval(null);
        term.writeln("\x1b[31m[HITL] Rejected — command will not run.\x1b[0m\r\n");
        break;
    }
  }, []);

  const { connState, send } = useTerminalSocket(sandboxId, handleFrame);

  // Print connection status changes to the terminal.
  const prevState = useRef(connState);
  useEffect(() => {
    const term = xtermRef.current;
    if (!term || prevState.current === connState) return;
    prevState.current = connState;
    if (connState === "open") {
      term.writeln("\x1b[32m[connected]\x1b[0m\r\n");
    } else if (connState === "closed" || connState === "error") {
      term.writeln("\x1b[33m[reconnecting…]\x1b[0m");
    }
  }, [connState]);

  const submitCmd = useCallback(() => {
    const script = cmd.trim();
    if (!script || connState !== "open") return;
    xtermRef.current?.writeln(`\x1b[1;32m$ \x1b[0m${script}`);
    send(script);
    setCmd("");
  }, [cmd, connState, send]);

  const onKeyDown = useCallback(
    (e: KeyboardEvent<HTMLInputElement>) => {
      if (e.key === "Enter") submitCmd();
    },
    [submitCmd]
  );

  const stateIcon =
    connState === "open" ? (
      <Wifi size={14} className="text-green-400" />
    ) : connState === "connecting" ? (
      <Loader2 size={14} className="text-yellow-400 animate-spin" />
    ) : (
      <WifiOff size={14} className="text-red-400" />
    );

  return (
    <div className="flex flex-col h-full bg-surface-900 rounded-lg overflow-hidden border border-surface-600">
      {/* Header */}
      <div className="flex items-center gap-2 px-3 py-2 bg-surface-800 border-b border-surface-600 shrink-0">
        <span className="text-xs text-gray-400 font-mono">
          {sandboxId ? `terminal · ${sandboxId}` : "terminal · no sandbox"}
        </span>
        <span className="ml-auto flex items-center gap-1 text-xs text-gray-500">
          {stateIcon}
          {connState}
        </span>
      </div>

      {/* HITL notice */}
      {pendingApproval && (
        <div className="flex items-center gap-2 px-3 py-2 bg-yellow-900/40 border-b border-yellow-700/50 text-yellow-300 text-xs shrink-0">
          <AlertCircle size={14} />
          <span>
            Awaiting approval for:{" "}
            <span className="font-medium">{pendingApproval.reason}</span>
          </span>
        </div>
      )}

      {/* xterm */}
      <div ref={containerRef} className="xterm-container flex-1 min-h-0" />

      {/* Input bar */}
      <div className="flex items-center gap-2 px-3 py-2 bg-surface-800 border-t border-surface-600 shrink-0">
        <span className="text-green-400 font-mono text-sm select-none">$</span>
        <input
          type="text"
          value={cmd}
          onChange={(e) => setCmd(e.target.value)}
          onKeyDown={onKeyDown}
          disabled={connState !== "open" || !sandboxId}
          placeholder={
            !sandboxId
              ? "Select a sandbox first"
              : connState !== "open"
              ? "Connecting…"
              : "Enter command…"
          }
          className="flex-1 bg-transparent text-sm font-mono text-gray-200 placeholder-gray-600 outline-none disabled:opacity-40"
          aria-label="Command input"
          autoComplete="off"
          spellCheck={false}
        />
        <button
          onClick={submitCmd}
          disabled={connState !== "open" || !cmd.trim() || !sandboxId}
          className="text-xs px-3 py-1 rounded bg-blue-600 hover:bg-blue-500 disabled:opacity-30 disabled:cursor-not-allowed text-white font-medium transition-colors"
        >
          Run
        </button>
      </div>
    </div>
  );
}
