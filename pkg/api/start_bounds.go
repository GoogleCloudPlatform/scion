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

package api

import "time"

// SyncStartCeiling is the Runtime Broker's bound on a synchronous create's
// start (Manager.Start) once the start has begun. The start then no longer
// follows the hub's dispatch request: the hub giving up on it (its
// synchronous dispatch timeout, a control-channel cancel frame, or a dropped
// HTTP request) does not cancel it. Only a stop, a delete or a newer start
// of the agent, broker shutdown, or this bound does (ptone/scion#4817).
//
// It is longer than the slowest legitimate start (the Kubernetes runtime's
// 10 minute pod-ready wait, its exec-ready wait, plus provisioning), so it
// ends only a start that is stuck. The hub's bounds on an unconfirmed
// create (start-claim holds, the run-adoption quiet period) must cover it,
// so the hub never reads a start that may still be running as finished.
const SyncStartCeiling = 15 * time.Minute
