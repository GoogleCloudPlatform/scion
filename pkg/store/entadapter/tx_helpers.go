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

package entadapter

import (
	"context"
	"fmt"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
)

// runInEntTx runs fn in a transaction on client and commits it if fn
// returns nil. When inTx is set, client already wraps the ambient WithTx
// transaction (see newTxCompositeStore, which sets the sub-stores' inTx
// flags), so fn runs on it directly: a second Tx call on a transactional
// client fails. A sub-store that needs its own transaction outside WithTx
// and must also work inside it carries an inTx flag and calls this.
func runInEntTx(ctx context.Context, client *ent.Client, inTx bool, fn func(c *ent.Client) error) error {
	if inTx {
		return fn(client)
	}
	tx, err := client.Tx(ctx)
	if err != nil {
		return markTransient(fmt.Errorf("begin transaction: %w", err))
	}
	defer func() { _ = tx.Rollback() }()
	if err := fn(tx.Client()); err != nil {
		return markTransient(err)
	}
	if err := tx.Commit(); err != nil {
		return markTransient(fmt.Errorf("commit transaction: %w", err))
	}
	return nil
}
