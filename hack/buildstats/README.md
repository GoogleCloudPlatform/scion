# buildstats

`hack/buildstats` measures Go compile and test cost **the same way every time**, so before/after numbers for a refactor are comparable. Every refactor gate uses it. It is stdlib-only Go and is not part of the product.

Each run prints a human-readable table (to stderr for the measuring subcommands) and can write a JSON record (`-json FILE`, or `-json -` for stdout) that `buildstats diff` compares.

## What it measures

| Measurement | How |
|---|---|
| Wall, user, sys time and **peak RSS** of a `go build` / `go test -c` (or any command) | buildstats starts the command as its **own direct child** and reads the child's rusage from `wait4`. `ru_maxrss` includes every descendant the child waited for, so for `go` it is the RSS of the largest single process, normally the biggest compile or the link. This does not need `/usr/bin/time`. The cgroup `memory.peak` is also read before and after the run, **for context only**: it covers the whole cgroup, including page cache, the agent harness and anything else in the container. It only goes up (older kernels cannot reset it) and it is **not comparable to RSS**. **Never use it for gates**; gates use the peak RSS. |
| Per-action timings | `compile` adds `-debug-actiongraph=DIR/actiongraph.json`. The summary lists the top actions by wall time (with each compile's user and sys time), plus the summed build and link time. |
| Compiler phase timings | `compile -bench-pkg PKG` adds `-gcflags=PKG=<inherited flags> -bench=DIR/bench-<unixnano>.txt`. The file name is unique per run, which changes the package's action ID, so **the measured package is always recompiled**. With a fixed name, a repeat run in the same `-dir` would be a cache hit with nothing measured. Unlike `-cpuprofile`, which the test-main compile overwrites, `-bench` **appends** one block per compiler invocation. You therefore get separate records for the package, its external `_test` package and `main` (the generated test main).  |
| Test-slice timing | `test` captures test2json output from `go test -json` or `go tool test2json`. It reports, per package: the result, elapsed time, top-level counts (pass/fail/skip/**incomplete**), number of subtests, the sum of test times, tests over 1s, the share of the top 20, and the N slowest tests. A test that started but never reported pass, fail or skip (a timeout or kill) is listed as `incomplete`, timed from its start to the package's last event. With `-count>1`, each test's runs are summed (`runs`) and the worst result is kept. |
| Normalised peak and compile time | When `-bench` data is present, `compile` divides the run's peak RSS and the package's `-bench` total by the measured package's size: lines from `fe:parse` and funcs from `be:compilefuncs`. The results are reported per 100k lines and per 10k funcs (the `normalized` section), so a package that grows between gates does not hide a real improvement. |
| Dependency counts | `deps [-test] PKG...` makes one `go list -deps -json` call and reports total and non-std counts per package. The package itself is excluded, so the total equals `go list -f '{{len .Deps}}'`. With `-test` the count is the test binary's closure, leaving out the package's own external test package (`PKG_test`) and the generated test main. |

### How `-gcflags` is handled

For each package, `cmd/go` applies only the **last** `-gcflags` whose pattern matches: first the ones from `GOFLAGS`, then the ones on the command line. Values are not combined. So that the measured compile uses the same compiler flags as an unmeasured one, `compile -bench-pkg PKG` builds its own `-gcflags` like this:

* It copies the flags that would otherwise apply to `PKG`: the last matching value from `GOFLAGS` or the command line. An `all=` or `PKG=` value (a `/...` pattern also counts) always matches. An unpatterned value matches **only if `PKG` is one of the positional package arguments**, because `cmd/go` applies unpatterned flags only to the packages named on the command line. A relative argument (`./pkg/hub`, `./pkg/hub/...`) is resolved to an import path through the nearest `go.mod`, without running go. Example: `GOFLAGS=-gcflags=-c=1` with `./pkg/hub` named becomes `PKG=-c=1 -bench=…`.
* It places its `-gcflags` after every `-gcflags` already on the command line.

`-bench-pkg` is normally the full import path. Paths containing whitespace are rejected (both `-bench-pkg` and the `-dir` holding the bench file), because they would split the `-gcflags` value. The final command is printed (shell-quoted) and stored in the record.

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

Every compile measurement uses one of the two allowed memory-capped forms. Both use `-p 1`, `GOMAXPROCS=2`, `GOFLAGS=-gcflags=-c=1` and `ulimit -v 16000000`:

| Form | Settings | Use |
|---|---|---|
| **Default** | `GOMEMLIMIT=6GiB GOGC=40` | Gate measurements (G1-G3) and anything else, unless told otherwise |
| **Trial** (P0-8) | `GOGC=25`, with `GOMEMLIMIT` unset | Only when the trial has been explicitly granted for that run |

Never mix forms within one comparison. `host.form` records the form, and `diff` warns when two records differ. The commands below use the default form.

**Slots:** any step that compiles **pkg/hub or ./cmd** (build, test, vet or type-check) is a 16G run. Announce it and wait for the coordinator's GO, as the project's resource rules require. Running a built test binary also needs a GO. On a host that queues go commands (wl), every go command below, including `go list` and the warm-up, goes through the queue tool, one command per call.

### Warm the dependency cache first

To compare like with like, warm the dependency cache first, so the measured run compiles only the package under study. The `-bench` flag changes the package's action ID, so the measured package is always recompiled even when the cache is warm.

The warm-up compiles every **non-standard-library dependency of the pkg/hub test binary, but not pkg/hub itself**, so it needs no 16G slot (for a ./cmd measurement, list ./cmd's deps the same way). It is still a sizeable compile, about 7 minutes and 2.6 GiB peak on a 2-CPU host when the cache is cold:

```sh
go list -deps -test -f '{{if not .Standard}}{{.ImportPath}}{{end}}' ./pkg/hub \
  | grep -v ' \[' | grep -v '\.test$' | grep -vx 'github.com/GoogleCloudPlatform/scion/pkg/hub' > /tmp/hubdeps.txt
(ulimit -v 16000000; GOMAXPROCS=2 GOMEMLIMIT=6GiB GOGC=40 GOFLAGS= go build -p 1 -buildvcs=false $(cat /tmp/hubdeps.txt))
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
 env -u SCION_AUTO_EXPOSE_PORTS GOMAXPROCS=2 GOMEMLIMIT=6GiB GOGC=40 GOFLAGS=-gcflags=-c=1 \
 /tmp/buildstats compile -label G1-hub-compile -dir /tmp/bs-hub \
   -bench-pkg github.com/GoogleCloudPlatform/scion/pkg/hub -json g1-hub-compile.json \
   -- go test -c -p 1 -vet=off -buildvcs=false -o /tmp/bs-hub/hub.test ./pkg/hub)
rm -f /tmp/bs-hub/hub.test
```

Compare two runs (for example before and after a set of moves, measured as a pair in the same form) with:

```sh
/tmp/buildstats diff g1-before.json g1-after.json
```

From G2 on, also measure each new subpackage split out of pkg/hub. **Name the subpackage on the command line** and set `-bench-pkg` to its import path. The inherited `GOFLAGS=-gcflags=-c=1` then applies to it, just as it applies to pkg/hub in the run above. If you only name pkg/hub, the subpackage is just a dependency and the unpatterned flag does not reach it. This command does not compile pkg/hub itself unless the subpackage's tests import it, but ask the coordinator whether it needs a 16G slot:

```sh
(ulimit -v 16000000
 env -u SCION_AUTO_EXPOSE_PORTS GOMAXPROCS=2 GOMEMLIMIT=6GiB GOGC=40 GOFLAGS=-gcflags=-c=1 \
 /tmp/buildstats compile -label G2-hub-webchat-compile -dir /tmp/bs-webchat \
   -bench-pkg github.com/GoogleCloudPlatform/scion/pkg/hub/webchat -json g2-webchat-compile.json \
   -- go test -c -p 1 -vet=off -buildvcs=false -o /tmp/bs-webchat/webchat.test ./pkg/hub/webchat)
```

`diff` warns when the two records differ in any of these ways:
* the invocation, after normalising the injected actiongraph and `-bench` paths and the `-o` path
* a non-zero exit code on either side
* the normalised package or the set of `-bench` packages
* the measured go toolchain
* `GOMAXPROCS`, `GOGC`, `GOMEMLIMIT` or `GOFLAGS`
* the address-space limit
* the CPU count or quota

### Small-package compile (no slot needed)

```sh
(ulimit -v 16000000
 env -u SCION_AUTO_EXPOSE_PORTS GOMAXPROCS=2 GOMEMLIMIT=6GiB GOGC=40 GOFLAGS=-gcflags=-c=1 \
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
/tmp/buildstats bench "$(jq -r .artifacts.bench g1-hub-compile.json)"   # bench accepts several files too
/tmp/buildstats tests -top 20 run1.t2j run2.t2j
```

Each of these also accepts `-json FILE`.

## JSON record

The top-level keys are: `schema` (currently 1), `kind`, `label`, `time`, `host`, `command`, `rusage`, `actiongraph`, `compiler_bench`, `tests`, `deps`, `normalized` and `artifacts`. A section that a subcommand does not produce is left out.
* `host` records the hostname, CPU count, cgroup `cpu.max`, `RLIMIT_AS`, git HEAD, and the relevant `GO*` environment variables.
* `host.form` is always present. It records `GOMAXPROCS`, `GOGC`, `GOMEMLIMIT`, `GOFLAGS` (each `"unset"` when not set) and `rlimit_as`. Check it before comparing two gate records; `diff` warns when any of these differ.
* `normalized` holds the size-normalised figures described above.
* `host.go_toolchain` is the version of the go toolchain that ran the measured command. It comes from the compiler's `-bench` `commit:` line when there is one, otherwise from the go binary's `GOROOT/VERSION`; buildstats never runs go just to ask. `buildstats_go_version` is only the version that built buildstats. `diff` warns when the toolchains differ.
* Records written before `host.form` existed get it filled in from `host.env` when they are read (an absent variable means it was unset).
* `artifacts` gives the paths of the raw actiongraph, bench and test2json files, so they can be summarised again later.
* `schema` is bumped whenever a field changes meaning or is removed.

## Tests

```sh
go test ./hack/buildstats
```

The unit tests use canned actiongraph, `-bench`, test2json and `go list` fixtures in `testdata/`, and do not run the go command. A few tests use `sh`, `cat` and `awk`. One of them checks that the peak RSS includes a grandchild process, and it is skipped when these tools are missing.
