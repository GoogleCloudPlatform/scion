# Kubernetes keeps the runtime's start command

**Date:** 2026-11-09
**Follows:** the env-precedence change (ptone/scion#1956)

## Change

`buildPod` in `pkg/runtime/k8s_runtime.go` skips `SCION_START_CMD` in the merged environment
(`config.Env`), as it does for environment-type secrets. Neither source replaces the runtime's
start command, so for them the pod's gate script runs the runtime's tmux start command, as on
Docker. A merged-environment value that references the name itself (`$(SCION_START_CMD) ...`) is
dropped too.

The name is held in one unexported constant, `k8sStartCmdEnv`, in `k8s_runtime.go`. It is used for
the runtime's entry, the secret skip set and the new skip. The Docker path in `common.go` keeps its
literals, because this change does not touch the other runtimes.

## Tests and docs

- `TestBuildPod_StartCmdWinsOverConfigEnv` (`pkg/runtime/k8s_parity_test.go`): a plain value and a
  self-reference in `config.Env` both yield exactly one `SCION_START_CMD` entry, holding the
  runtime's start command, while a neutral `config.Env` entry is kept with its value.
- The settings-precedence reference, in its section on the sources the runtime adds around the merged
  environment, lists `SCION_START_CMD` among the Kubernetes runtime keys that keep the runtime's value.
