# Scripts

Integration test scripts and developer utilities.

## Developer Utilities

- **`pr-deps.sh`** - Analyzes open PR dependency graphs using `gh` CLI. Supports branch dependency visualization (`graph`), topological merge order (`order`), and file overlap detection (`files`). Run `./scripts/pr-deps.sh --help` for usage.

## Integration Tests

- **`hub-env-integration-test.sh`** - Tests environment variable CRUD operations at user and project scopes.
- **`template-integration-test.sh`** - Tests template management operations via the Hub API.
