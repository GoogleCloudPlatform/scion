# ateapipb (vendored)

Generated Go client stubs for Substrate's `ateapi.Control` gRPC service
(github.com/agent-substrate/substrate, `pkg/proto/ateapipb`).

## Why vendored instead of `go get`

`internal/ateclient` (the dialer helper substrate ships) is `internal/`, so
scion re-implements it (`pkg/runtime/substrate`), but the generated
`ateapipb` package itself is what we need as a dependency.

`go get github.com/agent-substrate/substrate/pkg/proto/ateapipb@<sha>` pulls
in substrate's full `go.mod`, which requires **Go 1.27.0** (scion targets
1.26.1) and forces major-version upgrades of `k8s.io/api`,
`k8s.io/apimachinery`, `k8s.io/client-go`, and a dozen other shared
transitive deps scion already pins — a build-tooling-wide change out of
scope for the substrate runtime slice (substrate-runtime.md §1, "if that module
pulls in unreasonable transitive dependencies, copy the generated
`ateapipb` package under `third_party/`").

The generated package itself has exactly two dependencies:
`google.golang.org/protobuf` and `google.golang.org/grpc`, both already
present in scion's `go.mod` at compatible versions (the vendored files were
generated with `protoc-gen-go v1.36.11-devel`, matching scion's
`google.golang.org/protobuf v1.36.11`).

## Provenance

- Source: https://github.com/agent-substrate/substrate
- Commit: `d277088bc1d081ef716d81dd7986d05d0a36ad3a` (2026-09-22)
- Path: `pkg/proto/ateapipb/`
- License: Apache License 2.0 (`LICENSE`, copied from the source repo)

Files are copied verbatim (`ateapi.proto`, `ateapi.pb.go`,
`ateapi_grpc.pb.go`, `source.go`) and are not regenerated locally — do not
hand-edit the generated files. To refresh: re-copy from a newer substrate
commit and update the SHA above.
