# Hack

Developer convenience scripts for local development, testing, and infrastructure provisioning.

## Contents

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

These scripts are for development and operations -- not end-user tooling.
