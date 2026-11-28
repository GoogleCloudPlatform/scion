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

package hub

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// TokenBudgetAdmissionError is returned by checkTokenBudgetAdmission when a
// launch is refused due to an exhausted raw-token budget (429 budget_exhausted)
// or a project's usage.unmeasured_policy=deny setting (403 budget_unmeasured).
type TokenBudgetAdmissionError struct {
	HTTPStatus int
	Code       string
	Message    string
	Details    map[string]interface{}
}

func (e *TokenBudgetAdmissionError) Error() string {
	if e == nil {
		return ""
	}
	return e.Code + ": " + e.Message
}

// tokenBudgetsEnabled reports whether both tokenomics.usage_ledger and
// tokenomics.token_budgets are enabled on this Hub.
func (s *Server) tokenBudgetsEnabled() bool {
	if s == nil {
		return false
	}
	return s.experimentEnabled(experiments.TokenomicsUsageLedger) &&
		s.experimentEnabled(experiments.TokenomicsTokenBudgets)
}

// resolveAdmissionBillingUserID resolves the billing user for admission using
// the exact same store resolver (resolveBillingUserIDTx) that populates
// run_attribution.billing_user_id, ensuring attribution and admission never
// disagree. Principal-less agents return "".
func (s *Server) resolveAdmissionBillingUserID(ctx context.Context, agent *store.Agent) (string, error) {
	if s == nil || s.store == nil || agent == nil {
		return "", nil
	}
	uid, err := s.store.ResolveBillingUserID(ctx, agent.OwnerID, agent.CreatedBy, agent.Ancestry)
	if err != nil {
		return "", fmt.Errorf("resolve billing user for admission: %w", err)
	}
	if uid == nil {
		return "", nil
	}
	return *uid, nil
}

// resolveAgentHarnessCoverage resolves the agent's harness config usage.yaml
// live capability and maps it to the P2 measurement_status vocabulary
// ("unknown" when usage.yaml is absent/unparsable, distinct from explicit
// "unmeasured" / source: none).
func (s *Server) resolveAgentHarnessCoverage(ctx context.Context, agent *store.Agent) (harnessConfig, coverageStatus string, capKind usageCapabilityKind) {
	if agent == nil {
		return "", "unknown", usageCapUnknown
	}
	harnessConfig = strings.TrimSpace(agent.HarnessConfig)
	if harnessConfig == "" && agent.AppliedConfig != nil {
		harnessConfig = strings.TrimSpace(agent.AppliedConfig.HarnessConfig)
	}
	memo := newUsageCapabilityMemo(ctx, s)
	usageCap := memo.resolve(agent.ProjectID, harnessConfig)
	capKind = classifyUsageLiveCapability(usageCap)
	switch capKind {
	case usageCapFull:
		coverageStatus = MeasurementStatusMeasured
	case usageCapPartial:
		coverageStatus = MeasurementStatusPartial
	case usageCapCallsOnly:
		coverageStatus = MeasurementStatusCallsOnly
	case usageCapUnmeasured:
		coverageStatus = MeasurementStatusUnmeasured
	default:
		coverageStatus = "unknown"
	}
	return harnessConfig, coverageStatus, capKind
}

// checkTokenBudgetAdmission enforces raw-token budget admission and
// usage.unmeasured_policy for any launch entry point (create, start, resume,
// restart, wake, scheduled/trigger launch, reincarnate).
//
// No-op when either tokenomics.token_budgets or tokenomics.usage_ledger is off.
// Shell execution containers (isShellAgent) consume no LLM tokens and are
// exempt from both budget_exhausted and budget_unmeasured (D8).
// Never blocks stop, suspend, delete, or message delivery to running agents.
func (s *Server) checkTokenBudgetAdmission(ctx context.Context, agent *store.Agent, project *store.Project) error {
	if !s.tokenBudgetsEnabled() || agent == nil || agent.ProjectID == "" {
		return nil
	}
	if isShellAgent(agent) {
		return nil
	}
	eval := s.TokenBudgetEvaluator()
	if eval == nil {
		return nil
	}

	billingUserID, err := s.resolveAdmissionBillingUserID(ctx, agent)
	if err != nil {
		return err
	}

	// Pass zero time.Time{} so IsScopeExhaustedForAction uses the evaluator's clock.
	status, err := eval.IsScopeExhaustedForAction(
		ctx,
		agent.ProjectID,
		billingUserID,
		time.Time{},
		store.TokenActionPauseNew,
		store.TokenActionSuspendAndPause,
	)
	if err != nil {
		return err
	}
	if status == nil {
		return nil
	}

	// Check project usage.unmeasured_policy=deny only when not already
	// exhausted and an active non-zero token cap applies (reusing
	// status.HasActiveCap from the evaluation pass above).
	// A stale meter alone never blocks admission.
	needUnmeasuredCheck := !status.Exhausted && status.HasActiveCap
	if needUnmeasuredCheck {
		if project == nil && s.store != nil {
			if p, perr := s.store.GetProject(ctx, agent.ProjectID); perr == nil {
				project = p
			}
		}
		needUnmeasuredCheck = project != nil && project.Annotations[projectSettingUsageUnmeasuredPolicy] == UsageUnmeasuredPolicyDeny
	}
	if !status.Exhausted && !needUnmeasuredCheck {
		return nil
	}

	harnessConfig, coverageStatus, capKind := s.resolveAgentHarnessCoverage(ctx, agent)
	if status.Exhausted {
		return &TokenBudgetAdmissionError{
			HTTPStatus: http.StatusTooManyRequests,
			Code:       api.LaunchCodeBudgetExhausted,
			Message: fmt.Sprintf("the token budget has been exhausted (%s: %d of %d tokens used)",
				status.LimitName, status.Consumed, status.Cap),
			Details: map[string]interface{}{
				"limit":          status.LimitName,
				"scopeKind":      status.ScopeKind,
				"scopeId":        status.ScopeID,
				"consumed":       status.Consumed,
				"max":            status.Cap,
				"windowEnd":      status.WindowEndUTC.UTC().Format(time.RFC3339),
				"coverageStatus": coverageStatus,
			},
		}
	}
	if capKind == usageCapUnmeasured || capKind == usageCapUnknown || capKind == usageCapCallsOnly {
		return &TokenBudgetAdmissionError{
			HTTPStatus: http.StatusForbidden,
			Code:       api.LaunchCodeBudgetUnmeasured,
			Message:    "the harness is not measured for token usage under this project's policy",
			Details: map[string]interface{}{
				"policy":         UsageUnmeasuredPolicyDeny,
				"harnessConfig":  harnessConfig,
				"coverageStatus": coverageStatus,
			},
		}
	}

	return nil
}

// writeTokenBudgetAdmissionError writes a TokenBudgetAdmissionError as an HTTP
// JSON error response and reports whether err was handled.
func writeTokenBudgetAdmissionError(w http.ResponseWriter, err error) bool {
	var budgetErr *TokenBudgetAdmissionError
	if !errors.As(err, &budgetErr) {
		return false
	}
	writeError(w, budgetErr.HTTPStatus, budgetErr.Code, budgetErr.Message, budgetErr.Details)
	return true
}

// tokenBudgetAdmissionDMError maps a TokenBudgetAdmissionError to an
// *AgentDMError for DM wake / auto-wake paths, or returns nil.
func tokenBudgetAdmissionDMError(err error) *AgentDMError {
	var budgetErr *TokenBudgetAdmissionError
	if !errors.As(err, &budgetErr) {
		return nil
	}
	return &AgentDMError{
		Code:       budgetErr.Code,
		Message:    budgetErr.Message,
		HTTPStatus: budgetErr.HTTPStatus,
		Details:    budgetErr.Details,
	}
}
