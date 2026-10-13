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
	"sync"

	policytroubleshooterpb "cloud.google.com/go/policytroubleshooter/iam/apiv3/iampb"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	gax "github.com/googleapis/gax-go/v2"
)

func newCountingChecker() *countingChecker {
	return &countingChecker{
		inner: store.NewFakeCallerPermissionChecker(),
	}
}

var (
	cacheCaller = store.Principal{
		Kind:                store.PrincipalAgent,
		ID:                  "agent-1",
		ServiceAccountEmail: "caller@proj.iam.gserviceaccount.com",
	}

	cacheCaller2 = store.Principal{
		Kind:                store.PrincipalAgent,
		ID:                  "agent-2",
		ServiceAccountEmail: "caller2@proj.iam.gserviceaccount.com",
	}

	cacheTargetSA = &store.GCPServiceAccount{
		ID:        "sa-1",
		Email:     "target@proj.iam.gserviceaccount.com",
		ProjectID: "proj",
	}

	cacheTargetSA2 = &store.GCPServiceAccount{
		ID:        "sa-2",
		Email:     "target2@proj.iam.gserviceaccount.com",
		ProjectID: "proj",
	}
)

// fakePTClient is a scriptable PTClient for tests.
type fakePTClient struct {
	resp *policytroubleshooterpb.TroubleshootIamPolicyResponse
	err  error
	// captured records the last request for assertions.
	captured *policytroubleshooterpb.TroubleshootIamPolicyRequest
}

var (
	testCaller = store.Principal{
		Kind:                store.PrincipalAgent,
		ID:                  "agent-1",
		ServiceAccountEmail: "agent@my-project.iam.gserviceaccount.com",
	}

	testHumanCaller = store.Principal{
		Kind:  store.PrincipalUser,
		ID:    "user-1",
		Email: "alice@example.com",
	}

	testTargetSA = &store.GCPServiceAccount{
		ID:        "sa-1",
		Email:     "target@target-project.iam.gserviceaccount.com",
		ProjectID: "target-project",
	}

	testHubSAEmail = "hub-sa@hub-project.iam.gserviceaccount.com"
)

func (f *fakePTClient) TroubleshootIamPolicy(
	_ context.Context,
	req *policytroubleshooterpb.TroubleshootIamPolicyRequest,
	_ ...gax.CallOption,
) (*policytroubleshooterpb.TroubleshootIamPolicyResponse, error) {
	f.captured = req
	return f.resp, f.err
}

// countingChecker wraps a FakeCallerPermissionChecker and tracks call counts
// per target SA email.
type countingChecker struct {
	mu    sync.Mutex
	inner *store.FakeCallerPermissionChecker
	calls int
}

func (c *countingChecker) CanActAs(ctx context.Context, caller store.Principal, targetSA *store.GCPServiceAccount) (store.ActAsResult, error) {
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()
	return c.inner.CanActAs(ctx, caller, targetSA)
}

func (c *countingChecker) callCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}
