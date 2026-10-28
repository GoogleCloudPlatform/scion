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

// Package eventstreams tracks the browser events streams (the hub's /events
// Server-Sent Events endpoint) open on one hub instance
// (ptone/scion#5227), feeds the scion.hub.sse.* metrics from them
// (ptone/scion#5228), and classifies published events for
// scion.hub.events.published. It is phase 0 of ptone/scion#5219: it
// observes only. Phase 1 builds on the Registry for max age, supersede by
// key and the per-user cap.
//
// The package has no dependency on pkg/hub; the hub's events handler
// registers each stream after it subscribes and closes it on every exit
// path.
package eventstreams

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/observability/ssemetrics"
)

// Kind classifies a stream by what the web client opened it for. The
// client names it with the KindParam query parameter; anything else
// (including no parameter, which is every stream until the client sends
// one) is KindOther.
type Kind string

const (
	KindView             Kind = "view"
	KindAgentFeed        Kind = "agent-feed"
	KindTerminalMetadata Kind = "terminal-metadata"
	KindOther            Kind = "other"
)

// ParseKind maps a kind query parameter to a known Kind.
func ParseKind(s string) Kind {
	switch k := Kind(s); k {
	case KindView, KindAgentFeed, KindTerminalMetadata:
		return k
	}
	return KindOther
}

// CloseReason says why a stream ended. It is the reason attribute of
// scion.hub.sse.closes and the reason field of the close log line.
type CloseReason string

const (
	// CloseClient: the client went away (its request context ended).
	CloseClient CloseReason = "client"
	// CloseMaxAge: the stream reached its maximum age and the server sent a
	// reconnect hint.
	CloseMaxAge CloseReason = "max_age"
	// CloseSuperseded: a newer stream with the same key replaced it. Set by
	// phase 1 of ptone/scion#5219; unused until then.
	CloseSuperseded CloseReason = "superseded"
	// CloseCap: the user reached the per-instance stream cap. Set by phase 1
	// of ptone/scion#5219; unused until then.
	CloseCap CloseReason = "cap"
	// ClosePublisher: the event publisher closed the subscription.
	ClosePublisher CloseReason = "publisher"
	// CloseShutdown: the server began shutting down.
	CloseShutdown CloseReason = "shutdown"
	// CloseSessionEnded: the test identity web session behind the stream
	// ended or stopped being valid.
	CloseSessionEnded CloseReason = "session_ended"
)

// Query parameters that identify a stream. The web client sends them from
// phase 1 of ptone/scion#5219; until then they are absent and the stream is
// identified by its connection id.
const (
	BrowserParam = "browser"
	TabParam     = "tab"
	KindParam    = "kind"
)

// MaxClientIDLen bounds a client-supplied browser or tab id.
const MaxClientIDLen = 64

// SanitizeClientID returns s if it is a plausible client id (1 to
// MaxClientIDLen letters, digits, '-' or '_'), and "" otherwise, so a client
// value never reaches a log line or a map key unbounded. A dropped id does
// not refuse the stream; it falls back to its connection id.
func SanitizeClientID(s string) string {
	if s == "" || len(s) > MaxClientIDLen {
		return ""
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return ""
		}
	}
	return s
}

// NewConnID returns a random 16-hex-digit connection id.
func NewConnID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// Key identifies a stream. ConnID is always set and unique to the
// connection; BrowserID and TabID are the client's ids, empty until the
// client sends them, in which case ConnID is the stream's identity.
type Key struct {
	UserID    string
	BrowserID string
	TabID     string
	Kind      Kind
	ConnID    string
}

// HasClientIDs reports whether the client identified the stream with both a
// browser id and a tab id (the supersede key of phase 1).
func (k Key) HasClientIDs() bool {
	return k.BrowserID != "" && k.TabID != ""
}

// Info is a point-in-time view of one open stream.
type Info struct {
	Key           Key
	ConnectedAt   time.Time
	Subjects      int
	EventsWritten int64
	BytesWritten  int64
}

// Stream is one open stream in a Registry. The handler serving it records
// its writes on it and closes it when the stream ends.
type Stream struct {
	reg         *Registry
	key         Key
	connectedAt time.Time
	subjects    int

	events atomic.Int64
	bytes  atomic.Int64
	closed atomic.Bool
}

// Key returns the stream's key.
func (s *Stream) Key() Key { return s.key }

// ConnectedAt returns when the stream was registered.
func (s *Stream) ConnectedAt() time.Time { return s.connectedAt }

// RecordEvent counts one update event of n bytes written to the stream.
func (s *Stream) RecordEvent(n int) {
	s.events.Add(1)
	s.bytes.Add(int64(n))
	m := s.reg.recorder()
	m.AddEventsWritten(context.Background(), string(s.key.Kind), 1)
	m.AddBytesWritten(context.Background(), string(s.key.Kind), int64(n))
}

// RecordBytes counts n bytes written to the stream that are not an update
// event (a heartbeat or a reconnect hint).
func (s *Stream) RecordBytes(n int) {
	s.bytes.Add(int64(n))
	s.reg.recorder().AddBytesWritten(context.Background(), string(s.key.Kind), int64(n))
}

// Info returns the stream's current view.
func (s *Stream) Info() Info {
	return Info{
		Key:           s.key,
		ConnectedAt:   s.connectedAt,
		Subjects:      s.subjects,
		EventsWritten: s.events.Load(),
		BytesWritten:  s.bytes.Load(),
	}
}

// Close removes the stream from its registry, records the close metrics
// and writes one Info close log line to logger (slog.Default() if nil),
// whatever the perf-trace setting. Only the first call has any effect.
func (s *Stream) Close(reason CloseReason, logger *slog.Logger) {
	if !s.closed.CompareAndSwap(false, true) {
		return
	}
	s.reg.remove(s)
	lifetime := s.reg.now().Sub(s.connectedAt)
	s.reg.recorder().RecordClose(context.Background(), string(s.key.Kind), string(reason), lifetime)
	if logger == nil {
		logger = slog.Default()
	}
	logger.Info("events stream closed",
		slog.String("conn_id", s.key.ConnID),
		slog.String("user_id", s.key.UserID),
		slog.String("browser_id", s.key.BrowserID),
		slog.String("tab_id", s.key.TabID),
		slog.String("kind", string(s.key.Kind)),
		slog.Int("subjects", s.subjects),
		slog.Int64("duration_ms", lifetime.Milliseconds()),
		slog.Int64("events_written", s.events.Load()),
		slog.Int64("bytes_written", s.bytes.Load()),
		slog.String("reason", string(reason)),
	)
}

// Registry tracks the streams open on this hub instance. The zero value is
// ready to use, with metrics disabled.
type Registry struct {
	mu      sync.Mutex
	streams map[*Stream]struct{}
	perKind map[Kind]int
	perUser map[string]int

	metrics atomic.Pointer[recorderBox]
	// clock, when set, replaces time.Now. Tests only.
	clock func() time.Time
}

// recorderBox lets an interface value sit in an atomic.Pointer.
type recorderBox struct{ rec ssemetrics.Recorder }

// disabledRecorder is the recorder used until SetMetrics.
var disabledRecorder = ssemetrics.NewDisabled()

// SetMetrics wires the scion.hub.sse.* recorder. Nil disables it.
func (r *Registry) SetMetrics(rec ssemetrics.Recorder) {
	if rec == nil {
		r.metrics.Store(nil)
		return
	}
	r.metrics.Store(&recorderBox{rec: rec})
}

func (r *Registry) recorder() ssemetrics.Recorder {
	if b := r.metrics.Load(); b != nil {
		return b.rec
	}
	return disabledRecorder
}

func (r *Registry) now() time.Time {
	if r.clock != nil {
		return r.clock()
	}
	return time.Now()
}

// Register adds a stream with the given key and subject count and returns
// it. An empty Kind is KindOther. The caller must Close the stream on every
// exit path.
func (r *Registry) Register(key Key, subjects int) *Stream {
	if key.Kind == "" {
		key.Kind = KindOther
	}
	s := &Stream{reg: r, key: key, connectedAt: r.now(), subjects: subjects}
	m := r.recorder()
	ctx := context.Background()

	r.mu.Lock()
	if r.streams == nil {
		r.streams = make(map[*Stream]struct{})
		r.perKind = make(map[Kind]int)
		r.perUser = make(map[string]int)
	}
	r.streams[s] = struct{}{}
	r.perKind[key.Kind]++
	r.perUser[key.UserID]++
	// The gauges are recorded under the lock, so the last value recorded
	// is the current one.
	m.RecordStreams(ctx, string(key.Kind), int64(r.perKind[key.Kind]))
	m.RecordUsers(ctx, int64(len(r.perUser)))
	r.mu.Unlock()

	m.IncConnects(ctx, string(key.Kind))
	return s
}

// remove deletes s from the registry and updates the gauges.
func (r *Registry) remove(s *Stream) {
	m := r.recorder()
	ctx := context.Background()
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.streams[s]; !ok {
		return
	}
	delete(r.streams, s)
	if r.perKind[s.key.Kind]--; r.perKind[s.key.Kind] <= 0 {
		delete(r.perKind, s.key.Kind)
	}
	if r.perUser[s.key.UserID]--; r.perUser[s.key.UserID] <= 0 {
		delete(r.perUser, s.key.UserID)
	}
	m.RecordStreams(ctx, string(s.key.Kind), int64(r.perKind[s.key.Kind]))
	m.RecordUsers(ctx, int64(len(r.perUser)))
}

// Count returns the number of open streams.
func (r *Registry) Count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.streams)
}

// CountByKind returns the number of open streams of each kind that has at
// least one.
func (r *Registry) CountByKind() map[Kind]int {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[Kind]int, len(r.perKind))
	for k, n := range r.perKind {
		out[k] = n
	}
	return out
}

// Users returns the number of distinct users with an open stream.
func (r *Registry) Users() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.perUser)
}

// Snapshot returns every open stream, oldest first.
func (r *Registry) Snapshot() []Info {
	return r.collect(func(*Stream) bool { return true })
}

// UserStreams returns the open streams of one user, oldest first.
func (r *Registry) UserStreams(userID string) []Info {
	return r.collect(func(s *Stream) bool { return s.key.UserID == userID })
}

func (r *Registry) collect(keep func(*Stream) bool) []Info {
	r.mu.Lock()
	out := make([]Info, 0, len(r.streams))
	for s := range r.streams {
		if keep(s) {
			out = append(out, s.Info())
		}
	}
	r.mu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		if !out[i].ConnectedAt.Equal(out[j].ConnectedAt) {
			return out[i].ConnectedAt.Before(out[j].ConnectedAt)
		}
		return out[i].Key.ConnID < out[j].Key.ConnID
	})
	return out
}
