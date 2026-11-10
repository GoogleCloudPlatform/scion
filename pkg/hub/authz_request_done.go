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

// Authorization checks after the request context ended (ptone/scion#6012).
//
// When a client gives up, the request context is cancelled (or its deadline
// passes) while a handler may still be checking permissions or agent
// standing item by item. Each such check denies, as every failed check
// does, but it is not a hub fault: logging it as a WARN per item turns one
// abandoned list into hundreds of lines. Instead each one is logged at
// DEBUG and counted on a request-scoped note, and the request emits one
// INFO summary line when its handler returns.

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync"
)

// authzStoppedNote counts the authorization checks of one request that
// were refused because the request context had ended. It is installed by
// serveAfterAuth and read once when the handler returns.
type authzStoppedNote struct {
	// reqCtx is the request context the note was installed on. A check is
	// counted as stopped by the request ending only when its ctx ended
	// because reqCtx did (see authzStoppedByRequestEnd), not when only the
	// check's own bound has.
	reqCtx context.Context

	mu        sync.Mutex
	count     int
	firstSite string
	class     string
	route     string
	// emitted is set once the summary has been written; a later stopped
	// check is not counted silently but logged as a WARN by its caller.
	emitted bool
}

type authzStoppedNoteKey struct{}

// withAuthzStoppedNote installs a fresh note for the request context ctx
// unless one is present. It returns the note only when it installed it, so
// only the outermost installer emits; otherwise it returns ctx unchanged
// and a nil note.
func withAuthzStoppedNote(ctx context.Context) (context.Context, *authzStoppedNote) {
	if _, ok := ctx.Value(authzStoppedNoteKey{}).(*authzStoppedNote); ok {
		return ctx, nil
	}
	n := &authzStoppedNote{reqCtx: ctx}
	return context.WithValue(ctx, authzStoppedNoteKey{}, n), n
}

func authzStoppedNoteFrom(ctx context.Context) *authzStoppedNote {
	n, _ := ctx.Value(authzStoppedNoteKey{}).(*authzStoppedNote)
	return n
}

// authzStoppedByRequestEnd reports whether a check on ctx that failed with
// err was stopped by the request ending, that is, ctx ended because the
// request context did:
//   - ctx was cancelled (err is that cancellation) and the request context
//     has ended; or
//   - ctx ran out its deadline (err is that deadline), the request context
//     ran out its deadline too, and the two deadlines are the same one.
//
// Anything else is an ordinary fault and keeps its WARN: a check that ran
// past its own caller-set bound while the request is live, and a detached
// check (context.WithoutCancel of the request plus its own timeout) that
// ran past its own bound, even after the request ended. Without a note,
// only a cancellation counts; a deadline keeps its WARN.
func authzStoppedByRequestEnd(ctx context.Context, err error) bool {
	ctxErr := ctx.Err()
	if err == nil || ctxErr == nil {
		return false
	}
	canceled := errors.Is(ctxErr, context.Canceled) && errors.Is(err, context.Canceled)
	n := authzStoppedNoteFrom(ctx)
	if n == nil {
		return canceled
	}
	reqErr := n.reqCtx.Err()
	if reqErr == nil {
		return false
	}
	if canceled {
		return true
	}
	if !errors.Is(ctxErr, context.DeadlineExceeded) || !errors.Is(err, context.DeadlineExceeded) ||
		!errors.Is(reqErr, context.DeadlineExceeded) {
		return false
	}
	reqDeadline, ok := n.reqCtx.Deadline()
	if !ok {
		return false
	}
	deadline, ok := ctx.Deadline()
	return ok && deadline.Equal(reqDeadline)
}

// requestDoneClass returns "deadline" or "canceled" for the end of the
// request context of ctx (the note's, else ctx's own).
func requestDoneClass(ctx context.Context) string {
	end := ctx.Err()
	if n := authzStoppedNoteFrom(ctx); n != nil && n.reqCtx.Err() != nil {
		end = n.reqCtx.Err()
	}
	if errors.Is(end, context.DeadlineExceeded) {
		return "deadline"
	}
	return "canceled"
}

// logAuthzCheckFault logs the failure err of the check at site: at DEBUG,
// counted on the request's note, when the request ending stopped it
// (authzStoppedByRequestEnd); otherwise as the WARN warnMsg with attrs and
// the error. The caller denies either way.
func logAuthzCheckFault(ctx context.Context, logger *slog.Logger, site, warnMsg string, err error, attrs ...any) {
	if logger == nil {
		logger = slog.Default()
	}
	if authzStoppedByRequestEnd(ctx, err) && noteAuthzStopped(ctx, logger, site, err, attrs...) {
		return
	}
	logger.Warn(warnMsg, append(attrs, "error", err)...)
}

// noteAuthzStopped records one check at site that stopped because the
// request context ended, and logs it at DEBUG. It reports false, logging
// nothing, when the request's summary has already been written: the
// caller then logs its WARN, so no stopped check goes unreported. The
// caller still denies.
func noteAuthzStopped(ctx context.Context, logger *slog.Logger, site string, err error, attrs ...any) bool {
	class := requestDoneClass(ctx)
	if n := authzStoppedNoteFrom(ctx); n != nil {
		n.mu.Lock()
		if n.emitted {
			n.mu.Unlock()
			return false
		}
		if n.count == 0 {
			n.firstSite = site
			n.class = class
			n.route = routeFromContext(ctx)
		}
		n.count++
		n.mu.Unlock()
	}
	if logger == nil {
		logger = slog.Default()
	}
	logger.Debug("authorization check stopped after the request context ended",
		append([]any{"site", site, "class", class, "error", err}, attrs...)...)
	return true
}

// emit writes the request's single summary line, only when at least one
// check was stopped.
func (n *authzStoppedNote) emit(r *http.Request) {
	if n == nil {
		return
	}
	n.mu.Lock()
	count, site, class, route := n.count, n.firstSite, n.class, n.route
	n.emitted = true
	n.mu.Unlock()
	if count == 0 {
		return
	}
	attrs := []any{"count", count, "first_site", site, "class", class}
	if r != nil {
		attrs = append(attrs, "method", r.Method)
	}
	if route != "" {
		attrs = append(attrs, "route", route)
	}
	slog.Info("authorization checks stopped after the request context ended", attrs...)
}
