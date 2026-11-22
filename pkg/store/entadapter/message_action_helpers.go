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
	"errors"
	"fmt"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// errMessageActionRace marks a write that lost a race with a concurrent
// message-action prompt or state transition: a unique-index violation on
// (agent, key_norm, version), a counter collision, or a row whose status
// changed before a compare-and-set update ran.
var errMessageActionRace = errors.New("concurrent message action write")

// maxMessageActionRaceRetries bounds retries of a lost race: each retry reads
// the winner's committed rows, so n concurrent reposts of one key need up to
// n-1 retries for the last one.
const maxMessageActionRaceRetries = 5

// markMessageActionRace wraps a constraint violation as errMessageActionRace,
// maps Ent field validation failures to store.ErrInvalidInput, and maps any
// other error through mapError.
func markMessageActionRace(err error) error {
	if ent.IsConstraintError(err) {
		return fmt.Errorf("%w: %v", errMessageActionRace, err)
	}
	if ent.IsValidationError(err) {
		return fmt.Errorf("%w: %v", store.ErrInvalidInput, err)
	}
	return mapError(err)
}

// withMessageActionRetry runs fn inside a transaction (or directly when c is
// already in a transaction), retrying up to maxMessageActionRaceRetries times
// on errMessageActionRace or store.ErrTransient. If retries are exhausted on
// errMessageActionRace, it returns store.ErrTransient.
func withMessageActionRetry[T any](
	ctx context.Context,
	c *CompositeStore,
	fn func(tx *CompositeStore) (T, error),
) (T, error) {
	if c.inTx {
		return fn(c)
	}
	var res T
	for attempt := 0; ; attempt++ {
		err := c.WithTx(ctx, func(tx store.Store) error {
			var err error
			res, err = fn(tx.(*CompositeStore))
			return err
		})
		if err == nil {
			return res, nil
		}
		if attempt < maxMessageActionRaceRetries && (errors.Is(err, errMessageActionRace) || errors.Is(err, store.ErrTransient)) {
			continue
		}
		if errors.Is(err, errMessageActionRace) {
			var zero T
			return zero, store.ErrTransient
		}
		var zero T
		return zero, err
	}
}

// insertNextActionVersion runs a next-version insert under a unique
// (agent, key_norm, version) index with bounded retry on concurrent race
// collisions. Callers should wrap unique-index insert errors with
// markMessageActionRace and CAS misses with errMessageActionRace.
func insertNextActionVersion[T any](
	ctx context.Context,
	c *CompositeStore,
	fn func(tx *CompositeStore) (T, error),
) (T, error) {
	return withMessageActionRetry(ctx, c, fn)
}

// casActionRows executes a compare-and-set update over a set of expected rows
// (from status X to Y). update must apply the status predicate and return the
// affected row count. When affected == expected, ok is true and current is nil.
// When affected != expected, reload is invoked (when non-nil) to return the
// current rows so callers can inspect the actual statuses or report a conflict.
func casActionRows[Row any](
	ctx context.Context,
	expected int,
	update func(context.Context) (int, error),
	reload func(context.Context) ([]Row, error),
) (ok bool, current []Row, err error) {
	if expected == 0 {
		return true, nil, nil
	}
	n, err := update(ctx)
	if err != nil {
		return false, nil, mapError(err)
	}
	if n == expected {
		return true, nil, nil
	}
	if reload == nil {
		return false, nil, nil
	}
	rows, rerr := reload(ctx)
	if rerr != nil {
		return false, nil, mapError(rerr)
	}
	return false, rows, nil
}
