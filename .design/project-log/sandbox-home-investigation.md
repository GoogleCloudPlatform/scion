# Sandbox HOME/tmux Hook Investigation

**Date:** 2026-08-26
**Investigator:** sn-impl-em3
**Status:** Experiments complete, results delivered to architect

## Investigation Summary

Two independent bugs identified that both result in the tmux pane-exited hook
not loading in Cloud Run sandboxes:

### Bug 1: HOME not set (LIVE on Cloud Run)

**Chain:**
1. Cloud Run launcher runs as root → `os.Getuid() = 0` → `SCION_HOST_UID=0`
2. Sandbox's `setupHostUser()` gets uid=0 → returns `(0, 0, false)`
3. Supervisor config: `{UID: 0, Username: "scion", Rootless: false}`
4. Supervisor condition `UID > 0 || Rootless` → FALSE → HOME not set
5. `s.cmd.Env` stays nil → child inherits parent env → no HOME (sandbox starts with only envFor vars)
6. tmux has no -f flag → uses default config lookup → getpwuid(0) → root → /root/.tmux.conf → missing
7. No pane-exited hook → session outlives agent

**Verified by experiment:** `sudo env -i SCION_HOST_UID=0 ... sciontool init -- sh -c 'env | sort'`
shows NO HOME, NO USER, NO LOGNAME. Control with UID=1002 shows HOME=/home/scion.

**Side effects:**
- `usermod -o -u 0 scion` creates two UID-0 entries in /etc/passwd
- All files under /home/scion get chowned to root
- Any process at the original UID becomes orphaned

### Bug 2: Cross-filesystem relocateToScion (LATENT)

**Chain:**
1. If cfg.HomeDir and /scion are on different filesystems
2. `os.Rename` fails with EXDEV → logged, skipped
3. `os.RemoveAll(src)` destroys all source files unconditionally
4. Net: files gone, dst empty, symlink points to empty directory

**Verified by experiment:** Go program with src on tmpfs, dst on overlayfs.
All renames fail, RemoveAll destroys everything. Same-filesystem control works.

**Cloud Run status:** NOT triggered. Both paths are on the same container
root filesystem (no volume mounts configured). But the code is a latent
data-loss bug for any deployment with volume mounts.

## Code Locations

| File | Line | What |
|---|---|---|
| cloudrun_sandbox_runtime.go | 486-488 | SCION_HOST_UID = os.Getuid() |
| init.go | 1252-1394 | setupHostUser() → usermod scion to UID 0 |
| supervisor.go | 114-122 | HOME condition: UID > 0 || Rootless |
| cloudrun_sandbox_runtime.go | 437-491 | envFor(): no HOME in env |
| cloudrun_sandbox_runtime.go | 563-564 | tmux invocation: no -f flag |
| cloudrun_sandbox_runtime.go | 336-377 | relocateToScion: cross-fs data loss |

## Full Results

See `/scion-volumes/scratchpad/exp-results-combined.md` for complete experiment
data including exact commands and outputs.
