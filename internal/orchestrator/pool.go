//go:build linux

// pool.go — Phase 7: pre-warmed snapshot pool.
//
// The pool keeps a fixed number of restored (or restorable) VM slots warm.
// When a caller requests a sandbox:
//   - If a warm slot is available, it is handed out immediately (<10 ms).
//   - Otherwise the caller waits for a fresh restore (typically <125 ms).
//
// After hand-out the pool asynchronously refills to its target depth.
// The pool is safe for concurrent use.
package orchestrator

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// PoolConfig tunes the snapshot pool.
type PoolConfig struct {
	// Snapshot is the base snapshot all pool slots are restored from.
	Snapshot Snapshot

	// Spec describes the resource caps for each warm instance.
	// Must match the snapshot's CPU/memory configuration.
	Spec LaunchSpec

	// Depth is the target number of warm (already restored) slots.
	// Minimum 1. Recommended 2–5 depending on burst pattern.
	Depth int

	// RefillTimeout is the max time to allow a refill restore to take.
	// Zero defaults to 10 seconds.
	RefillTimeout time.Duration

	// MaxWaitTime is how long Acquire blocks before returning an error.
	// Zero defaults to 30 seconds.
	MaxWaitTime time.Duration
}

func (c *PoolConfig) applyDefaults() {
	if c.Depth < 1 {
		c.Depth = 1
	}
	if c.RefillTimeout == 0 {
		c.RefillTimeout = 10 * time.Second
	}
	if c.MaxWaitTime == 0 {
		c.MaxWaitTime = 30 * time.Second
	}
}

// Pool manages a set of pre-restored sandbox instances ready for immediate use.
type Pool struct {
	cfg    PoolConfig
	sup    *Supervisor
	log    *slog.Logger
	mu     sync.Mutex
	warm   []*Instance // ready-to-hand-out instances
	refill chan struct{} // buffered: signals the refill goroutine
	done   chan struct{} // close to stop the refill loop
}

// NewPool creates a pool and starts the background refill goroutine.
// Call pool.Close() when shutting down.
func NewPool(sup *Supervisor, cfg PoolConfig, log *slog.Logger) (*Pool, error) {
	cfg.applyDefaults()
	if log == nil {
		log = slog.Default()
	}
	p := &Pool{
		cfg:    cfg,
		sup:    sup,
		log:    log,
		refill: make(chan struct{}, cfg.Depth),
		done:   make(chan struct{}),
	}
	// Kick off initial fill.
	for i := 0; i < cfg.Depth; i++ {
		p.triggerRefill()
	}
	go p.refillLoop()
	return p, nil
}

// Acquire returns a warm instance from the pool, restoring a new one if
// the pool is empty. The caller owns the returned instance and must call
// pool.Release or pool.Sup.Terminate when done.
func (p *Pool) Acquire(ctx context.Context) (*Instance, error) {
	deadline := time.Now().Add(p.cfg.MaxWaitTime)
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	// Fast path: grab a warm slot.
	p.mu.Lock()
	if len(p.warm) > 0 {
		inst := p.warm[len(p.warm)-1]
		p.warm = p.warm[:len(p.warm)-1]
		p.mu.Unlock()
		p.triggerRefill()
		p.log.Debug("pool: warm slot acquired", "id", inst.ID, "remaining", p.depth())
		return inst, nil
	}
	p.mu.Unlock()

	// Slow path: restore one right now.
	p.log.Info("pool: no warm slots — restoring immediately")
	return p.restore(ctx)
}

// Release returns a previously Acquire'd instance to the pool IF the pool is
// not already at depth. If the pool is full, the instance is terminated.
// Returns an error only if termination fails.
func (p *Pool) Release(inst *Instance) error {
	p.mu.Lock()
	if len(p.warm) < p.cfg.Depth {
		p.warm = append(p.warm, inst)
		p.mu.Unlock()
		p.log.Debug("pool: instance returned to pool", "id", inst.ID)
		return nil
	}
	p.mu.Unlock()
	// Pool is full; discard.
	p.log.Debug("pool: pool full, terminating released instance", "id", inst.ID)
	return p.sup.Terminate(inst.ID)
}

// Close drains the pool, terminates all warm instances, and stops the
// refill goroutine. Safe to call multiple times.
func (p *Pool) Close() {
	close(p.done)
	p.mu.Lock()
	warm := p.warm
	p.warm = nil
	p.mu.Unlock()
	for _, inst := range warm {
		if err := p.sup.Terminate(inst.ID); err != nil {
			p.log.Warn("pool close: terminate", "id", inst.ID, "err", err)
		}
	}
}

// Depth returns the current number of warm instances in the pool.
func (p *Pool) Depth() int { return p.depth() }

func (p *Pool) depth() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.warm)
}

// ── internal ─────────────────────────────────────────────────────────────────

func (p *Pool) triggerRefill() {
	select {
	case p.refill <- struct{}{}:
	default: // refill queue already has a pending signal; don't block
	}
}

func (p *Pool) refillLoop() {
	for {
		select {
		case <-p.done:
			return
		case <-p.refill:
			p.mu.Lock()
			need := p.cfg.Depth - len(p.warm)
			p.mu.Unlock()
			for i := 0; i < need; i++ {
				if err := p.refillOne(); err != nil {
					p.log.Warn("pool: refill failed", "err", err)
					// Back-off and retry; don't flood the log.
					select {
					case <-time.After(2 * time.Second):
					case <-p.done:
						return
					}
				}
			}
		}
	}
}

func (p *Pool) refillOne() error {
	ctx, cancel := context.WithTimeout(context.Background(), p.cfg.RefillTimeout)
	defer cancel()

	start := time.Now()
	inst, err := p.restore(ctx)
	if err != nil {
		return fmt.Errorf("pool refill: %w", err)
	}
	elapsed := time.Since(start)
	p.mu.Lock()
	p.warm = append(p.warm, inst)
	depth := len(p.warm)
	p.mu.Unlock()
	p.log.Info("pool: refilled", "id", inst.ID, "restore_ms", elapsed.Milliseconds(), "depth", depth)
	return nil
}

func (p *Pool) restore(ctx context.Context) (*Instance, error) {
	return p.sup.LoadSnapshot(ctx, p.cfg.Snapshot, p.cfg.Spec)
}
