# Defect doc revision 3 — four corrections from architect review

**Date:** 2026-08-26
**Agent:** dev-defect-rev3
**Files changed:** `defect-sandbox-delete-hang.md` (repo + scratchpad copies)

## What was done

Applied four corrections from the architect's review of the defect doc revision 3
draft:

1. **Process state characters added to persistence table.** Deployed
   `probe_state_chars.py` to Instance `val-delete-2` (run `psc-1787713650`,
   03:07–03:08 UTC). Every cell in the persistence table now carries the
   `/proc/<pid>/stat` field-3 state character. All orphans (both `runsc delete`
   and `runsc state`) are in state **S** (interruptible sleep) at t=5 s, t=30 s,
   and t=60 s — none reached **Z** (zombie). This substantiates the "worse
   persistence profile" claim: the orphans are live sleeping processes, not
   defunct zombies awaiting reap.

2. **Tables labeled with their source runs.** The probe-count table cites
   `delete_timeout_validation_v4.py` (02:42–02:45 UTC). The persistence table
   cites `probe_state_chars.py` run `psc-1787713650` (03:07–03:08 UTC). A reader
   can now trace each data point to its validation script and time window.

3. **Content placed as §4b, not §8.** The `sandbox exec` orphan finding now sits
   immediately after §4 (the refuse-but-kill defect), grouping all three
   subcommand defects together. Sections 5–7 retain their original numbering.

4. **Upstream question folded into §6.** "Is the `runsc state` hang the same root
   cause as the `runsc delete --force` hang?" is now item 5 in §6's numbered
   list, rather than a stranded question at the end of the finding.

## Probe details

- **Script:** `probe_state_chars.py` uploaded to
  `gs://ptone-experiments-instance-gym/validation/probe_state_chars.py`
- **Instance:** `val-delete-2` in `ptone-experiments/us-east4` (still running)
- **Method:** Created sandbox, issued `delete --force` (backgrounded), ran 3
  `sandbox exec` probes, then read `/proc/<pid>/stat` field 3 at t=5 s, t=30 s,
  t=60 s for every sandbox-related process.
- **Result:** 1 `runsc-delete` (S) + 3 `runsc-state` (S) at all three
  checkpoints. No process reached zombie state within 60 s.
