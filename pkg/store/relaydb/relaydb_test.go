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

//go:build !no_sqlite

package relaydb

import (
	"context"
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
)

// TestSettingsReaderResolvesConduit: hub.conduit is resolved from the
// experiments settings row as the hub resolves it: no row means the
// registry default, an override wins, and a malformed document is off.
func TestSettingsReaderResolvesConduit(t *testing.T) {
	ctx := context.Background()
	exp := func(def bool) *experiments.Registry {
		r, err := experiments.NewRegistry([]experiments.Experiment{{
			Name: conduitExperiment, Title: "T", Description: "D", Default: def,
			Layers: []experiments.Layer{experiments.LayerServer}, Stage: experiments.StageAlpha,
			Issue: "ptone/scion#1", Owner: "o", ReviewBy: "2027-01-01",
		}}, nil)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	for _, tc := range []struct {
		name  string
		def   bool
		value string // "" = no row
		want  bool
	}{
		{name: "no row, default off", def: false, want: false},
		{name: "no row, default on", def: true, want: true},
		{name: "override on", def: false, value: `{"overrides":{"hub.conduit":true}}`, want: true},
		{name: "override off", def: true, value: `{"overrides":{"hub.conduit":false}}`, want: false},
		{name: "another experiment only", def: true, value: `{"overrides":{"web.other":false}}`, want: true},
		{name: "malformed document", def: true, value: `{"overrides":"yes"}`, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := enttest.NewClient(t)
			if tc.value != "" {
				client.HubSetting.Create().SetSection(experimentsSection).SetValue(json.RawMessage(tc.value)).SaveX(ctx)
			}
			r := &SettingsReader{client: client, reg: exp(tc.def)}
			got, err := r.ConduitEnabled(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("ConduitEnabled = %v, want %v", got, tc.want)
			}
		})
	}

	t.Run("unreadable", func(t *testing.T) {
		client := enttest.NewClient(t)
		_ = client.Close()
		if _, err := (&SettingsReader{client: client}).ConduitEnabled(ctx); err == nil {
			t.Fatal("no error from a closed database")
		}
	})
}

// TestSubjectMatches: the listener matches subjects as the hub does.
func TestSubjectMatches(t *testing.T) {
	for _, tc := range []struct {
		pattern, subject string
		want             bool
	}{
		{"agent.*.deleted", "agent.a1.deleted", true},
		{"agent.*.deleted", "agent.a1.status", false},
		{"agent.*.deleted", "agent.a1.deleted.x", false},
		{"conduit.authz.changed", "conduit.authz.changed", true},
		{"project.>", "project.p1.deleted", true},
		{"project.>", "project", false},
	} {
		if got := subjectMatches(tc.pattern, tc.subject); got != tc.want {
			t.Errorf("subjectMatches(%q, %q) = %v", tc.pattern, tc.subject, got)
		}
	}
}

// TestListenerDeliversOnlyItsSubjects: a notification reaches a subscriber
// only when its subject is one of the listener's and the subscriber's.
func TestListenerDeliversOnlyItsSubjects(t *testing.T) {
	l := &Listener{patterns: []string{"agent.*.deleted", "admin.settings.updated"}, subs: map[*subscription]struct{}{}, onListen: map[int]func(){}}
	all, unsubAll := l.Subscribe("agent.*.deleted", "admin.settings.updated", "user.>")
	defer unsubAll()
	settings, unsubSettings := l.Subscribe("admin.settings.updated")
	defer unsubSettings()
	for _, s := range []string{"user.u1.created", "agent.a1.deleted", "admin.settings.updated"} {
		l.deliver(s)
	}
	if got := <-all; got != "agent.a1.deleted" {
		t.Fatalf("first delivery %q", got)
	}
	if got := <-all; got != "admin.settings.updated" {
		t.Fatalf("second delivery %q", got)
	}
	if got := <-settings; got != "admin.settings.updated" {
		t.Fatalf("settings delivery %q", got)
	}
	select {
	case got := <-all:
		t.Fatalf("delivered %q outside the listener's subjects", got)
	default:
	}
	select {
	case got := <-settings:
		t.Fatalf("delivered %q outside the subscriber's subjects", got)
	default:
	}
}

// TestListenHooksRunOffTheReadLoop: a listen hook that blocks does not hold
// up the caller (the read loop).
func TestListenHooksRunOffTheReadLoop(t *testing.T) {
	l := &Listener{subs: map[*subscription]struct{}{}, onListen: map[int]func(){}}
	release, ran := make(chan struct{}), make(chan struct{})
	l.AddOnListen(func() {
		<-release
		close(ran)
	})
	returned := make(chan struct{})
	go func() {
		l.fireOnListen()
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(10 * time.Second):
		t.Fatal("fireOnListen blocked on a listen hook: hooks run on the read loop")
	}
	close(release)
	select {
	case <-ran:
	case <-time.After(10 * time.Second):
		t.Fatal("the listen hook never ran")
	}
}

// TestListenerNotStartedUntilStart: NewListener starts no connection loop
// (nothing connects or LISTENs before Start), and a listener that was never
// started closes at once.
func TestListenerNotStartedUntilStart(t *testing.T) {
	l, err := NewListener("postgres://relay@127.0.0.1:1/scion", []string{"agent.*.deleted"}, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	if l.started {
		t.Fatal("the listener started before Start")
	}
	select {
	case <-l.done:
		t.Fatal("a connection loop ran before Start")
	default:
	}
	l.Close()
	if l.started {
		t.Fatal("Close started the listener")
	}
}
