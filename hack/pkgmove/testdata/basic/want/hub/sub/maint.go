// Copyright 2026 Example Authors
// SPDX-License-Identifier: Apache-2.0

package sub

import (
	"fmt"
	"runtime"
	"strings"
)

const MaxItems = 3

var Registry = map[string]int{}

var computed = seed()

type Widget struct {
	Name string
	n    int
}

func NewWidget(name string) *Widget { return &Widget{Name: name, n: computed} }

func (w *Widget) Count() int { return w.n }

// Label is exported already.
func (w *Widget) Label() string { return fmt.Sprint(w.Name) }

func Helper(x int, rest ...string) (int, error) {
	return x + len(strings.Join(rest, "")), nil
}

func seed() int { return 2 }

// Exported returns a widget.
func Exported() *Widget { return NewWidget("e") }

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

func OnlyForTests() string { return "t" }

func init() {
	Registry["init"] = 1
}

// guard recovers a panic; staying code defers it.
func Guard(err *error) {
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
func WhereVia() string { return where() }
