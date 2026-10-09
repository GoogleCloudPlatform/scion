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

package logging

import (
	"context"
	"log/slog"
	"math"

	"github.com/GoogleCloudPlatform/scion/pkg/util/logging/loglevel"
)

// passAllLevel lets every record through an inner handler; the levelFilter
// wrapping it makes the real decision from the shared loglevel state.
const passAllLevel = slog.Level(math.MinInt32)

// levelFilter is a slog.Handler that gates records using the process-wide
// level spec from package loglevel, including per-component levels keyed on
// the "subsystem" attribute that Subsystem() attaches.
//
// The subsystem is taken from WithAttrs (the usual Subsystem() path) or,
// failing that, from a top-level "subsystem" attribute on the record itself.
// Because a record-level attribute is not known in Enabled, Enabled admits
// anything at or above the most verbose configured level and Handle makes
// the final decision.
type levelFilter struct {
	inner     slog.Handler
	subsystem string
	// grouped is true once WithGroup has been applied: later attrs are
	// nested and no longer name the subsystem.
	grouped bool
}

// newLevelFilter wraps h so that it honours the shared level spec.
func newLevelFilter(h slog.Handler) slog.Handler {
	if h == nil {
		return nil
	}
	if lf, ok := h.(*levelFilter); ok {
		return lf
	}
	return &levelFilter{inner: h}
}

// Enabled implements slog.Handler.
func (h *levelFilter) Enabled(ctx context.Context, level slog.Level) bool {
	var floor slog.Level
	if h.subsystem != "" {
		floor = loglevel.Effective(h.subsystem)
	} else {
		floor = loglevel.MinLevel().Level()
	}
	return level >= floor && h.inner.Enabled(ctx, level)
}

// Handle implements slog.Handler.
func (h *levelFilter) Handle(ctx context.Context, r slog.Record) error {
	sub := h.subsystem
	if sub == "" && !h.grouped {
		r.Attrs(func(a slog.Attr) bool {
			if a.Key == AttrSubsystem {
				sub = a.Value.String()
				return false
			}
			return true
		})
	}
	if r.Level < loglevel.Effective(sub) {
		return nil
	}
	return h.inner.Handle(ctx, r)
}

// WithAttrs implements slog.Handler.
func (h *levelFilter) WithAttrs(attrs []slog.Attr) slog.Handler {
	next := &levelFilter{inner: h.inner.WithAttrs(attrs), subsystem: h.subsystem, grouped: h.grouped}
	if !h.grouped {
		for _, a := range attrs {
			if a.Key == AttrSubsystem {
				next.subsystem = a.Value.String()
			}
		}
	}
	return next
}

// WithGroup implements slog.Handler.
func (h *levelFilter) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	return &levelFilter{inner: h.inner.WithGroup(name), subsystem: h.subsystem, grouped: true}
}
