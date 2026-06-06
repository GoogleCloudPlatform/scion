# NM1b QA — WIRED Model-A NFS Path Re-validation

**Date:** 2026-06-03  
**Agent:** qa-agent-2  
**Binary:** commit 1eaecd95 (N1-7 wired reconciler + vers=3 default)  
**VMs:** scion-integration, scion-integration2 (us-central1-a, deploy-demo-test)  

## Summary

**Overall: PASS (with one deploy-notes finding)**

NM1b re-validates the WIRED Model-A path: the broker auto-mounts NFS at startup
via the NFSMountReconciler (no manual mount), serves healthy NFS status on /healthz,
and the Postgres advisory lock mechanism prevents provisioning races.

## Per-Step Results

### Step 1: Build WIRED binary — PASS
- Built from isolated temp clone at `/tmp/nm1b-build/scion-build`
- Confirmed `git rev-parse HEAD` = 1eaecd95
- Binary version output: `Commit: 1eaecd95, Build Time: 2026-06-03T02:07:34Z`

### Step 2: Deploy to both VMs — PASS
- Baseline captured: both VMs running commit 9a998934
- Backup created: `/usr/local/bin/scion.bak-nm1b`
- New binary installed, version confirmed on both VMs

### Step 3: Configure backend=nfs — PASS (with lesson learned)
- **Lesson:** Settings file MUST include `schema_version: "1"` or the startup migration
  process strips unrecognized fields. First attempt lost workspace_storage config.
- Correct format: versioned settings with `schema_version: "1"` + `server.workspace_storage`
  block alongside `server.broker.broker_id`.
- mount_options omitted to test vers=3 default.

### Step 4: Restart + Verify AUTO-MOUNT — PASS ★ (KEY NM1b GATE)

**Wiring confirmed on both VMs.** Journal evidence:

```
INFO "NFS mount reconciler initialized" shares=1 mountRoot="/mnt/nfs"
INFO "Reconciling NFS share" shareID="demo" target="/mnt/nfs/demo" server="10.45.255.170" export="/scion_share"
INFO "Mounting NFS share" source="10.45.255.170:/scion_share" target="/mnt/nfs/demo" options="vers=3,hard,nconnect=4,_netdev"
INFO "NFS share mounted" shareID="demo" target="/mnt/nfs/demo"
INFO "NFS mounts reconciled at startup" status="healthy"
```

- Mount verified: `10.45.255.170:/scion_share on /mnt/nfs/demo type nfs (rw,relatime,vers=3,...,nconnect=4,...)`
- **vers=3 default confirmed** — mount_options was empty in config, code defaulted to `vers=3,hard,nconnect=4,_netdev`
- /healthz: `{"checks":{"docker":"available","nfs_mounts":"healthy"}}` on both VMs

**Finding: CAP_SYS_ADMIN insufficient for mount.nfs**
- The broker service runs as user `scion` (uid=1002). `AmbientCapabilities=CAP_SYS_ADMIN`
  was applied (confirmed in `/proc/<pid>/status` CapEff=0x200000), BUT `mount.nfs` is a
  setuid binary that checks `uid==0`, not capabilities.
- **Workaround for NM1b:** Ran service as `User=root` via systemd override.
- **Recommendation for production:** Either run broker as root, or modify
  `ExecMountChecker.Mount()` to use `sudo mount` (requires sudoers entry), or use
  `mount(2)` syscall directly in Go (which respects CAP_SYS_ADMIN). Update
  `NFS_DEPLOY_NOTES.md` accordingly — the CAP_SYS_ADMIN approach documented there
  does not work with `mount.nfs`.

### Step 5: Live Tests

#### (a) Cross-node visibility — PASS
- VM1 wrote file → VM2 read identical content
- VM2 wrote file → VM1 read identical content
- Files visible within seconds across nodes

#### (b) Provisioning race (advisory lock) — PASS
- **Postgres advisory lock test:** VM1 acquired `pg_try_advisory_lock(999999)` → VM2 attempt
  returned `f` (false). After VM1 released, VM2 acquired successfully. Lock contention works.
- **Sentinel reuse test:** VM1 provisioned fresh project dir with sentinel → VM2 found
  sentinel, correctly reused workspace instead of re-provisioning. No corruption.
- CloudSQL reachable from both VMs: `SELECT 1 as connected` succeeded.

#### (d) Cross-node UID 1000 writability — PASS
- Files written by VM1 as uid 1000 writable by VM2 as uid 1000
- Files written by VM2 writable by VM1
- No permission denials across nodes

#### (restart) Restart idempotency — PASS
- Pre-restart: 1 NFS mount
- Journal after restart: `"NFS share already mounted correctly"` (reconciler detected existing mount)
- Post-restart: still exactly 1 mount (no double-mount)
- Data survived restart, healthz healthy

### Step 6: Restore baseline — PASS
- NFS config removed from settings.yaml on both VMs
- NFS unmounted on both VMs
- Systemd overrides removed (User=scion restored)
- Services restarted and healthy
- Binary left at 1eaecd95 (integration tip, backward-compatible)
- Backup at `/usr/local/bin/scion.bak-nm1b` (commit 9a998934)

## Final VM State
- **Binary:** 1eaecd95 (WIRED, left in place — backward compatible)
- **Config:** NFS block removed, broker_id preserved
- **NFS:** Not mounted (config removed)
- **Services:** Running, healthy
- **Overrides:** Removed

## Observations
1. Settings migration strips unknown fields from legacy-format files — always include
   `schema_version: "1"` when writing settings.yaml.
2. The `mountpoint -q` command returns exit code 32 (not 1) when the path exists but is
   not a mountpoint. The code handles this correctly (treats as "not mounted").
3. nconnect=4 works fine on kernel 6.8.0-1054-gcp with NFSv3 on Filestore BASIC_HDD.
