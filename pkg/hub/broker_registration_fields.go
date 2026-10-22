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
	"sort"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Field ownership on registration of an existing Runtime Broker:
//
//   - The GCP host service account and project are written together. They
//     are set at creation, or through the update API. A registration fills
//     them only while both are unset and they were not set or cleared
//     through the update API (store.SetRuntimeBrokerGCPHostIfUnset). An
//     empty request value never clears them. A fill needs actAs for the
//     registering user on the requested pair (registrationFillsGCPHost,
//     registrationFillAuthorized); a request that would not be written is
//     kept without a check.
//   - A registration adds labels whose key is absent and keeps the stored
//     value of existing keys. It never removes a key.
//   - A non-empty requested value that is not applied is reported in the
//     response (KeptSetting) and in the register audit event.

// KeptSetting is a value a registration request carried that was not
// applied because the stored value is kept.
type KeptSetting struct {
	// Field is gcpHostServiceAccountEmail, gcpHostProjectId or labels.<key>.
	Field          string `json:"field"`
	StoredValue    string `json:"storedValue"`
	RequestedValue string `json:"requestedValue"`
	// Reason is KeptReasonSet; KeptReasonPinned when the GCP host pair was
	// cleared through the update API; or KeptReasonEmailRequired for a
	// project sent without a service account.
	Reason string `json:"reason"`
}

const (
	// KeptReasonSet: the stored value is set and is kept.
	KeptReasonSet = "set"
	// KeptReasonPinned: the stored value was cleared through the update API
	// and stays cleared until the next update.
	KeptReasonPinned = "pinned"
	// KeptReasonEmailRequired: a GCP host project without a service
	// account is not applied; the pair is filled only with both.
	KeptReasonEmailRequired = "email_required"
)

const (
	keptFieldGCPHostEmail   = "gcpHostServiceAccountEmail"
	keptFieldGCPHostProject = "gcpHostProjectId"
	keptFieldLabelPrefix    = "labels."
)

// mergeRegistrationLabels adds the requested labels whose key is absent
// from stored and returns the merged map (stored is not modified) plus a
// KeptSetting for each requested key whose stored value differs.
// The broker type label (store.BrokerTypeLabel) defaults to "external" when
// absent from both.
func mergeRegistrationLabels(stored, requested map[string]string) (map[string]string, []KeptSetting) {
	out := make(map[string]string, len(stored)+len(requested)+1)
	for k, v := range stored {
		out[k] = v
	}
	keys := make([]string, 0, len(requested))
	for k := range requested {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var kept []KeptSetting
	for _, k := range keys {
		v := requested[k]
		cur, ok := out[k]
		if !ok {
			out[k] = v
			continue
		}
		if cur != v {
			kept = append(kept, KeptSetting{Field: keptFieldLabelPrefix + k, StoredValue: cur, RequestedValue: v, Reason: KeptReasonSet})
		}
	}
	if _, ok := out[store.BrokerTypeLabel]; !ok {
		out[store.BrokerTypeLabel] = "external"
	}
	return out, kept
}

// gcpHostKept lists the requested GCP host values that differ from the
// stored row. Empty requested values are never listed.
func gcpHostKept(row *store.RuntimeBroker, email, project string) []KeptSetting {
	reason := KeptReasonSet
	if row.GCPHostSource == store.GCPHostSourceAdmin && row.GCPHostServiceAccountEmail == "" && row.GCPHostProjectID == "" {
		reason = KeptReasonPinned
	}
	var kept []KeptSetting
	if email != "" && !strings.EqualFold(email, row.GCPHostServiceAccountEmail) {
		kept = append(kept, KeptSetting{Field: keptFieldGCPHostEmail, StoredValue: row.GCPHostServiceAccountEmail, RequestedValue: email, Reason: reason})
	}
	if project != "" && project != row.GCPHostProjectID {
		kept = append(kept, KeptSetting{Field: keptFieldGCPHostProject, StoredValue: row.GCPHostProjectID, RequestedValue: project, Reason: reason})
	}
	return kept
}

// registrationGCPHostResult is the outcome of fillRegistrationGCPHost.
type registrationGCPHostResult struct {
	Applied bool
	Kept    []KeptSetting
}

// registrationFillsGCPHost reports whether a registration requesting email
// would write the GCP host pair of the existing row b: the request carries an
// email, b has no pair, and the pair was not set or cleared through the
// update API. It is the condition of store.SetRuntimeBrokerGCPHostIfUnset,
// evaluated on the row the handler read, and it selects the requests whose
// host service account needs an actAs decision.
func registrationFillsGCPHost(b *store.RuntimeBroker, email string) bool {
	return b != nil && email != "" &&
		b.GCPHostServiceAccountEmail == "" && b.GCPHostProjectID == "" &&
		b.GCPHostSource != store.GCPHostSourceAdmin
}

// registrationFillAuthorized reports whether verdict v lets a registration
// fill an unset GCP host pair with requested. nil means the pair may be
// written; the stored pair is unset when it is written (the conditional
// UPDATE), which is the value the decision was made against.
// ErrHostSAClaimNotAuthorized means the decision for exactly this pair did
// not allow it. ErrHostSAClaimStale means no decision covers the pair: the
// handler made none because the row it read would not be filled.
func registrationFillAuthorized(requested hostSAClaim, v hostSAClaimVerdict) error {
	if hostSAClaimWriteAllowed(hostSAClaim{}, requested, v) == nil {
		return nil
	}
	if v.Claim == requested {
		return ErrHostSAClaimNotAuthorized
	}
	return ErrHostSAClaimStale
}

// registrationFillDenied returns ErrHostSAClaimNotAuthorized when the
// handler decided a fill of the requested pair and refused it, so the
// registration is refused before any write. Otherwise it returns nil;
// fillRegistrationGCPHost applies the remaining rules.
func registrationFillDenied(email, project string, v hostSAClaimVerdict) error {
	if email == "" {
		return nil
	}
	err := registrationFillAuthorized(hostSAClaim{Email: strings.ToLower(email), ProjectID: project}, v)
	if errors.Is(err, ErrHostSAClaimNotAuthorized) {
		return err
	}
	return nil
}

// fillRegistrationGCPHost applies the registration rule for the GCP host
// pair on an existing row: a request with an email fills the pair only
// while it is unset and not set through the update API, and only with an
// allowed verdict for exactly the requested pair (hostSA, see
// registrationFillAuthorized). Without such a verdict nothing is written:
// the requested values are kept, or, when the row became fillable after the
// handler read it, the request is refused with ErrHostSAClaimStale. email
// must already be lowercased.
func fillRegistrationGCPHost(ctx context.Context, st store.Store, brokerID, email, project string, hostSA hostSAClaimVerdict) (registrationGCPHostResult, error) {
	var res registrationGCPHostResult
	if email == "" && project == "" {
		return res, nil
	}
	requested := hostSAClaim{Email: strings.ToLower(email), ProjectID: project}
	authorized := email != "" && registrationFillAuthorized(requested, hostSA) == nil
	if email != "" {
		if err := registrationFillDenied(email, project, hostSA); err != nil {
			return res, err
		}
	}
	if authorized {
		applied, err := st.SetRuntimeBrokerGCPHostIfUnset(ctx, brokerID, email, project, store.GCPHostSourceRegistration)
		if err != nil {
			return res, fmt.Errorf("failed to set GCP host fields: %w", err)
		}
		if applied {
			res.Applied = true
			return res, nil
		}
	}
	row, err := st.GetRuntimeBroker(ctx, brokerID)
	if err != nil {
		return res, fmt.Errorf("failed to read runtime broker: %w", err)
	}
	if !authorized && registrationFillsGCPHost(row, email) {
		return res, ErrHostSAClaimStale
	}
	res.Kept = gcpHostKept(row, email, project)
	if email == "" && row.GCPHostServiceAccountEmail == "" && row.GCPHostSource != store.GCPHostSourceAdmin {
		for i := range res.Kept {
			res.Kept[i].Reason = KeptReasonEmailRequired
		}
	}
	return res, nil
}

// keptSettingsAuditDetails returns the register audit details for kept
// settings: kept.<field>.stored and kept.<field>.requested per entry, and
// gcp_host_kept=true when a GCP host field was kept. Label keys are
// sanitized for the log.
func keptSettingsAuditDetails(kept []KeptSetting) []string {
	var kv []string
	gcpKept := false
	for _, k := range kept {
		field := sanitizeForLog(k.Field, 160)
		kv = append(kv,
			"kept."+field+".stored", sanitizeForLog(k.StoredValue, 256),
			"kept."+field+".requested", sanitizeForLog(k.RequestedValue, 256),
		)
		if k.Field == keptFieldGCPHostEmail || k.Field == keptFieldGCPHostProject {
			gcpKept = true
		}
	}
	if gcpKept {
		kv = append(kv, "gcp_host_kept", "true")
	}
	return kv
}
