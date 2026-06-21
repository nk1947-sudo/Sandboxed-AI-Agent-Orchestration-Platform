import { FormEvent, useState } from "react";
import { Shield, Eye, EyeOff, Loader2 } from "lucide-react";
import { setToken } from "./lib/auth";
import { listVMs, ApiError } from "./lib/api";

interface Props {
  onLogin: () => void;
}

export default function LoginScreen({ onLogin }: Props) {
  const [token, setTokenVal] = useState("");
  const [show, setShow] = useState(false);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState("");

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    if (!token.trim()) { setError("Enter an API token."); return; }
    setError("");
    setLoading(true);
    try {
      setToken(token.trim());
      // Verify credentials by pinging /api/vms.
      await listVMs();
      onLogin();
    } catch (err) {
      setToken(""); // clear in-memory on failure
      if (err instanceof ApiError && err.status === 401) {
        setError("Invalid token — access denied.");
      } else {
        setError("Cannot reach gateway. Is it running?");
      }
    } finally {
      setLoading(false);
    }
  };

  return (
    <div className="flex items-center justify-center min-h-screen bg-surface-900 px-4">
      <div className="w-full max-w-sm space-y-6">
        {/* Logo */}
        <div className="flex flex-col items-center gap-3">
          <div className="p-3 rounded-2xl bg-blue-600/20 border border-blue-500/30">
            <Shield size={32} className="text-blue-400" />
          </div>
          <div className="text-center">
            <h1 className="text-lg font-semibold text-gray-100">Sandbox Hypervisor</h1>
            <p className="text-sm text-gray-500 mt-0.5">Enter your operator API token</p>
          </div>
        </div>

        {/* Form */}
        <form onSubmit={(e) => void submit(e)} className="space-y-3">
          <div className="relative">
            <input
              type={show ? "text" : "password"}
              value={token}
              onChange={(e) => setTokenVal(e.target.value)}
              placeholder="API token"
              autoFocus
              autoComplete="current-password"
              className="w-full bg-surface-800 border border-surface-600 rounded-lg px-3 py-2.5 pr-10 text-sm text-gray-100 placeholder-gray-600 outline-none focus:border-blue-500 transition-colors"
              aria-label="API token"
            />
            <button
              type="button"
              onClick={() => setShow((s) => !s)}
              className="absolute right-2.5 top-1/2 -translate-y-1/2 text-gray-500 hover:text-gray-300"
              aria-label={show ? "Hide token" : "Show token"}
            >
              {show ? <EyeOff size={15} /> : <Eye size={15} />}
            </button>
          </div>

          {error && (
            <p role="alert" className="text-xs text-red-400">
              {error}
            </p>
          )}

          <button
            type="submit"
            disabled={loading}
            className="w-full flex items-center justify-center gap-2 py-2.5 rounded-lg bg-blue-600 hover:bg-blue-500 disabled:opacity-50 disabled:cursor-not-allowed text-sm font-medium text-white transition-colors"
          >
            {loading && <Loader2 size={15} className="animate-spin" />}
            {loading ? "Verifying…" : "Sign in"}
          </button>
        </form>

        <p className="text-xs text-center text-gray-600">
          Dev mode: leave token blank if{" "}
          <code className="font-mono">API_TOKEN</code> is unset.
        </p>
      </div>
    </div>
  );
}
