// Copyright 2026 The Scion Authors.

package agent

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// The agent home is bind-mounted into the agent container, and the files
// the harness writes there are owned by the uid it ran as. When that uid
// differs from the agent runtime's uid, the start-time writes the runtime
// makes in an existing agent's home (removing the stale provisioner hook
// and bundle, recording the run ID) fail with a permission error
// (ptone/scion#4330). On such an error the runtime repairs ownership of the
// home once per start, through its own one-shot helper, and retries the
// write once; otherwise the start fails with an AgentHomeOwnershipError.

// AgentHomeOwnershipError reports an agent home write that failed with a
// permission error and could not be fixed by repairing ownership.
type AgentHomeOwnershipError struct {
	// Path is the path the write failed on.
	Path string
	// Dir is the directory holding Path, whose ownership blocked the write.
	Dir string
	// DirUID and DirGID are Dir's owner, -1 when unknown.
	DirUID, DirGID int
	// RuntimeUID and RuntimeGID are the agent runtime process's ids.
	RuntimeUID, RuntimeGID int
	// RepairErr is why the repair failed, nil when it ran but the write
	// still failed.
	RepairErr error
	// Err is the write error.
	Err error
}

func (e *AgentHomeOwnershipError) Error() string {
	owner := "an unknown owner"
	if e.DirUID >= 0 {
		owner = fmt.Sprintf("uid %d (gid %d)", e.DirUID, e.DirGID)
	}
	msg := fmt.Sprintf("agent home is not writable by the agent runtime: %v; %s is owned by %s but the agent runtime runs as uid %d (gid %d)",
		e.Err, e.Dir, owner, e.RuntimeUID, e.RuntimeGID)
	if e.RepairErr != nil {
		return msg + fmt.Sprintf("; ownership repair failed: %v", e.RepairErr)
	}
	return msg + "; the write still failed after ownership repair"
}

func (e *AgentHomeOwnershipError) Unwrap() error { return e.Err }

// newAgentHomeOwnershipError builds the error for a permission error err.
func newAgentHomeOwnershipError(err, repairErr error) *AgentHomeOwnershipError {
	e := &AgentHomeOwnershipError{
		DirUID: -1, DirGID: -1,
		RuntimeUID: os.Getuid(), RuntimeGID: os.Getgid(),
		RepairErr: repairErr, Err: err,
	}
	var pe *fs.PathError
	if errors.As(err, &pe) {
		e.Path = pe.Path
		// A remove, a create or a mkdir is blocked by the directory that
		// holds the entry.
		e.Dir = filepath.Dir(pe.Path)
		if uid, gid, ok := pathOwner(e.Dir); ok {
			e.DirUID, e.DirGID = uid, gid
		}
	}
	return e
}

func pathOwner(path string) (uid, gid int, ok bool) {
	fi, err := os.Lstat(path)
	if err != nil {
		return 0, 0, false
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, false
	}
	return int(st.Uid), int(st.Gid), true
}

// agentHomeOwner is the owner the runtime advertises to the agent
// container as SCION_HOST_UID and SCION_HOST_GID
// (runtime.AdvertisedHostOwnerIDs), for the workspace backend this start
// uses.
type agentHomeOwner struct {
	backend  string
	uid, gid int
}

// advertisedAgentHomeOwner returns the agentHomeOwner for this start. The
// nfs workspace backend is selected for every sharing mode Start uses
// whenever workspace_storage.backend is "nfs" (runtime.SelectWorkspaceBackend),
// so the backend is read from that setting directly; any other backend
// advertises the agent runtime's own ids.
func advertisedAgentHomeOwner(settings *config.VersionedSettings) agentHomeOwner {
	backend := ""
	nfsUID, nfsGID := 0, 0
	if settings != nil && settings.Server != nil && settings.Server.WorkspaceStorage != nil {
		ws := settings.Server.WorkspaceStorage
		backend = ws.Backend
		if ws.NFS != nil {
			nfsUID, nfsGID = ws.NFS.UID, ws.NFS.GID
		}
	}
	uid, gid := runtime.AdvertisedHostOwnerIDs(backend, nfsUID, nfsGID)
	return agentHomeOwner{backend: backend, uid: uid, gid: gid}
}

// errAgentHomeRepairNotAllowed is the repair error when the advertised
// owner is not the agent runtime itself.
var errAgentHomeRepairNotAllowed = errors.New("agent home ownership repair not allowed")

// agentHomeRepair repairs ownership of one agent home, at most once.
type agentHomeRepair struct {
	rt    runtime.Runtime
	home  string
	image string
	owner agentHomeOwner

	attempted bool
	err       error
}

type agentHomeRepairKey struct{}

// contextWithAgentHomeRepair attaches the repair for agentHome to ctx, for
// writeAgentHome. Start attaches it once the agent's image is resolved.
func contextWithAgentHomeRepair(ctx context.Context, rt runtime.Runtime, agentHome, image string, owner agentHomeOwner) context.Context {
	if agentHome == "" {
		return ctx
	}
	return context.WithValue(ctx, agentHomeRepairKey{}, &agentHomeRepair{rt: rt, home: agentHome, image: image, owner: owner})
}

// repair runs the ownership repair the first time it is called and returns
// its result on every call.
func (r *agentHomeRepair) repair(ctx context.Context) error {
	if r.attempted {
		return r.err
	}
	r.attempted = true
	r.err = r.run(ctx)
	if r.err != nil {
		slog.Error("Agent home ownership repair failed", "home", r.home, "error", r.err)
	} else {
		slog.Info("Repaired agent home ownership", "home", r.home, "uid", r.owner.uid, "gid", r.owner.gid)
	}
	return r.err
}

func (r *agentHomeRepair) run(ctx context.Context) error {
	// The repair only gives the home back to the agent runtime when that is
	// also the owner the container realigns its user to. On the nfs
	// backend the owner is a stable, node-independent uid and gid set once
	// for the export; changing it on a restart would undo that, so the
	// agent runtime must be able to write the home as it is.
	if r.owner.backend == "nfs" || r.owner.uid != os.Getuid() {
		return fmt.Errorf("%w: the agent runtime (uid %d, gid %d) must be able to write the agent home as the workspace owner uid %d (gid %d), or through that group, for the %q workspace backend; moving the agent runtime's own state out of the agent home is tracked in ptone/scion#4402",
			errAgentHomeRepairNotAllowed, os.Getuid(), os.Getgid(), r.owner.uid, r.owner.gid, r.owner.backend)
	}
	repairer, ok := r.rt.(runtime.AgentHomeOwnershipRepairer)
	if !ok {
		return fmt.Errorf("%w (%s)", runtime.ErrAgentHomeRepairUnsupported, r.rt.Name())
	}
	if r.image == "" {
		return errors.New("no agent image to run the ownership repair helper from")
	}
	exists, err := r.rt.ImageExists(ctx, r.image)
	if err != nil {
		return fmt.Errorf("check image %q for the ownership repair helper: %w", r.image, err)
	}
	if !exists {
		if err := r.rt.PullImage(ctx, r.image); err != nil {
			return fmt.Errorf("pull image %q for the ownership repair helper: %w", r.image, err)
		}
	}
	return repairer.RepairAgentHomeOwnership(ctx, runtime.AgentHomeOwnershipRepair{
		HomeDir: r.home, Image: r.image, UID: r.owner.uid, GID: r.owner.gid,
	})
}

// writeAgentHome runs write, a write into the agent home. On a permission
// error it repairs the home's ownership (once per start, through the repair
// attached to ctx) and runs write once more. A permission error that
// remains is returned as an *AgentHomeOwnershipError naming the path and
// the uid mismatch. Other errors are returned unchanged. Without an
// attached repair, a permission error is returned as an
// *AgentHomeOwnershipError right away.
func writeAgentHome(ctx context.Context, write func() error) error {
	err := write()
	if err == nil || !errors.Is(err, fs.ErrPermission) {
		return err
	}
	r, _ := ctx.Value(agentHomeRepairKey{}).(*agentHomeRepair)
	if r == nil {
		return newAgentHomeOwnershipError(err, errors.New("no ownership repair available at this point of the start"))
	}
	found := newAgentHomeOwnershipError(err, nil)
	slog.Warn("Agent home write failed with a permission error; repairing ownership and retrying once",
		"home", r.home, "dir", found.Dir, "dir_uid", found.DirUID, "dir_gid", found.DirGID,
		"runtime_uid", found.RuntimeUID, "error", err)
	if repairErr := r.repair(ctx); repairErr != nil {
		return newAgentHomeOwnershipError(err, repairErr)
	}
	if err := write(); err != nil {
		if errors.Is(err, fs.ErrPermission) {
			return newAgentHomeOwnershipError(err, nil)
		}
		return err
	}
	return nil
}

// warnAgentHomeOwnerMismatch logs a warning when agentHome is not owned by
// the agent runtime's uid: the harness ran as a different uid than the
// runtime, so the runtime may be unable to update its state in the home.
// It reports only; writeAgentHome does the repair.
func warnAgentHomeOwnerMismatch(agentName, agentHome string, owner agentHomeOwner) {
	uid, gid, ok := pathOwner(agentHome)
	if !ok {
		return
	}
	if runtimeUID := os.Getuid(); uid != runtimeUID {
		slog.Warn("Agent home is owned by a different uid than the agent runtime; the harness in the container ran as a uid that does not match the agent runtime's, so start-time writes to the agent home may need an ownership repair",
			"agent", agentName, "home", agentHome, "home_uid", uid, "home_gid", gid,
			"runtime_uid", runtimeUID, "runtime_gid", os.Getgid(),
			"advertised_uid", owner.uid, "advertised_gid", owner.gid, "workspace_backend", owner.backend)
	}
}
