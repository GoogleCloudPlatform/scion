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

package backoff

import (
	"testing"
	"time"
)

func TestFailureDoublesAndCaps(t *testing.T) {
	b := &Backoff{Base: time.Second, Max: 10 * time.Second}
	want := []time.Duration{1, 2, 4, 8, 10, 10, 10}
	for i, w := range want {
		if got := b.Failure(); got != w*time.Second {
			t.Fatalf("failure %d: got %v, want %v", i+1, got, w*time.Second)
		}
	}
}

func TestResetReturnsToBase(t *testing.T) {
	b := &Backoff{Base: time.Second, Max: time.Minute}
	b.Failure()
	b.Failure()
	b.Failure()
	b.Reset()
	if got := b.Failure(); got != time.Second {
		t.Fatalf("after reset: got %v, want %v", got, time.Second)
	}
}

func TestAtMax(t *testing.T) {
	b := &Backoff{Base: time.Second, Max: 5 * time.Minute}
	if got := b.AtMax(); got != 5*time.Minute {
		t.Fatalf("AtMax() = %v, want %v", got, 5*time.Minute)
	}
}

func TestJitterOnlyShortensWithinFraction(t *testing.T) {
	for _, r := range []float64{0, 0.5, 0.999} {
		b := &Backoff{Base: time.Second, Max: 10 * time.Second, Jitter: 0.2, Rand: func() float64 { return r }}
		got := b.AtMax()
		if got > 10*time.Second || got < 8*time.Second {
			t.Fatalf("r=%v: AtMax() = %v, want within [8s, 10s]", r, got)
		}
	}
}

func TestNonPositiveBaseAndSmallMax(t *testing.T) {
	b := &Backoff{}
	for i := 0; i < 3; i++ {
		if got := b.Failure(); got != DefaultBase {
			t.Fatalf("zero Backoff failure %d: got %v, want %v", i+1, got, DefaultBase)
		}
	}
	b = &Backoff{Base: 10 * time.Second, Max: time.Second}
	if got := b.Failure(); got != 10*time.Second {
		t.Fatalf("Max below Base: got %v, want %v", got, 10*time.Second)
	}
	if got := b.AtMax(); got != 10*time.Second {
		t.Fatalf("Max below Base AtMax: got %v, want %v", got, 10*time.Second)
	}
}
