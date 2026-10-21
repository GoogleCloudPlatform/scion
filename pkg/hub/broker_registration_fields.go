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
//     empty request value never clears them.
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

// fillRegistrationGCPHost applies the registration rule for the GCP host
// pair on an existing row: a request with an email fills the pair only
// while it is unset and not set through the update API. email must already
// be lowercased.
func fillRegistrationGCPHost(ctx context.Context, st store.Store, brokerID, email, project string) (registrationGCPHostResult, error) {
	var res registrationGCPHostResult
	if email == "" && project == "" {
		return res, nil
	}
	if email != "" {
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
