# buildstats

`hack/buildstats` measures Go compile and test cost **the same way every time**, so before/after numbers for a refactor are comparable. Every refactor gate uses it. It is stdlib-only Go and is not part of the product.

Each run prints a human-readable table (to stderr for the measuring subcommands) and can write a JSON record (`-json FILE`, or `-json -` for stdout) that `buildstats diff` compares.

## What it measures

| Measurement | How |
|---|---|
| Wall, user, sys time and **peak RSS** of a `go build` / `go test -c` (or any command) | buildstats starts the command as its **own direct child** and reads the child's rusage from `wait4`. `ru_maxrss` includes every descendant the child waited for, so for `go` it is the RSS of the largest single process, normally the biggest compile or the link. This does not need `/usr/bin/time`. The cgroup `memory.peak` is also read before and after the run when it is readable. It covers the whole cgroup and only goes up (it cannot be reset on older kernels), so it only tells you something when it rises during the run. |
| Per-action timings | `compile` adds `-debug-actiongraph=DIR/actiongraph.json`. The summary lists the top actions by wall time (with each compile's user and sys time), plus the summed build and link time. |
| Compiler phase timings | `compile -bench-pkg PKG` adds `-gcflags=PKG=<inherited flags> -bench=DIR/bench.txt`. Unlike `-cpuprofile`, which the test-main compile overwrites, `-bench` **appends** one block per compiler invocation. You therefore get separate records for the package, its external `_test` package and `main` (the generated test main). buildstats deletes the file before each run. |
| Test-slice timing | `test` captures test2json output from `go test -json` or `go tool test2json`. It reports, per package: the result, elapsed time, top-level counts (pass/fail/skip), number of subtests, the sum of test times, tests over 1s, the share of the top 20, and the N slowest tests. |
| Dependency counts | `deps [-test] PKG...` makes one `go list -deps -json` call and reports total and non-std counts per package. The package itself is excluded, so the total equals `go list -f '{{len .Deps}}'`. With `-test` the count is the test binary's closure. |

### How `-gcflags` is handled

For each package, `cmd/go` applies only the **last** `-gcflags` whose pattern matches: first the ones from `GOFLAGS`, then the ones on the command line. Values are not combined. So that the measured compile uses the same compiler flags as an unmeasured one, `compile -bench-pkg PKG` builds its own `-gcflags` like this:

* It copies the flags that would otherwise apply to `PKG`: the last unpatterned, `all=`, or `PKG=` value from `GOFLAGS` or the command line. For example, `GOFLAGS=-gcflags=-c=1` becomes `PKG=-c=1 -bench=…`.
* It places its `-gcflags` after every `-gcflags` already on the command line.

`-bench-pkg` must be spelled exactly as in any existing `PKG=` pattern, normally the full import path. The final command is printed (shell-quoted) and stored in the record.

## Build it once

The tool's own build is a go command, so build it once and then run the binary:

```sh
go build -buildvcs=false -o /tmp/buildstats ./hack/buildstats
```

On a host where go commands must go through a queue wrapper, make **buildstats** the queued command. It then runs `go` as its own child, so the rusage it reads is the compile's, not the queue's:

```sh
<queue-tool> -- go build -buildvcs=false -o /tmp/buildstats ./hack/buildstats
<queue-tool> -- /tmp/buildstats compile ... -- go test -c ...
```

`buildstats deps` runs a single `go list`, so it also counts as one queued go command.

## Gate invocations

All compile measurements use the mandated memory-capped form: `-p 1`, `GOMAXPROCS=2`, `GOGC=40`, `GOFLAGS=-gcflags=-c=1`, `ulimit -v 16000000`, and no `GOMEMLIMIT` (the P0-8 rule). Compiling **pkg/hub or ./cmd** needs a 16G slot: announce it and wait for the GO, as the project's resource rules require. Running a built test binary also needs a GO.

To compare like with like, warm the dependency cache first, so the measured run compiles only the package under study. The `-bench` flag changes the package's action ID, so the measured package is always recompiled even when the cache is warm. Warming does not compile the package itself:

```sh
go list -deps -test -f '{{if not .Standard}}{{.ImportPath}}{{end}}' ./pkg/hub \
  | grep -v ' \[' | grep -v '\.test$' | grep -vx 'github.com/GoogleCloudPlatform/scion/pkg/hub' > /tmp/hubdeps.txt
(ulimit -v 16000000; GOMAXPROCS=2 GOGC=40 GOFLAGS= go build -p 1 -buildvcs=false $(cat /tmp/hubdeps.txt))
```

Keep `GOFLAGS=` empty for the warm-up. An unpatterned `-gcflags` applies only to the packages **named on the command line**. In the measured `go test -c ./pkg/hub`, the dependencies are not named, so they compile with the default flags. If the warm-up names them under `GOFLAGS=-gcflags=-c=1`, it caches them with `-c=1`, and the measured run then misses the cache and recompiles them. Check this with the `ran a tool` count in the actiongraph line. After a correct warm-up it is just the measured package, its test variants, the test main and the link.

### G0: dependency counts (free; no compile)

```sh
/tmp/buildstats deps -label G0-deps -json g0-deps.json \
  ./pkg/config ./pkg/hub ./pkg/agent ./pkg/runtime ./pkg/runtimebroker ./cmd
/tmp/buildstats deps -test -label G0-test-deps -json g0-test-deps.json \
  ./pkg/config ./pkg/hub ./pkg/agent ./pkg/runtime ./pkg/runtimebroker ./cmd
```

(The vitest timing for G0 is measured separately; `buildstats run -- npx vitest run ...` gives its wall time and peak RSS.)

### G1, G2, G3: pkg/hub test-binary compile (16G slot; needs a GO)

```sh
(ulimit -v 16000000
 env -u SCION_AUTO_EXPOSE_PORTS GOMAXPROCS=2 GOGC=40 GOFLAGS=-gcflags=-c=1 \
 /tmp/buildstats compile -label G1-hub-compile -dir /tmp/bs-hub \
   -bench-pkg github.com/GoogleCloudPlatform/scion/pkg/hub -json g1-hub-compile.json \
   -- go test -c -p 1 -vet=off -buildvcs=false -o /tmp/bs-hub/hub.test ./pkg/hub)
rm -f /tmp/bs-hub/hub.test
```

From G2 on, also measure each new subpackage split out of pkg/hub. Use the same command with `-bench-pkg` set to that package's import path and a non-hub label, then compare against the baseline:

```sh
/tmp/buildstats diff g1-hub-compile.json g2-hub-compile.json
```

`diff` prints a warning when the settings that change compile cost differ between the two records: `GOMAXPROCS`, `GOGC`, `GOMEMLIMIT`, `GOFLAGS`, the address-space limit, or the CPU count or quota.

### Small-package compile (no slot needed)

```sh
(ulimit -v 16000000
 env -u SCION_AUTO_EXPOSE_PORTS GOMAXPROCS=2 GOGC=40 GOFLAGS=-gcflags=-c=1 \
 /tmp/buildstats compile -label config-compile -dir /tmp/bs-config \
   -bench-pkg github.com/GoogleCloudPlatform/scion/pkg/config -json config-compile.json \
   -- go test -c -p 1 -vet=off -buildvcs=false -o /tmp/bs-config/config.test ./pkg/config)
```

### Test-slice timing

For a non-hub package (no GO needed):

```sh
(ulimit -v 16000000; ulimit -u 2048
 env -u SCION_AUTO_EXPOSE_PORTS GOMAXPROCS=2 \
 /tmp/buildstats test -label config-tests -top 10 -out config.t2j -json config-tests.json \
   -- go test -json -short -count=1 -timeout 10m ./pkg/config)
```

A pkg/hub slice runs an already-built `hub.test`, which needs a GO:

```sh
(cd pkg/hub && ulimit -v 16000000 && ulimit -u 2048 &&
 env -u SCION_AUTO_EXPOSE_PORTS GOMAXPROCS=2 \
 /tmp/buildstats test -label hub-slice-authz -out authz.t2j -json authz.json \
   -- go tool test2json -t -p hub /tmp/bs-hub/hub.test -test.v=test2json -test.count=1 \
      -test.timeout=15m -test.run '^(TestAuthz|TestAuthorize|TestAccessConstraint)')
```

## Re-summarising existing files

```sh
/tmp/buildstats actiongraph -top 20 /tmp/bs-hub/actiongraph.json
/tmp/buildstats bench /tmp/bs-hub/bench.txt
/tmp/buildstats tests -top 20 run1.t2j run2.t2j
```

Each of these also accepts `-json FILE`.

## JSON record

The top-level keys are: `schema` (currently 1), `kind`, `label`, `time`, `host`, `command`, `rusage`, `actiongraph`, `compiler_bench`, `tests`, `deps` and `artifacts`. A section that a subcommand does not produce is left out.
* `host` records the hostname, CPU count, cgroup `cpu.max`, `RLIMIT_AS`, git HEAD, and the relevant `GO*` environment variables.
* `artifacts` gives the paths of the raw actiongraph, bench and test2json files, so they can be summarised again later.
* `schema` is bumped whenever a field changes meaning or is removed.

## Tests

```sh
go test ./hack/buildstats
```

The unit tests use canned actiongraph, `-bench`, test2json and `go list` fixtures in `testdata/`, and do not run the go command.
