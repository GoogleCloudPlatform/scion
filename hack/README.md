# Hack

Developer convenience scripts for local development, testing, and infrastructure provisioning.

## Contents

### Scripts

| Script | Purpose |
|--------|---------|
| `setup.sh` | Set up an isolated test environment |
| `cleanup.sh` | Clean up test artifacts |
| `smoke_test.sh` | Run basic functionality smoke tests |
| `test_auth.sh` / `test_oauth.sh` | Test authentication flows |
| `run-claude.sh` | Run the Claude harness locally |
| ~~`gce-demo-*.sh`~~ | Moved to [`scripts/starter-hub/`](../scripts/starter-hub/) |
| `create-cluster.sh` | Create a Kubernetes cluster |
| `merge-work.sh` | Merge agent work branches |
| `version.sh` | Display version information |

### Go Tools

| Tool | Purpose |
|------|---------|
| `go run ./hack/apitest` | Stress tests API-level multi-hub integration against shared Postgres DB |
| `go run ./hack/dbdiag` | Diagnoses database connection pool usage and active advisory locks |
| `go run ./hack/pkgmove` | Generates behaviour-preserving package moves (git mv, export renames, alias file, safety report); see [`pkgmove/README.md`](pkgmove/README.md) |
| `go run ./hack/minttoken` | Mints a long-lived user access-token JWT for local API integration testing |
| `go run ./hack/buildstats` | Measures compile/test cost (wall, user, peak RSS, actiongraph, compiler phases, test2json, dep counts) for refactor gates; see [`buildstats/README.md`](buildstats/README.md) |
| `go run ./hack/hubshard/extract` | Moves shared pkg/hub test helpers verbatim into `*_helpers_test.go` files and verifies the move (test sharding prep); see [`hubshard/extract/README.md`](hubshard/extract/README.md) |

### Kubernetes Test Manifests

| Manifests | Purpose |
|-----------|---------|
| `k8s-nfs/` | Pod and PV configurations for testing GKE NFS shared workspace mount scenarios |

These scripts and tools are for development and operations -- not end-user tooling.
