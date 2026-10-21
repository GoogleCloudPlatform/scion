# cr-ci workflow templates (planned, P5)

These are templates, **not** active workflows. GitHub only runs workflows under
`.github/workflows/`, and copying one there is a separate, reviewed change (P7).

Planned contents:

- `cr-runner-poc.yml`: a `workflow_dispatch` PoC job on
  `[self-hosted, Linux, X64, <RUNNER_LABEL>]` that runs `golangci-lint` and
  reports `/sys/fs/cgroup/memory.peak` and `df -h /scratch` (design section 7,
  phase 1).
- `runs-on-snippet.md`: the variable-routed `runs-on` expression for the `lint`
  job of `ci.yml`. It includes the fork guard and falls back to `ubuntu-latest`
  when the variable is unset:

  ```yaml
  runs-on: ${{ (github.event_name != 'pull_request' || github.event.pull_request.head.repo.full_name == github.repository) && vars.SCION_LARGE_RUNNER || 'ubuntu-latest' }}
  ```

  `SCION_LARGE_RUNNER` is the default of `ROUTING_VARIABLE_NAME` in
  `config.example.env`. Jobs that use `services:` containers or Docker must
  stay on GitHub-hosted runners (see [architecture](../docs/architecture.md)).
