//go:build linux

// Command controlplane is the host control-plane daemon. It wires:
//
//   - Redis state store (sandbox records, heartbeats, sessions, rate limits)
//   - Orchestrator (Firecracker microVM lifecycle, cgroup v2, jailer)
//   - HITL gate (request classifier + Redis-backed approval queue)
//   - HTTP/WebSocket API gateway (REST + terminal proxy)
//
// All configuration is environment-driven (12-factor). See loadConfig for the
// full list of supported variables.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/yourorg/sandbox-platform/internal/gateway"
	"github.com/yourorg/sandbox-platform/internal/hitl"
	"github.com/yourorg/sandbox-platform/internal/orchestrator"
	"github.com/yourorg/sandbox-platform/internal/state"
)

type appConfig struct {
	orch orchestrator.Config

	redisAddr     string
	redisPassword string
	redisDB       int

	httpAddr              string
	heartbeatTTL          time.Duration
	healthInterval        time.Duration
	shutdownTerminatesAll bool

	// Phase 4 API gateway.
	apiToken       string
	allowedOrigins []string // parsed from ALLOWED_ORIGINS (comma-separated)
	hitlTTL        time.Duration
	vsockRetryMax  int
}

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)

	cfg := loadConfig()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	// One shared Redis client for the state store, HITL gate, and rate limiter.
	rdb := redis.NewClient(&redis.Options{
		Addr:     cfg.redisAddr,
		Password: cfg.redisPassword,
		DB:       cfg.redisDB,
	})
	defer rdb.Close()
	if err := rdb.Ping(ctx).Err(); err != nil {
		log.Error("redis unreachable", "addr", cfg.redisAddr, "err", err)
		os.Exit(1)
	}

	store := state.NewWithClient(rdb)
	gate := hitl.New(rdb, hitl.Config{ApproveTTL: cfg.hitlTTL})

	sup, err := orchestrator.NewSupervisor(cfg.orch,
		orchestrator.WithLogger(log),
		orchestrator.WithRecorder(store),
	)
	if err != nil {
		log.Error("init supervisor", "err", err)
		os.Exit(1)
	}

	// Reclaim orphaned state (cgroups, chroots, CIDs) from a previous run.
	if err := sup.Reconcile(ctx); err != nil {
		log.Error("reconcile", "err", err)
		os.Exit(1)
	}

	go healthLoop(ctx, log, sup, store, cfg.heartbeatTTL, cfg.healthInterval)

	apiHandler := gateway.NewHandler(sup, store, gate, log, gateway.HandlerConfig{
		Token:       cfg.apiToken,
		Origins:     cfg.allowedOrigins,
		VsockConfig: gateway.Config{RetryMax: cfg.vsockRetryMax},
	})

	srv := &http.Server{
		Addr:              cfg.httpAddr,
		Handler:           buildMux(sup, apiHandler),
		ReadHeaderTimeout: 5 * time.Second,
		// WebSocket connections can be long-lived; no global read/write timeout.
	}
	go func() {
		log.Info("server listening", "addr", cfg.httpAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("http server", "err", err)
			stop()
		}
	}()

	<-ctx.Done()
	log.Info("shutdown signal received")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)

	if cfg.shutdownTerminatesAll {
		for _, inst := range sup.List() {
			if err := sup.Terminate(inst.ID); err != nil {
				log.Warn("terminate on shutdown", "id", inst.ID, "err", err)
			}
		}
	}
	log.Info("controlplane stopped", "remaining", sup.Count())
}

// healthLoop reaps dead sandboxes and refreshes heartbeats on a fixed interval.
func healthLoop(ctx context.Context, log *slog.Logger, sup *orchestrator.Supervisor, store *state.Store, ttl, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			for _, id := range sup.ReapDead() {
				log.Warn("reaped dead sandbox", "id", id)
			}
			for _, inst := range sup.List() {
				if err := store.Heartbeat(ctx, inst.ID, ttl); err != nil {
					log.Warn("heartbeat", "id", inst.ID, "err", err)
				}
			}
		}
	}
}

// buildMux assembles the full HTTP routing table.
func buildMux(sup *orchestrator.Supervisor, api *gateway.Handler) http.Handler {
	mux := http.NewServeMux()

	// Health endpoints (Phase 2).
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"active":` + strconv.Itoa(sup.Count()) + "}\n"))
	})

	// Phase 4: REST + WebSocket.
	api.Register(mux)

	return mux
}

func loadConfig() appConfig {
	rawOrigins := envStr("ALLOWED_ORIGINS", "")
	var origins []string
	for _, o := range strings.Split(rawOrigins, ",") {
		if s := strings.TrimSpace(o); s != "" {
			origins = append(origins, s)
		}
	}

	return appConfig{
		orch: orchestrator.Config{
			FirecrackerBin:  envStr("FC_BIN", "firecracker"),
			JailerBin:       envStr("JAILER_BIN", "jailer"),
			KernelImagePath: envStr("KERNEL_IMAGE", "/var/lib/sandbox/vmlinux-6.1"),
			RootfsPath:      envStr("ROOTFS_IMAGE", "/var/lib/sandbox/rootfs.ext4"),
			UseJailer:       envBool("USE_JAILER", true),
			ChrootBaseDir:   envStr("CHROOT_BASE", "/srv/jailer"),
			JailerUID:       envInt("JAILER_UID", 10001),
			JailerGID:       envInt("JAILER_GID", 10001),
			StateDir:        envStr("STATE_DIR", "/srv/sandbox-state"),
			CgroupRoot:      envStr("CGROUP_ROOT", "/sys/fs/cgroup"),
			CgroupBase:      envStr("CGROUP_BASE", "sandboxes"),
			MaxConcurrent:   envInt("MAX_CONCURRENT", 0),
			MemoryBudgetMiB: int64(envInt("MEMORY_BUDGET_MIB", 0)),
		},
		redisAddr:             envStr("REDIS_ADDR", "127.0.0.1:6379"),
		redisPassword:         envStr("REDIS_PASSWORD", ""),
		redisDB:               envInt("REDIS_DB", 0),
		httpAddr:              envStr("HTTP_ADDR", ":8080"),
		heartbeatTTL:          time.Duration(envInt("HEARTBEAT_TTL_SEC", 15)) * time.Second,
		healthInterval:        time.Duration(envInt("HEALTH_INTERVAL_SEC", 5)) * time.Second,
		shutdownTerminatesAll: envBool("SHUTDOWN_TERMINATES_ALL", true),
		apiToken:              envStr("API_TOKEN", ""),
		allowedOrigins:        origins,
		hitlTTL:               time.Duration(envInt("HITL_TTL_SEC", 3600)) * time.Second,
		vsockRetryMax:         envInt("VSOCK_RETRY_MAX", 10),
	}
}

func envStr(key, def string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v, ok := os.LookupEnv(key); ok {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envBool(key string, def bool) bool {
	if v, ok := os.LookupEnv(key); ok {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return def
}
