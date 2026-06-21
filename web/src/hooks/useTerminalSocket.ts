import { useCallback, useEffect, useRef, useState } from "react";
import { terminalWsUrl } from "../lib/api";

export type ConnState = "connecting" | "open" | "closed" | "error";

export interface WsFrame {
  kind:
    | "output"
    | "exit"
    | "error"
    | "pending"
    | "approved"
    | "rejected";
  data?: string;
  exit_code?: number;
  approval_id?: string;
  reason?: string;
  message?: string;
}

export interface UseTerminalSocket {
  connState: ConnState;
  send: (script: string, timeoutSec?: number) => void;
  lastFrame: WsFrame | null;
}

const BACKOFF_BASE = 1_000;
const BACKOFF_CAP = 30_000;

export function useTerminalSocket(
  sandboxId: string | null,
  onFrame: (f: WsFrame) => void
): UseTerminalSocket {
  const wsRef = useRef<WebSocket | null>(null);
  const retryRef = useRef(0);
  const unmountedRef = useRef(false);
  const [connState, setConnState] = useState<ConnState>("closed");
  const [lastFrame, setLastFrame] = useState<WsFrame | null>(null);
  const onFrameRef = useRef(onFrame);
  onFrameRef.current = onFrame;

  const connect = useCallback(() => {
    if (!sandboxId) return;
    setConnState("connecting");
    const ws = new WebSocket(terminalWsUrl(sandboxId));
    wsRef.current = ws;

    ws.onopen = () => {
      if (unmountedRef.current) { ws.close(); return; }
      retryRef.current = 0;
      setConnState("open");
    };

    ws.onmessage = (ev) => {
      let frame: WsFrame;
      try {
        frame = JSON.parse(ev.data as string) as WsFrame;
      } catch {
        return;
      }
      setLastFrame(frame);
      onFrameRef.current(frame);
    };

    ws.onerror = () => {
      setConnState("error");
    };

    ws.onclose = () => {
      if (unmountedRef.current) return;
      setConnState("closed");
      // Exponential backoff reconnect.
      const delay = Math.min(
        BACKOFF_BASE * 2 ** retryRef.current,
        BACKOFF_CAP
      );
      retryRef.current += 1;
      setTimeout(connect, delay);
    };
  }, [sandboxId]);

  useEffect(() => {
    unmountedRef.current = false;
    if (sandboxId) connect();
    return () => {
      unmountedRef.current = true;
      wsRef.current?.close();
    };
  }, [sandboxId, connect]);

  const send = useCallback(
    (script: string, timeoutSec = 30) => {
      if (wsRef.current?.readyState === WebSocket.OPEN) {
        wsRef.current.send(
          JSON.stringify({ script, timeout_sec: timeoutSec })
        );
      }
    },
    []
  );

  return { connState, send, lastFrame };
}
