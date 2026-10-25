# skip-summary scenarios

Each directory is one run of a real Go test binary, used by
`hack/hubshard/test-skip-summary.sh` (`make hubshard-skip-summary-selftest`)
to check `run_skip_summary` in `hack/hubshard/lib.sh` without Go:

- `plain.txt`: the binary's stdout without `-test.v`
- `verbose.txt`: its stdout with `-test.v` appended
- `rc`: its exit status (the same in both runs)
- `summary.txt`: the skip summary `run_skip_summary` prints for `verbose.txt`

The binary is `go test -c` (Go 1.26.1) of a package holding only
[`toy_test.go.txt`](toy_test.go.txt) (as `toy_test.go`, module `toy`), run
with `-test.paniconexit0 -test.count=1 -test.run='^Name$'`:

| Scenario | -test.run | Other |
|---|---|---|
| pass-logs | `TestPassLogs` | |
| skips | `TestSkipTop\|TestSkipSub\|TestSkipNow\|TestSkipEmpty` | |
| fail-subtest | `TestFailSub` | |
| nested | `TestNested` | |
| nested-parallel | `TestNestedParallel` | |
| panic | `TestPassLogs\|TestPanic` | |
| timeout | `TestSkipTop\|TestTimeout` | `-test.timeout=1s` |
| testmain-exit3 | `TestPassLogs\|TestSkipTop` | env `TOY_EXIT3=1` |
| long-line | `TestLong` | |
| non-utf8 | `TestNonUTF8` | |
| example-fail | `Example_fail` | |
| parallel | `TestParA\|TestParB\|TestParC` | |
| slash-subtest | `TestSlashName` | |
| skip-after-subtest | `TestSkipAfterSub` | |
| slash-sibling | `TestSlashSibling` | |
| slash-sibling-fail | `TestSlashSiblingFail` | |
| skip-sibling-parent | `TestSkipSiblingParent` | |

In `long-line`, each run of 200000 `x` bytes is stored as `@X200000@`;
the self-test expands it.

The files are kept byte for byte (`.gitattributes` turns off whitespace
checks here): `t.Skip()` with no message logs a line ending in a space.
