# Substrate: lazy working-directory test does not depend on the TMPDIR layout

**Date:** 2026-11-05
**Base:** c54bc5ec787ff4106fb775f3aedb6980cedc07cc. Addresses ptone/scion#7249 (part 2).

## Change

`TestSubstrateServeInitOptions_ResolveWorkingDir_IsLazy_SeesStateAtCallTime`
(`cmd/sciontool/commands/substrate_serve_test.go`) failed whenever an ancestor of
`os.TempDir()` lacked the other-execute bit. One example is a TMPDIR inside a private 0700
directory. The test faked the scion user as uid/gid 1/1 and created its fixtures with
`os.MkdirTemp("", ...)`. `canSearchDir` decides from mode bits alone, so for uid 1 every
ancestor needed other-x, and both candidates came back "not searchable". CI passed because its
`/tmp` is 1777.

The test uses the same setup as the sibling `...FromRealEnv` tests. The fake scion user is
the test process's own uid/gid, and the fixtures live under `t.TempDir()`, so the owner bit
decides for every directory the test creates. The not-yet-usable workspace is modelled as mode
0, changed to 0o700 between building the options and calling `ResolveWorkingDir`. The assertion
is the same, the real `os.Stat` path is kept, and no skip is added.

## Notes

- The issue suggested ambient `SCION_HOST_*` variables as the cause. That does not apply: the
  package `TestMain` already clears them, and the resolver reads only `SCION_WORKSPACE_PATH`.
- Mutation check: resolving eagerly when the options are built makes the test fail (it then
  picks the home directory). `canSearchDir` does not special-case root, so this holds when the test
  runs as root too.
- Test-only change; no production code touched.
