# ateapipb (vendored)

Generated Go client stubs for Substrate's `substrate.ateapi.v1.Control` gRPC
service (module `go.ate.dev/substrate`, `api/ateapi/v1`). The Go package
clause is `ateapi`; importers use the alias
`ateapipb "github.com/GoogleCloudPlatform/scion/third_party/ateapipb"`.

## Why vendored instead of `go get`

`internal/ateclient` (the dialer helper substrate ships) is `internal/`, so
scion re-implements it (`pkg/runtime/substrate`), but the generated
`ateapipb` package itself is what we need as a dependency.

`go get go.ate.dev/substrate/api/ateapi/v1@v1.0.0` pulls
in substrate's full `go.mod`, which requires **Go 1.27.0** (scion targets
1.26.11) and forces upgrades of `k8s.io/api` (v0.37),
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

- Module: `go.ate.dev/substrate`
- Tag: `v1.0.0` (commit `b34fbee742906494b5eb4f1f36987176bf8770a4`,
  2026-11-05; the API is identical in `v1.0.1`)
- Path: `api/ateapi/v1/`
- License: Apache License 2.0 (`LICENSE`, copied from the source repo)

Files are copied verbatim (`ateapi.proto`, `ateapi.pb.go`,
`ateapi_grpc.pb.go`, `source.go`) and are not regenerated locally — do not
hand-edit the generated files. To refresh: re-copy from a newer substrate
tag and update the tag and SHA above:

```sh
for f in ateapi.proto ateapi.pb.go ateapi_grpc.pb.go source.go; do
  git -C <substrate checkout> show <tag>:api/ateapi/v1/$f > third_party/ateapipb/$f
done
```
