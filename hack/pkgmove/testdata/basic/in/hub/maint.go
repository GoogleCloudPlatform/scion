// Copyright 2026 Example Authors
// SPDX-License-Identifier: Apache-2.0

package hub

import (
	"fmt"
	"runtime"
	"strings"
	"time"
)

const maxItems = 3

var registry = map[string]int{}

var computed = seed()

type widget struct {
	Name string
	n    int
}

func newWidget(name string) *widget { return &widget{Name: name, n: computed} }

func (w *widget) count() int { return w.n }

// Label is exported already.
func (w *widget) Label() string { return fmt.Sprint(w.Name) }

func helper(x int, rest ...string) (int, error) {
	return x + len(strings.Join(rest, "")), nil
}

func seed() int { return 2 }

// Exported returns a widget.
func Exported() *widget { return newWidget("e") }

// Map applies f to every element.
func Map[T any](xs []T, f func(T) T) []T {
	out := make([]T, 0, len(xs))
	for _, x := range xs {
		out = append(out, f(x))
	}
	return out
}

// Pair is a generic pair.
type Pair[K comparable, V any] struct {
	K K
	V V
}

func onlyForTests() string { return "t" }

func init() {
	registry["init"] = 1
}

// guard recovers a panic; staying code defers it.
func guard(err *error) {
	if r := recover(); r != nil {
		*err = fmt.Errorf("recovered: %v", r)
	}
}

// where reports the file of its caller.
func where() string {
	_, file, _, _ := runtime.Caller(1)
	return file
}

// whereVia reaches where through another moved function.
func whereVia() string { return where() }

// Timeout is aliased with a standard-library type in its signature.
func Timeout(d time.Duration) time.Duration { return 2 * d }
