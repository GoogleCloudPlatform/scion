package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/GoogleCloudPlatform/scion/cr-ci/mediator/internal/githubapp"
	"github.com/GoogleCloudPlatform/scion/cr-ci/mediator/internal/logging"
)

type webhookPayload struct {
	Action      string `json:"action"`
	WorkflowJob struct {
		ID         int64    `json:"id"`
		RunID      int64    `json:"run_id"`
		Labels     []string `json:"labels"`
		Conclusion string   `json:"conclusion"`
		RunnerName string   `json:"runner_name"`
	} `json:"workflow_job"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
}

var errSecretUnavailable = errors.New("webhook secret unavailable")

func (s *Server) handleWebhook(w http.ResponseWriter, r *http.Request) {
	delivery := r.Header.Get("X-GitHub-Delivery")
	event := r.Header.Get("X-GitHub-Event")
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxWebhookBody))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			s.log().Warn("webhook body too large", "event", logging.EventWebhookRejected, "delivery", delivery, "reason", "body_too_large")
			plain(w, http.StatusRequestEntityTooLarge, "body too large")
			return
		}
		s.log().Warn("webhook body unreadable", "event", logging.EventWebhookRejected, "delivery", delivery, "reason", "body_unreadable")
		plain(w, http.StatusBadRequest, "body unreadable")
		return
	}
	// Webhook work must not die with the delivery (GitHub gives up after
	// 10 s): a cancel between mint and launch would orphan a registration.
	ctx, cancel := detached(r.Context(), s.o.RequestTimeout)
	defer cancel()

	ok, err := s.verifySignature(ctx, body, r.Header.Get("X-Hub-Signature-256"))
	if err != nil {
		s.log().Error("webhook secret unavailable", "event", logging.EventError, "delivery", delivery, "error", err.Error())
		plain(w, http.StatusServiceUnavailable, "unavailable")
		return
	}
	if !ok {
		s.log().Warn("webhook signature mismatch", "event", logging.EventWebhookRejected, "delivery", delivery, "reason", "signature")
		plain(w, http.StatusUnauthorized, "bad signature")
		return
	}
	if event != "workflow_job" {
		s.log().Info("webhook ignored", "event", logging.EventWebhookIgnored, "delivery", delivery, "github_event", event, "reason", "event_type")
		plain(w, http.StatusOK, "ignored")
		return
	}
	var p webhookPayload
	if err := json.Unmarshal(body, &p); err != nil || p.WorkflowJob.ID == 0 {
		s.log().Warn("webhook payload malformed", "event", logging.EventWebhookRejected, "delivery", delivery, "reason", "payload")
		plain(w, http.StatusBadRequest, "malformed payload")
		return
	}
	job := p.WorkflowJob
	attrs := []any{"delivery", delivery, "action", p.Action, "job_id", job.ID, "run_id", job.RunID, "repo", p.Repository.FullName}
	if job.RunnerName != "" {
		attrs = append(attrs, "runner", job.RunnerName)
	}
	notConfigured := func() bool {
		if s.o.MediatorURL != "" {
			return false
		}
		s.log().Error("MEDIATOR_URL is not set; cannot launch runners", append(attrs, "event", logging.EventError, "reason", "not_configured")...)
		plain(w, http.StatusServiceUnavailable, "not configured")
		return true
	}

	// A job running (or finished) on a cr-ci runner is checked by runner
	// name, never by labels: whatever its labels, it ran on our runner.
	if _, ours := parseRunnerName(job.RunnerName); ours && (p.Action == "in_progress" || p.Action == "completed") {
		if notConfigured() {
			return
		}
		result, err := s.checkRunnerJob(ctx, p.Repository.FullName, job.ID, job.RunID, job.RunnerName, nil, "webhook")
		if p.Action == "completed" {
			// The ephemeral runner is gone with the job.
			s.forgetRunner(job.RunnerName)
		}
		if err != nil {
			plain(w, http.StatusServiceUnavailable, "runner check failed")
			return
		}
		plain(w, http.StatusOK, result)
		return
	}

	if !hasLabel(job.Labels, s.o.RunnerLabel) {
		s.log().Debug("webhook ignored", append(attrs, "event", logging.EventWebhookIgnored, "reason", "label")...)
		plain(w, http.StatusOK, "ignored")
		return
	}
	if !s.servable(job.Labels) {
		s.log().Warn("job labels cannot be served by a cr-ci runner (it carries RUNNER_LABEL only)",
			append(attrs, "event", logging.EventWebhookIgnored, "reason", "labels_unservable", "labels", strings.Join(job.Labels, ","))...)
		plain(w, http.StatusOK, "ignored")
		return
	}
	repo, ok := s.targetRepo(p.Repository.FullName)
	if !ok {
		s.log().Warn("webhook rejected", append(attrs, "event", logging.EventWebhookRejected, "reason", "repo_not_allowed")...)
		plain(w, http.StatusOK, "ignored")
		return
	}
	if notConfigured() {
		return
	}

	switch p.Action {
	case "queued":
		s.onQueued(w, ctx, repo, p, attrs)
	case "completed":
		// Finished without running on a cr-ci runner (cancelled or skipped
		// while queued): stop the runner launched for it, if it is idle.
		if err := s.stopUnusedRunners(ctx, repo, job.ID, attrs); err != nil {
			plain(w, http.StatusServiceUnavailable, "cleanup failed")
			return
		}
		plain(w, http.StatusOK, "ok")
	default:
		plain(w, http.StatusOK, "ignored")
	}
}

func (s *Server) onQueued(w http.ResponseWriter, ctx context.Context, repo githubapp.Repo, p webhookPayload, attrs []any) {
	// Fork guard from the API, not the payload.
	run, err := s.o.GitHub.GetRun(ctx, repo, p.WorkflowJob.RunID)
	if err != nil {
		s.log().Error("fetching workflow run failed", append(attrs, "event", logging.EventError, "error", err.Error())...)
		plain(w, http.StatusServiceUnavailable, "github unavailable")
		return
	}
	if reason := forkReason(run, repo); reason != "" {
		s.log().Warn("refusing to launch a runner for a fork or privileged-trigger run",
			append(attrs, "event", logging.EventRejectFork, "reason", reason, "run_event", run.Event, "head_repository", headRepo(run))...)
		plain(w, http.StatusOK, "rejected")
		return
	}
	out, err := s.dispatch(ctx, repo, p.WorkflowJob.ID, p.WorkflowJob.RunID, "webhook")
	if err != nil {
		plain(w, http.StatusServiceUnavailable, "dispatch failed")
		return
	}
	plain(w, http.StatusOK, string(out))
}

// verifySignature checks X-Hub-Signature-256 in constant time.
//
// Rotation: on a mismatch the secret is re-read (at most once a minute while
// one is cached, every 10 s while none is), and a replaced secret stays
// valid for an hour, so neither the order of the two rotation steps nor a
// refetch triggered by a forged request rejects genuine deliveries.
func (s *Server) verifySignature(ctx context.Context, body []byte, header string) (bool, error) {
	sigHex, ok := strings.CutPrefix(header, "sha256=")
	if !ok {
		return false, nil
	}
	sig, err := hex.DecodeString(sigHex)
	if err != nil || len(sig) != sha256.Size {
		return false, nil
	}
	check := func(secret []byte) bool {
		m := hmac.New(sha256.New, secret)
		m.Write(body)
		return hmac.Equal(m.Sum(nil), sig)
	}
	// Fast path: compare against the cached secrets without waiting for a
	// refetch (which any sender of a bad signature can trigger once a minute).
	s.secretMu.Lock()
	cur, prev, prevUntil := s.secret, s.prevSecret, s.prevUntil
	s.secretMu.Unlock()
	if cur != nil && check(cur) {
		return true, nil
	}
	if prev != nil && s.o.Now().Before(prevUntil) && check(prev) {
		return true, nil
	}

	// Slow path: refetch, single-flight, without holding secretMu.
	s.secretFetchMu.Lock()
	defer s.secretFetchMu.Unlock()
	s.secretMu.Lock()
	now := s.o.Now()
	if s.secret != nil && !hmac.Equal(s.secret, cur) && check(s.secret) {
		s.secretMu.Unlock()
		return true, nil // another request refetched it meanwhile
	}
	backoff := secretRefetchBackoff
	if s.secret == nil {
		backoff = secretRetryBackoff
	}
	if !s.secretAttempt.IsZero() && now.Sub(s.secretAttempt) < backoff {
		empty := s.secret == nil
		s.secretMu.Unlock()
		if empty {
			return false, errSecretUnavailable
		}
		return false, nil
	}
	s.secretAttempt = now
	s.secretMu.Unlock()

	secret, err := s.o.WebhookSecret(ctx)

	s.secretMu.Lock()
	defer s.secretMu.Unlock()
	if err != nil {
		if s.secret != nil {
			return false, nil
		}
		return false, err
	}
	if s.secret != nil && !hmac.Equal(secret, s.secret) {
		s.prevSecret, s.prevUntil = s.secret, now.Add(secretGrace)
	}
	s.secret = secret
	return check(secret), nil
}
