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

package hubclient

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newDeleteTestServer(t *testing.T, h http.HandlerFunc) Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := New(srv.URL)
	require.NoError(t, err)
	return c
}

func TestDeleteWithResult_204IsDone(t *testing.T) {
	var gotPath, gotQuery string
	c := newDeleteTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodDelete, r.Method)
		gotPath, gotQuery = r.URL.Path, r.URL.RawQuery
		w.WriteHeader(http.StatusNoContent)
	})
	res, err := c.ProjectAgents("p1").(AgentDeleteResulter).DeleteWithResult(context.Background(), "a1",
		&DeleteAgentOptions{DeleteFiles: true, RemoveBranch: false})
	require.NoError(t, err)
	assert.Equal(t, DeleteResult{}, res)
	assert.Equal(t, "/api/v1/projects/p1/agents/a1", gotPath)
	assert.Equal(t, "removeBranch=false", gotQuery, "same query as Delete")
}

func TestDeleteWithResult_202IsAccepted(t *testing.T) {
	c := newDeleteTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"agentId":"uuid-1","deletion":{"state":"deleting","soft":true,"claim":3,"startedAt":"2026-10-03T10:00:00Z"}}`))
	})
	res, err := DeleteWithResult(context.Background(), c.ProjectAgents("p1"), "a1", nil)
	require.NoError(t, err)
	assert.True(t, res.Accepted)
	assert.Equal(t, "uuid-1", res.AgentID)
	require.NotNil(t, res.Deletion)
	assert.Equal(t, DeletionStateDeleting, res.Deletion.State)
	assert.Equal(t, int64(3), res.Deletion.Claim)
	assert.True(t, res.Deletion.Soft)
}

func TestDeleteWithResult_202WithoutBodyIsAccepted(t *testing.T) {
	c := newDeleteTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	})
	res, err := DeleteWithResult(context.Background(), c.ProjectAgents("p1"), "a1", nil)
	require.NoError(t, err)
	assert.True(t, res.Accepted)
	assert.Empty(t, res.AgentID)
	assert.Nil(t, res.Deletion)
}

// Errors keep today's *apiclient.APIError so callers can read the code.
func TestDeleteWithResult_ErrorsKeepAPIError(t *testing.T) {
	for _, status := range []int{http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusConflict, http.StatusForbidden} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			c := newDeleteTestServer(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"error":{"code":"runtime_error","message":"broker said no","details":{"deletionCode":"runtime_error"}}}`))
			})
			svc := c.ProjectAgents("p1")
			_, err := DeleteWithResult(context.Background(), svc, "a1", nil)
			var apiErr *apiclient.APIError
			require.True(t, errors.As(err, &apiErr), "got %T", err)
			assert.Equal(t, status, apiErr.StatusCode)

			// Delete returns the same error shape.
			err2 := svc.Delete(context.Background(), "a1", nil)
			var apiErr2 *apiclient.APIError
			require.True(t, errors.As(err2, &apiErr2))
			assert.Equal(t, apiErr.StatusCode, apiErr2.StatusCode)
			assert.Equal(t, apiErr.Code, apiErr2.Code)
		})
	}
}

// The existing Delete is unchanged: a 202 is still a nil error.
func TestDelete_202StillNilError(t *testing.T) {
	c := newDeleteTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"agentId":"x"}`))
	})
	require.NoError(t, c.ProjectAgents("p1").Delete(context.Background(), "a1", nil))
}

// fallbackAgentService implements AgentService but not AgentDeleteResulter,
// like third-party test doubles.
type fallbackAgentService struct {
	AgentService
	deleted int
	err     error
}

func (f *fallbackAgentService) Delete(ctx context.Context, agentID string, opts *DeleteAgentOptions) error {
	f.deleted++
	return f.err
}

func TestDeleteWithResult_FallbackCannotTell202(t *testing.T) {
	f := &fallbackAgentService{}
	res, err := DeleteWithResult(context.Background(), f, "a1", nil)
	require.NoError(t, err)
	assert.Equal(t, 1, f.deleted)
	assert.False(t, res.Accepted, "the fallback cannot see a 202; it behaves as before (no poll)")

	f.err = errors.New("boom")
	_, err = DeleteWithResult(context.Background(), f, "a1", nil)
	require.EqualError(t, err, "boom")
}

// The client type must decode everything the hub's store.DeletionInfo emits.
func TestDeletionInfo_MatchesStoreJSON(t *testing.T) {
	st := reflect.TypeOf(store.DeletionInfo{})
	ct := reflect.TypeOf(DeletionInfo{})
	require.Equal(t, st.NumField(), ct.NumField())
	for i := 0; i < st.NumField(); i++ {
		sf, cf := st.Field(i), ct.Field(i)
		assert.Equal(t, sf.Name, cf.Name)
		assert.Equal(t, sf.Tag.Get("json"), cf.Tag.Get("json"), sf.Name)
		assert.Equal(t, sf.Type, cf.Type, sf.Name)
	}

	now := time.Date(2026, 10, 3, 10, 0, 0, 0, time.UTC)
	in := store.DeletionInfo{State: "failed", Code: "in_doubt", Error: "e", Soft: true, Claim: 7, StartedAt: now, LeaseExpiresAt: &now, ExpiresAt: &now}
	data, err := json.Marshal(in)
	require.NoError(t, err)
	var out DeletionInfo
	require.NoError(t, json.Unmarshal(data, &out))
	assert.Equal(t, DeletionInfo(in), out)
}

// --- WaitForAgentDeletion ---

type getStep struct {
	agent *Agent
	err   error
}

// scriptedAgentService answers Get from a script; the last step repeats.
type scriptedAgentService struct {
	AgentService
	steps []getStep
	calls int
	ids   []string
}

func (s *scriptedAgentService) Get(ctx context.Context, agentID string) (*Agent, error) {
	s.ids = append(s.ids, agentID)
	i := s.calls
	if i >= len(s.steps) {
		i = len(s.steps) - 1
	}
	s.calls++
	return s.steps[i].agent, s.steps[i].err
}

// fakeClock advances by the requested interval each time After is called.
type fakeClock struct {
	now   time.Time
	waits []time.Duration
}

func (c *fakeClock) Now() time.Time { return c.now }
func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	c.waits = append(c.waits, d)
	c.now = c.now.Add(d)
	ch := make(chan time.Time, 1)
	ch <- c.now
	return ch
}

func (c *fakeClock) opts() DeletionWaitOptions {
	return DeletionWaitOptions{Now: c.Now, After: c.After}
}

func deleting() *Agent {
	return &Agent{ID: "a1", Deletion: &DeletionInfo{State: DeletionStateDeleting, Claim: 1}}
}

func apiErr(status int) error {
	return &apiclient.APIError{StatusCode: status, Code: "x", Message: http.StatusText(status)}
}

func TestWaitForAgentDeletion_Outcomes(t *testing.T) {
	failed := &Agent{ID: "a1", Deletion: &DeletionInfo{State: DeletionStateFailed, Code: "runtime_error", Error: "broker unreachable"}}
	live := &Agent{ID: "a1"}
	softDeleted := &Agent{ID: "a1", DeletedAt: time.Date(2026, 10, 3, 10, 0, 0, 0, time.UTC)}

	tests := []struct {
		name      string
		steps     []getStep
		want      DeletionWaitOutcome
		wantPolls int
		check     func(t *testing.T, r DeletionWaitResult)
	}{
		{
			name:      "confirmed via 404",
			steps:     []getStep{{agent: deleting()}, {agent: deleting()}, {err: apiErr(http.StatusNotFound)}},
			want:      DeletionConfirmed,
			wantPolls: 3,
		},
		{
			name:      "confirmed via deletedAt",
			steps:     []getStep{{agent: deleting()}, {agent: softDeleted}},
			want:      DeletionConfirmed,
			wantPolls: 2,
		},
		{
			name:      "failed surfaces the code",
			steps:     []getStep{{agent: deleting()}, {agent: failed}},
			want:      DeletionFailed,
			wantPolls: 2,
			check: func(t *testing.T, r DeletionWaitResult) {
				require.NotNil(t, r.Deletion)
				assert.Equal(t, "runtime_error", r.Deletion.Code)
				assert.Equal(t, "broker unreachable", r.Deletion.Error)
			},
		},
		{
			name:  "failed abandoned and in_doubt are failures too",
			steps: []getStep{{agent: &Agent{ID: "a1", Deletion: &DeletionInfo{State: DeletionStateFailed, Code: "abandoned"}}}},
			want:  DeletionFailed, wantPolls: 1,
		},
		{
			name:      "live row with no deletion marker is not deleted (after the grace poll)",
			steps:     []getStep{{agent: live}},
			want:      DeletionNotTaken,
			wantPolls: 2,
			check: func(t *testing.T, r DeletionWaitResult) {
				assert.Nil(t, r.Deletion)
			},
		},
		{
			name:      "a single null read is lag: deleting afterwards resets the grace",
			steps:     []getStep{{agent: live}, {agent: deleting()}, {agent: live}, {err: apiErr(http.StatusNotFound)}},
			want:      DeletionConfirmed,
			wantPolls: 4,
		},
		{
			name:      "403 cannot observe",
			steps:     []getStep{{agent: deleting()}, {err: apiErr(http.StatusForbidden)}},
			want:      DeletionUnobservable,
			wantPolls: 2,
			check: func(t *testing.T, r DeletionWaitResult) {
				assert.True(t, apiclient.IsForbiddenError(r.Err))
			},
		},
		{
			name:      "401 cannot observe",
			steps:     []getStep{{err: apiErr(http.StatusUnauthorized)}},
			want:      DeletionUnobservable,
			wantPolls: 1,
		},
		{
			name:      "other non-retryable 4xx ends early as unobservable (400)",
			steps:     []getStep{{err: apiErr(http.StatusBadRequest)}},
			want:      DeletionUnobservable,
			wantPolls: 1,
			check: func(t *testing.T, r DeletionWaitResult) {
				require.Error(t, r.Err)
				assert.Contains(t, r.Err.Error(), "Bad Request")
			},
		},
		{
			name:      "other non-retryable 4xx ends early as unobservable (405)",
			steps:     []getStep{{err: apiErr(http.StatusMethodNotAllowed)}},
			want:      DeletionUnobservable,
			wantPolls: 1,
		},
		{
			name:      "other non-retryable 4xx ends early as unobservable (410)",
			steps:     []getStep{{agent: deleting()}, {err: apiErr(http.StatusGone)}},
			want:      DeletionUnobservable,
			wantPolls: 2,
		},
		{
			name:      "408 and 429 are retried",
			steps:     []getStep{{err: apiErr(http.StatusRequestTimeout)}, {err: apiErr(http.StatusTooManyRequests)}, {err: apiErr(http.StatusNotFound)}},
			want:      DeletionConfirmed,
			wantPolls: 3,
		},
		{
			name:      "transient errors keep polling",
			steps:     []getStep{{err: apiErr(http.StatusBadGateway)}, {err: errors.New("connection reset")}, {err: apiErr(http.StatusNotFound)}},
			want:      DeletionConfirmed,
			wantPolls: 3,
		},
		{
			name:      "timeout while still deleting",
			steps:     []getStep{{agent: deleting()}},
			want:      DeletionTimedOut,
			wantPolls: 91, // t=0,2,...,180
		},
		{
			name:      "timeout after only transient errors reports the last error",
			steps:     []getStep{{err: errors.New("connection refused")}},
			want:      DeletionTimedOut,
			wantPolls: 91,
			check: func(t *testing.T, r DeletionWaitResult) {
				require.Error(t, r.Err)
				assert.Contains(t, r.Err.Error(), "connection refused")
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := &scriptedAgentService{steps: tt.steps}
			clk := &fakeClock{now: time.Unix(0, 0)}
			r := WaitForAgentDeletion(context.Background(), svc, "uuid-1", clk.opts())
			assert.Equal(t, tt.want, r.Outcome, "outcome %s", r.Outcome)
			assert.Equal(t, tt.wantPolls, r.Polls)
			assert.Equal(t, tt.wantPolls, svc.calls)
			for _, id := range svc.ids {
				assert.Equal(t, "uuid-1", id)
			}
			for _, w := range clk.waits {
				assert.Equal(t, DefaultDeletionPollInterval, w)
			}
			if tt.check != nil {
				tt.check(t, r)
			}
		})
	}
}

func TestWaitForAgentDeletion_CustomIntervalAndTimeout(t *testing.T) {
	svc := &scriptedAgentService{steps: []getStep{{agent: deleting()}}}
	clk := &fakeClock{now: time.Unix(0, 0)}
	opts := clk.opts()
	opts.Interval = 5 * time.Second
	opts.Timeout = 20 * time.Second
	r := WaitForAgentDeletion(context.Background(), svc, "a1", opts)
	assert.Equal(t, DeletionTimedOut, r.Outcome)
	assert.Equal(t, 5, r.Polls) // t=0,5,10,15,20
}

func TestWaitForAgentDeletion_CanceledContextEndsAsTimeout(t *testing.T) {
	svc := &scriptedAgentService{steps: []getStep{{agent: deleting()}}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	never := func(time.Duration) <-chan time.Time { return nil }
	r := WaitForAgentDeletion(ctx, svc, "a1", DeletionWaitOptions{After: never})
	assert.Equal(t, DeletionTimedOut, r.Outcome)
	assert.ErrorIs(t, r.Err, context.Canceled)
	assert.Equal(t, 1, r.Polls)
}

// stallingAgentService answers the scripted replies, then blocks every
// later Get until its ctx ends (a hub that accepts but never answers).
type stallingAgentService struct {
	AgentService
	before []*Agent
	calls  int
}

func (s *stallingAgentService) Get(ctx context.Context, agentID string) (*Agent, error) {
	s.calls++
	if s.calls <= len(s.before) {
		return s.before[s.calls-1], nil
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

// N4: each GET is capped at the remaining budget, so a stalled hub cannot
// stretch the wait past Timeout. A stall cut short by that cap does not
// replace a good earlier read.
func TestWaitForAgentDeletion_StalledGetStaysWithinTimeout(t *testing.T) {
	for _, tc := range []struct {
		name    string
		before  []*Agent
		wantErr bool
	}{
		{name: "first poll stalls", wantErr: true},
		{name: "stall after a deleting read", before: []*Agent{deleting()}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := &stallingAgentService{before: tc.before}
			start := time.Now()
			r := WaitForAgentDeletion(context.Background(), svc, "a1", DeletionWaitOptions{
				Interval: time.Millisecond,
				Timeout:  50 * time.Millisecond,
			})
			assert.Less(t, time.Since(start), 2*time.Second, "the stalled GET must end at the budget")
			assert.Equal(t, DeletionTimedOut, r.Outcome)
			assert.Equal(t, len(tc.before)+1, r.Polls)
			if tc.wantErr {
				assert.ErrorIs(t, r.Err, context.DeadlineExceeded)
			} else {
				assert.NoError(t, r.Err, "the last good read stands")
				require.NotNil(t, r.Deletion)
				assert.Equal(t, DeletionStateDeleting, r.Deletion.State)
			}
		})
	}
}

// End to end over HTTP: the poll reads the project-scoped agent path, and the
// hub's JSON (deletion, deletedAt) decodes into the client Agent.
func TestWaitForAgentDeletion_OverHTTPProjectScoped(t *testing.T) {
	var polls atomic.Int32
	c := newDeleteTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodGet, r.Method)
		require.Equal(t, "/api/v1/projects/p1/agents/uuid-1", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch polls.Add(1) {
		case 1:
			_, _ = w.Write([]byte(`{"id":"uuid-1","phase":"stopping","deletion":{"state":"deleting","soft":true,"claim":1,"startedAt":"2026-10-03T10:00:00Z"}}`))
		default:
			_, _ = w.Write([]byte(`{"id":"uuid-1","phase":"stopped","deletedAt":"2026-10-03T10:00:05Z","deletion":null}`))
		}
	})
	r := WaitForAgentDeletion(context.Background(), c.ProjectAgents("p1"), "uuid-1",
		DeletionWaitOptions{Interval: time.Millisecond, Timeout: 5 * time.Second})
	assert.Equal(t, DeletionConfirmed, r.Outcome)
	assert.Equal(t, 2, r.Polls)
}
