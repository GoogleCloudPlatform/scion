// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package runtime

import "context"

// sensitiveExecKey is the context key marking an Exec/ExecWithStdin call
// whose argv, stdout or stderr may carry caller-supplied content that must
// never reach logs or returned errors.
type sensitiveExecKey struct{}

// WithSensitiveExec marks ctx so that Exec/ExecWithStdin implementations
// suppress command output from debug logs and returned errors, instead of
// including it as they normally do for diagnostics.
//
// This exists for the agent-keys dedicated terminal-injection path
// (agent.Manager.SendKeys): the generated tmux command it delivers on stdin,
// and any stdout/stderr a failing runtime backend produces from it, can
// contain the exact bytes a caller asked to inject into a terminal. Broker-level
// redaction alone is not sufficient — pkg/runtime's own command-execution
// helpers already log CombinedOutput on failure (runSimpleCommand /
// runSimpleCommandWithStdin, common.go) and the Kubernetes backend embeds
// stderr directly in the error it returns (k8s_runtime.go's
// execWithOptionalStdin) — both before any broker-side sanitization ever
// sees the value. See .design/agent-keys-contract.md §5's "Audit and
// limits" redaction rule.
//
// This must not be used to suppress diagnostics for any other caller: every
// other Exec/ExecWithStdin call site relies on this output for real
// troubleshooting, so the marker is opt-in per call, not a global log-level
// change.
func WithSensitiveExec(ctx context.Context) context.Context {
	return context.WithValue(ctx, sensitiveExecKey{}, true)
}

// IsSensitiveExec reports whether ctx was marked by WithSensitiveExec.
func IsSensitiveExec(ctx context.Context) bool {
	v, _ := ctx.Value(sensitiveExecKey{}).(bool)
	return v
}
