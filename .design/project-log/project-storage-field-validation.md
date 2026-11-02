# Project storage: one validator for the storage block fields

**Date:** 2026-10-27
**Branch:** scion/fsstore-psvalidate

## Summary

`config.ValidateProjectStorage(ps, origin)` (`pkg/config/project_storage_validate.go`) is now the one
validator for the fields of `server.project_storage`. It runs the block's structural checks
(`Validate`) and then field rules for the selected backend: nfs share ids, servers, exports,
mount options, `mount_root`, owner ids, `pv_name` and `storage_class`, volume names and claim names,
and `subpath_root`. It returns `[]FieldError` (settings path, rule, value). It is a pure function
with no I/O, so later callers can reuse it as is (a pending-restart health check, and a hub that
delivers a managed block).

- `OriginFile` (host file and environment) stays backward compatible: existing share ids such as
  `scion-hub1` stay valid, and mount options are checked for syntax only.
- `OriginManaged` adds: share ids must be DNS labels, mount options must be on an allowlist,
  `mount_root` and `auto_mount` must not be set, and uid and gid must be non-zero.

## Call sites

- `SynthesizeProjectStorage`, the single resolution, calls it. That covers the hub start
  (`resolveHubProjectStorage`), the broker start (`brokerStorageLayout`, `brokerNFSConfig`), both
  admin settings write paths (file mode and workstation), `scion config validate`, and the
  per-agent resolutions. A block synthesized from the deprecated blocks is checked by the same rules
  and reported under the deprecated keys. The shared-dir-only export of the transitional split does
  not need `server` or `export`.
- The broker's earlier copy of the share checks, `validateBrokerNFS`, is removed.

## Behaviour changes

- **Broker start with an invalid block:** the broker no longer falls back to the local layout.
  `runtimebroker.ServerConfig.StorageError` is set. Create, start and restart are refused with
  `422 storage_invalid`, which names the fields but not their values. Stop, delete and logs keep
  working. `/healthz` is degraded with `project_storage: unavailable`. The storage descriptor is
  sent as `{backend, invalid: true}`, and `projectfs.Layout.DescriptorDiff` treats it as a mismatch.
- **Admin PUT:** an invalid block returns `400`, with `details.fieldErrors`. Nothing is written.
- **Mount options:** every origin refuses `suid` and `dev`, and `nosuid`, `nodev` or `rw` with a
  value. File blocks keep options such as `exec` and `defaults`. Managed blocks use an allowlist. Mount options are hardened
  separately (the mount code is not changed here).
- **IPv6 server literals** stay refused, as the broker refused them before
  (`allowIPv6NFSServer`, one switch). The validator refuses everything the broker's former checks
  refused (superset test).
- **`mount_root`** must now be clean (no trailing `/`).

## Not in this change

- The pending-restart health check, which reports a hand-edited invalid file before restart.
- Applying a hub-managed block on the broker.
- Removing the per-agent re-reads of the settings file. Agent start paths still read the global
  settings, not the immutable startup Layout.

## Reporting storage differences by field name

`projectfs.LayoutIssue` and `projectfs.RenderLayoutIssues(items, view)` are now the one place where
storage layout values become text. There are three views:

- **Public:** field names only, plus a fixed "ask a hub admin" remediation.
- **Admin:** adds the values.
- **Log:** the full form, with values.

`Layout.DescriptorIssues` returns the hub/broker comparison as issues (`DescriptorDiff` is now its
log view), and `config.FieldErrorList.Issues` converts validation errors. These responses now use
the public view:

- the dispatch refusal for a layout mismatch, in both its message and its `storageLayoutDiff`
  details;
- agent-move refusals between brokers on different exports, or with different export identity
  markers;
- the broker's refusal of agent starts for an invalid block;
- the health summary's `layout_mismatch` for non-admin readers;
- broker records read by non-admins. Their storage descriptor drops the server, export, subpath
  root, export identity marker and claim.

Hub admins, the admin settings validation responses, and the broker reading its own record still
see values. A test checks that pkg/hub code never formats layout value fields with `fmt`.
