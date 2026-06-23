-- 0001_init.sql — base schema for the enterprise data layer.
-- gen_random_uuid() is built into PostgreSQL 13+ (no pgcrypto extension needed).

CREATE TABLE IF NOT EXISTS users (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    username      text UNIQUE NOT NULL,
    password_hash text NOT NULL,
    role          text NOT NULL DEFAULT 'operator',
    disabled      boolean NOT NULL DEFAULT false,
    created_at    timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS sessions (
    id           uuid PRIMARY KEY,
    user_id      uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at   timestamptz NOT NULL DEFAULT now(),
    expires_at   timestamptz NOT NULL,
    last_seen_at timestamptz NOT NULL DEFAULT now(),
    user_agent   text NOT NULL DEFAULT '',
    ip           inet
);
CREATE INDEX IF NOT EXISTS idx_sessions_user    ON sessions(user_id);
CREATE INDEX IF NOT EXISTS idx_sessions_expires ON sessions(expires_at);

CREATE TABLE IF NOT EXISTS sandboxes (
    id           text PRIMARY KEY,
    owner_id     uuid REFERENCES users(id) ON DELETE SET NULL,
    name         text NOT NULL DEFAULT '',
    status       text NOT NULL DEFAULT 'running',
    vcpus        integer NOT NULL,
    mem_mib      integer NOT NULL,
    cpu_percent  integer NOT NULL DEFAULT 0,
    pids_max     integer NOT NULL DEFAULT 0,
    cid          bigint,
    created_at   timestamptz NOT NULL DEFAULT now(),
    stopped_at   timestamptz,
    last_used_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_sandboxes_owner  ON sandboxes(owner_id);
CREATE INDEX IF NOT EXISTS idx_sandboxes_status ON sandboxes(status);

CREATE TABLE IF NOT EXISTS snapshots (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    sandbox_id      text NOT NULL REFERENCES sandboxes(id) ON DELETE CASCADE,
    mem_file_path   text NOT NULL,
    state_file_path text NOT NULL,
    disk_file_path  text NOT NULL,
    size_bytes      bigint NOT NULL DEFAULT 0,
    guest_vcpus     integer NOT NULL,
    guest_mem_mib   integer NOT NULL,
    label           text NOT NULL DEFAULT '',
    created_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_snapshots_sandbox ON snapshots(sandbox_id, created_at DESC);

CREATE TABLE IF NOT EXISTS transcript_lines (
    id         bigserial PRIMARY KEY,
    sandbox_id text NOT NULL REFERENCES sandboxes(id) ON DELETE CASCADE,
    seq        bigint NOT NULL,
    ts         timestamptz NOT NULL DEFAULT now(),
    kind       text NOT NULL,
    data       text NOT NULL DEFAULT '',
    exit_code  integer
);
CREATE INDEX IF NOT EXISTS idx_transcript_sandbox_seq ON transcript_lines(sandbox_id, seq);

CREATE TABLE IF NOT EXISTS audit_log (
    id      bigserial PRIMARY KEY,
    user_id uuid,
    action  text NOT NULL,
    target  text NOT NULL DEFAULT '',
    detail  jsonb,
    ts      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_audit_ts   ON audit_log(ts DESC);
CREATE INDEX IF NOT EXISTS idx_audit_user ON audit_log(user_id);
