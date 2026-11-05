# Provision status in the in-memory outputs directory

**Date:** 2026-11-05
**Addresses:** ptone/scion#7287

## Problem

`sciontool harness provision` records its outcome in the harness outputs directory: "running" first, then "ok" or "failed". The status lets a failed `20-harness-provision` pre-start hook report its reason. `WriteHarnessProvisionStatus` never created that directory.

When `SCION_HARNESS_OUTPUTS_DIR` moves the outputs to the in-memory volume (`/run/scion/mem/outputs`), only a root `sciontool init` creates the directory (`stagedsecrets.PrepareHarnessDir`), or the provisioner script later, when it writes its first output.

On Kubernetes NFS-home pods, init runs unprivileged and the runtime creates only the in-memory secrets directory. So on every pod start:
- the "running" write failed with `open outputs: no such file or directory`;
- a provisioning failure before the script created the directory was not recorded at all, and the pre-start error lost its reason.

The symptom was first seen on Cloud Run at an earlier base. At this base, a Cloud Run instance's root init already creates the directory before pre-start hooks, so that case is handled by init's existing directory setup and is not changed here.

## Change

`WriteHarnessProvisionStatus` may now create exactly one directory: `outputs` directly below the in-memory root. It does so only when the writer is not root.
- The root and every ancestor are opened without following symbolic links. A missing root is never created.
- `outputs` is created with `mkdirat` relative to the checked root descriptor, mode 0700 set explicitly whatever the umask, with no chown. Only this call's own successful `mkdirat` counts as created; `EEXIST` takes the pre-existing branch, so nothing this call did not create is ever chmod'ed. The directory is then opened `O_NOFOLLOW|O_DIRECTORY` relative to the same descriptor.
- An existing `outputs` must be a real directory owned by the writer with a mode no wider than 0700. Otherwise the write is skipped (`ErrProvisionStatusDirUnsafe`, logged as a warning by the recorder), and the directory is never repaired.
- The leaf is written through the directory's own descriptor.
- A root writer, and every other path (including the home bundle's outputs directory), behave as before: nothing is created.

The fd-level code is in `provision_status_unix.go`. `provision_status_other.go` fails closed on other platforms (`errors.ErrUnsupported`); the package is part of the Windows build of `./cmd/scion`, which this stub keeps compiling.

## Tests

`pkg/sciontool/hooks/provision_status_unix_test.go` covers:
- a first start with the directory missing;
- the created directory's mode and owner under two umasks;
- an existing private directory used unchanged (including a narrower mode);
- the skip branches: not a directory, another owner, wider modes;
- a symbolic link at `outputs`, and at the in-memory root;
- an existing directory, symbolic link or file at `outputs` taking the pre-existing branch;
- an existing directory below a root the writer cannot write still being used (`EEXIST` before a permission error);
- only the fixed path created (never a missing root);
- the home outputs directory never created;
- a root writer never creating.

`cmd/sciontool/commands/harness_status_test.go` covers the first-start "running" status, an early failure recorded, and the warning for an unsafe directory.

Tests that create the directory skip when the test process is root, because the writer must own what it creates.

- The symlink-target tests use a 0700, writer-owned target (`t.TempDir` creates its directories 0777 less the umask), so only the no-follow open of `outputs` can refuse the link; dropping `O_NOFOLLOW` from that open makes them fail.
