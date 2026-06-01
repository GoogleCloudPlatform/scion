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

package ent

import "entgo.io/ent/dialect"

// Driver returns the underlying dialect.Driver backing the client.
//
// This is a hand-written extension (not part of the generated code) that gives
// adapters access to the driver so they can (a) branch on the active SQL
// dialect and (b) execute dialect-specific raw statements that the generated
// query builders cannot express — notably `SELECT ... FOR UPDATE SKIP LOCKED`
// used by the job-claim paths under Postgres. See
// pkg/store/entadapter/schedule_store.go.
func (c *Client) Driver() dialect.Driver {
	return c.driver
}
