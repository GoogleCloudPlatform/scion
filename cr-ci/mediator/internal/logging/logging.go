// Package logging builds the mediator's structured logger: one JSON object
// per line on stdout, with the field names Cloud Logging recognises
// ("severity", "message", "time").
//
// Every log line carries an "event" attribute with a stable name (see the
// Event* constants) so that log-based metrics and alerts can match on it.
// Callers must never pass secret material (webhook secret, App private key,
// JWTs, installation tokens, JIT configs, claim tokens, OIDC tokens) as an
// attribute; a test in the server package enforces this end to end.
package logging

import (
	"io"
	"log/slog"
)

// Event names. They are part of the operational contract documented in
// cr-ci/docs/operations.md; rename them only together with that page.
const (
	EventStartup         = "startup"
	EventWebhookIgnored  = "webhook_ignored"
	EventWebhookRejected = "webhook_rejected"
	EventRejectFork      = "SECURITY_REJECT_FORK_PR"
	EventKillHijacked    = "SECURITY_KILL_HIJACKED_RUNNER"
	EventRunnerSwap      = "runner_swap"
	// EventRunnerSwapLowBudget (WARNING) follows runner_swap when the job that
	// took the runner inherits less than 20 minutes of the task timeout.
	EventRunnerSwapLowBudget = "runner_swap_low_budget"
	// EventRunnerRebind (INFO) follows runner_swap when the idle runner launched for
	// the job that took the runner is handed the displaced job instead; in a chain of
	// swaps the displaced job is passed on runner by runner, one rebind per hand-off.
	EventRunnerRebind        = "runner_rebind"
	EventDispatch            = "dispatch"
	EventDispatchSkip        = "dispatch_skip"
	EventCapacityFull        = "capacity_full"
	EventLaunchFailed        = "launch_failed"
	EventCancel              = "cancel"
	EventClaimConsumed       = "claim_consumed"
	EventClaimRejected       = "claim_rejected"
	EventReconcile           = "reconcile"
	EventReconcileDenied     = "reconcile_denied"
	EventReconcileSkip       = "reconcile_skip"
	EventReconcileAllSkipped = "reconcile_all_skipped"
	EventRunnerReaped        = "runner_reaped"
	EventExecutionReaped     = "execution_reaped"
	EventError               = "error"
)

// New returns a JSON logger writing to w with Cloud Logging field names.
func New(w io.Writer, level slog.Leveler) *slog.Logger {
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{
		Level: level,
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if len(groups) > 0 {
				return a
			}
			switch a.Key {
			case slog.LevelKey:
				a.Key = "severity"
				if lvl, ok := a.Value.Any().(slog.Level); ok {
					a.Value = slog.StringValue(severity(lvl))
				}
			case slog.MessageKey:
				a.Key = "message"
			}
			return a
		},
	}))
}

func severity(l slog.Level) string {
	switch {
	case l >= slog.LevelError:
		return "ERROR"
	case l >= slog.LevelWarn:
		return "WARNING"
	case l >= slog.LevelInfo:
		return "INFO"
	default:
		return "DEBUG"
	}
}
