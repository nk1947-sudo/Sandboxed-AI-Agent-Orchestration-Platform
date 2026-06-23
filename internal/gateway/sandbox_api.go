//go:build linux

package gateway

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/yourorg/sandbox-platform/internal/auth"
	"github.com/yourorg/sandbox-platform/internal/orchestrator"
	"github.com/yourorg/sandbox-platform/internal/store/pg"
)

// authorizeSandbox loads a sandbox and checks the principal owns it (or is an
// admin). On any failure it writes the error response and returns ok=false.
// Callers must have verified h.db != nil.
func (h *Handler) authorizeSandbox(w http.ResponseWriter, r *http.Request, id string) (pg.Sandbox, bool) {
	sb, err := h.db.Sandboxes.ByID(r.Context(), id)
	if errors.Is(err, pg.ErrNotFound) {
		writeJSON(w, http.StatusNotFound, errBody("sandbox not found"))
		return pg.Sandbox{}, false
	}
	if err != nil {
		h.log.Error("sandbox lookup", "id", id, "err", err)
		writeJSON(w, http.StatusInternalServerError, errBody("lookup failed"))
		return pg.Sandbox{}, false
	}
	p, _ := auth.FromContext(r.Context())
	if !p.IsAdmin() && sb.OwnerID != "" && sb.OwnerID != p.UserID {
		writeJSON(w, http.StatusForbidden, errBody("not your sandbox"))
		return pg.Sandbox{}, false
	}
	return sb, true
}

// listSandboxes returns the caller's durable sandbox history (admins see all).
func (h *Handler) listSandboxes(w http.ResponseWriter, r *http.Request) {
	if h.db == nil {
		writeJSON(w, http.StatusNotImplemented, errBody("history not enabled"))
		return
	}
	p, _ := auth.FromContext(r.Context())
	owner := p.UserID
	if p.IsAdmin() {
		owner = "" // see every sandbox
	}
	list, err := h.db.Sandboxes.ListByOwner(r.Context(), owner)
	if err != nil {
		h.log.Error("list sandboxes", "err", err)
		writeJSON(w, http.StatusInternalServerError, errBody("list failed"))
		return
	}
	if list == nil {
		list = []pg.Sandbox{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"sandboxes": list, "count": len(list)})
}

// stopSandbox snapshots a running sandbox (memory + device state + disk) and
// halts the live VM, making it resumable to its exact prior state.
func (h *Handler) stopSandbox(w http.ResponseWriter, r *http.Request) {
	if h.db == nil || h.snap == nil || h.sup == nil {
		writeJSON(w, http.StatusNotImplemented, errBody("snapshots not enabled"))
		return
	}
	id := r.PathValue("id")
	if _, ok := h.authorizeSandbox(w, r, id); !ok {
		return
	}
	if _, live := h.sup.Get(id); !live {
		writeJSON(w, http.StatusConflict, errBody("sandbox is not running"))
		return
	}

	snap, err := h.sup.StopWithSnapshot(r.Context(), h.snap, id)
	if err != nil {
		h.log.Error("stop with snapshot", "id", id, "err", err)
		writeJSON(w, http.StatusInternalServerError, errBody("stop failed"))
		return
	}

	var size int64
	for _, p := range []string{snap.Paths.MemFile, snap.Paths.StateFile, snap.Paths.DiskFile} {
		if fi, statErr := os.Stat(p); statErr == nil {
			size += fi.Size()
		}
	}
	if _, err := h.db.Snapshots.Create(r.Context(), pg.Snapshot{
		SandboxID:     id,
		MemFilePath:   snap.Paths.MemFile,
		StateFilePath: snap.Paths.StateFile,
		DiskFilePath:  snap.Paths.DiskFile,
		SizeBytes:     size,
		GuestVCPUs:    int(snap.GuestVCPUs),
		GuestMemMiB:   int(snap.GuestMemMiB),
	}); err != nil {
		h.log.Error("record snapshot", "id", id, "err", err)
	}
	if err := h.db.Sandboxes.MarkStopped(r.Context(), id); err != nil {
		h.log.Warn("mark stopped", "id", id, "err", err)
	}
	p, _ := auth.FromContext(r.Context())
	h.audit(r, p.UserID, "sandbox.stop", id, map[string]any{"snapshot": snap.ID})
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "status": "stopped", "snapshot": snap.ID})
}

// resumeSandbox restores a stopped sandbox from its latest snapshot, reusing the
// same logical id so history and terminal sessions stay consistent.
func (h *Handler) resumeSandbox(w http.ResponseWriter, r *http.Request) {
	if h.db == nil || h.snap == nil || h.sup == nil {
		writeJSON(w, http.StatusNotImplemented, errBody("resume not enabled"))
		return
	}
	id := r.PathValue("id")
	sb, ok := h.authorizeSandbox(w, r, id)
	if !ok {
		return
	}
	if _, live := h.sup.Get(id); live {
		writeJSON(w, http.StatusConflict, errBody("sandbox already running"))
		return
	}

	snap, err := h.db.Snapshots.Latest(r.Context(), id)
	if errors.Is(err, pg.ErrNotFound) {
		writeJSON(w, http.StatusConflict, errBody("no snapshot to resume"))
		return
	}
	if err != nil {
		h.log.Error("latest snapshot", "id", id, "err", err)
		writeJSON(w, http.StatusInternalServerError, errBody("lookup failed"))
		return
	}

	orchSnap := orchestrator.Snapshot{
		ID:        snap.ID,
		SandboxID: id,
		Paths: orchestrator.SnapshotPaths{
			MemFile:   snap.MemFilePath,
			StateFile: snap.StateFilePath,
			DiskFile:  snap.DiskFilePath,
		},
		GuestMemMiB: int64(snap.GuestMemMiB),
		GuestVCPUs:  int64(snap.GuestVCPUs),
	}
	spec := orchestrator.LaunchSpec{
		ID:         id, // reuse the same id (the old VM was torn down on stop)
		VCPUs:      int64(sb.VCPUs),
		MemMiB:     int64(sb.MemMiB),
		CPUPercent: sb.CPUPercent,
		PidsMax:    int64(sb.PidsMax),
	}
	inst, err := h.sup.LoadSnapshot(r.Context(), orchSnap, spec)
	if err != nil {
		h.log.Error("resume", "id", id, "err", err)
		writeJSON(w, http.StatusInternalServerError, errBody("resume failed"))
		return
	}

	if err := h.db.Sandboxes.Upsert(r.Context(), pg.Sandbox{
		ID:         id,
		OwnerID:    sb.OwnerID,
		Name:       sb.Name,
		Status:     pg.SandboxRunning,
		VCPUs:      sb.VCPUs,
		MemMiB:     sb.MemMiB,
		CPUPercent: sb.CPUPercent,
		PidsMax:    sb.PidsMax,
		CID:        int64(inst.CID),
	}); err != nil {
		h.log.Warn("update sandbox on resume", "id", id, "err", err)
	}
	p, _ := auth.FromContext(r.Context())
	h.audit(r, p.UserID, "sandbox.resume", id, map[string]any{"snapshot": snap.ID})
	writeJSON(w, http.StatusOK, instanceToView(inst))
}

// renameSandbox sets a human-friendly name.
func (h *Handler) renameSandbox(w http.ResponseWriter, r *http.Request) {
	if h.db == nil {
		writeJSON(w, http.StatusNotImplemented, errBody("history not enabled"))
		return
	}
	id := r.PathValue("id")
	if _, ok := h.authorizeSandbox(w, r, id); !ok {
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("invalid JSON"))
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		writeJSON(w, http.StatusBadRequest, errBody("name required"))
		return
	}
	if err := h.db.Sandboxes.Rename(r.Context(), id, req.Name); err != nil {
		h.log.Error("rename sandbox", "id", id, "err", err)
		writeJSON(w, http.StatusInternalServerError, errBody("rename failed"))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// deleteSandbox tears down any live VM, deletes snapshot artefacts (files +
// rows), and archives the history entry.
func (h *Handler) deleteSandbox(w http.ResponseWriter, r *http.Request) {
	if h.db == nil {
		writeJSON(w, http.StatusNotImplemented, errBody("history not enabled"))
		return
	}
	id := r.PathValue("id")
	if _, ok := h.authorizeSandbox(w, r, id); !ok {
		return
	}
	if h.sup != nil {
		if _, live := h.sup.Get(id); live {
			_ = h.sup.Terminate(id)
		}
	}
	snaps, err := h.db.Snapshots.ListBySandbox(r.Context(), id)
	if err == nil {
		for _, s := range snaps {
			_ = os.RemoveAll(filepath.Dir(s.MemFilePath))
			_ = h.db.Snapshots.Delete(r.Context(), s.ID)
		}
	}
	if err := h.db.Sandboxes.Archive(r.Context(), id); err != nil {
		h.log.Error("archive sandbox", "id", id, "err", err)
		writeJSON(w, http.StatusInternalServerError, errBody("delete failed"))
		return
	}
	p, _ := auth.FromContext(r.Context())
	h.audit(r, p.UserID, "sandbox.delete", id, nil)
	w.WriteHeader(http.StatusNoContent)
}

// getTranscript returns a sandbox's persisted terminal transcript for replay.
func (h *Handler) getTranscript(w http.ResponseWriter, r *http.Request) {
	if h.db == nil {
		writeJSON(w, http.StatusNotImplemented, errBody("transcripts not enabled"))
		return
	}
	id := r.PathValue("id")
	if _, ok := h.authorizeSandbox(w, r, id); !ok {
		return
	}
	var after int64
	if v := r.URL.Query().Get("after"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			after = n
		}
	}
	lines, err := h.db.Transcripts.List(r.Context(), id, after, 5000)
	if err != nil {
		h.log.Error("get transcript", "id", id, "err", err)
		writeJSON(w, http.StatusInternalServerError, errBody("transcript failed"))
		return
	}
	if lines == nil {
		lines = []pg.TranscriptLine{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"lines": lines, "count": len(lines)})
}
