//go:build !no_sqlite

// REPRO ONLY (nc-delivery-unreachable investigation). Not a fix. Each test
// asserts the CURRENT (buggy) behaviour so it passes today and documents the
// false-"Delivered" chain for native chat v2.

package hub

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

type reproFailingDispatcher struct{ brokerMockDispatcher }

func (d *reproFailingDispatcher) DispatchAgentMessage(ctx context.Context, a *store.Agent, m string, i bool, sm *messages.StructuredMessage) error {
	_ = d.brokerMockDispatcher.DispatchAgentMessage(ctx, a, m, i, sm)
	return errors.New("agent 'x' not found or not running")
}

func reproSetup(t *testing.T, phase string, deleted bool, disp AgentDispatcher) (*Server, store.Store, string, *store.Agent) {
	t.Helper()
	srv, s, wcs, proj, db := setupSendTest(t)
	srv.SetDispatcher(disp)
	ctx := t.Context()
	a := &store.Agent{ID: tid("repro-" + phase), ProjectID: proj.ID, Name: "Repro", Slug: "repro-" + phase,
		Phase: phase, OwnerID: DevUserID, CreatedBy: DevUserID}
	if err := s.CreateAgent(ctx, a); err != nil {
		t.Fatal(err)
	}
	if deleted {
		a.DeletedAt = time.Now()
		if err := s.UpdateAgent(ctx, a); err != nil {
			t.Fatal(err)
		}
	}
	topicID := tid("repro-topic-" + phase)
	if err := wcs.CreateTopic(ctx, WebChatTopic{ID: topicID, ProjectID: proj.ID, Name: "repro-" + phase,
		CreatedBy: "dev", CreatedAt: time.Now().UTC(), DefaultAgent: a.Slug}); err != nil {
		t.Fatal(err)
	}
	setTopicConversationID(t, db, s, topicID, proj.ID)
	return srv, s, topicID, a
}

func reproSend(t *testing.T, srv *Server, s store.Store, topicID, content string) (int, string, *store.Message) {
	t.Helper()
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/chat/conversations/"+topicID+"/messages",
		map[string]string{"content": content})
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	id, _ := resp["id"].(string)
	var m *store.Message
	if id != "" {
		m, _ = s.GetMessage(t.Context(), id)
	}
	t.Logf("HTTP %d body=%s", rec.Code, rec.Body.String())
	if m != nil {
		r := ""
		if m.DispatchFailureReason != nil {
			r = *m.DispatchFailureReason
		}
		t.Logf("stored: recipient=%s dispatch_state=%s reason=%q", m.Recipient, m.DispatchState, r)
	}
	return rec.Code, rec.Body.String(), m
}

// Stopped default agent: broker buffer accepts (dispatcher returns nil) -> 201, row "dispatched",
// and no message ID on ctx so the broker's async failure report (#1820) can never flip it.
func TestReproNC_StoppedDefault_BufferAccepts(t *testing.T) {
	d := &brokerMockDispatcher{}
	srv, s, topic, _ := reproSetup(t, "stopped", false, d)
	code, _, m := reproSend(t, srv, s, topic, "hello")
	if code != http.StatusCreated || m == nil || m.DispatchState != store.MessageDispatchDispatched {
		t.Fatalf("behaviour changed: code=%d msg=%+v", code, m)
	}
	msgs := d.getMessages()
	if len(msgs) != 1 || msgs[0].messageID != "" {
		t.Fatalf("expected 1 dispatch with no ctx message id, got %+v", msgs)
	}
	t.Logf("dispatched to stopped agent; ctx messageID=%q (empty => async failure unreportable)", msgs[0].messageID)
}

// Suspended default agent whose dispatch errors synchronously: row is marked failed but the
// HTTP response is still 201 and carries no failure signal -> frontend shows "Delivered".
func TestReproNC_SuspendedDefault_DispatchErrors(t *testing.T) {
	srv, s, topic, _ := reproSetup(t, "suspended", false, &reproFailingDispatcher{})
	code, body, m := reproSend(t, srv, s, topic, "hello")
	if code != http.StatusCreated || m == nil || m.DispatchState != store.MessageDispatchFailed {
		t.Fatalf("behaviour changed: code=%d msg=%+v", code, m)
	}
	var resp map[string]any
	_ = json.Unmarshal([]byte(body), &resp)
	if _, ok := resp["dispatchState"]; ok {
		t.Fatalf("response now carries dispatchState: %s", body)
	}
}

// Leading @-mention of a stopped agent (no default): same false success.
func TestReproNC_LeadingMentionStopped(t *testing.T) {
	d := &brokerMockDispatcher{}
	srv, s, wcs, proj, db := setupSendTest(t)
	srv.SetDispatcher(d)
	ctx := t.Context()
	a := &store.Agent{ID: tid("repro-m"), ProjectID: proj.ID, Name: "M", Slug: "stopped-m", Phase: "error",
		OwnerID: DevUserID, CreatedBy: DevUserID}
	if err := s.CreateAgent(ctx, a); err != nil {
		t.Fatal(err)
	}
	topicID := tid("repro-topic-m")
	if err := wcs.CreateTopic(ctx, WebChatTopic{ID: topicID, ProjectID: proj.ID, Name: "m", CreatedBy: "dev", CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	setTopicConversationID(t, db, s, topicID, proj.ID)
	code, body, m := reproSend(t, srv, s, topicID, "@stopped-m please look")
	if code != http.StatusCreated || m == nil || m.DispatchState != store.MessageDispatchDispatched {
		t.Fatalf("behaviour changed: code=%d msg=%+v", code, m)
	}
	t.Logf("mentions in response: %s", body)
}

// Deleted default agent still named on the topic: resolution yields nil default, message silently
// falls through to human-to-human (201, recipient not an agent). Frontend's optimistic bubble still
// renders "-> agent" + "Delivered".
func TestReproNC_DeletedDefault(t *testing.T) {
	d := &brokerMockDispatcher{}
	srv, s, topic, _ := reproSetup(t, "running", true, d)
	code, _, m := reproSend(t, srv, s, topic, "hello")
	if code != http.StatusCreated || len(d.getMessages()) != 0 {
		t.Fatalf("behaviour changed: code=%d dispatches=%d", code, len(d.getMessages()))
	}
	if m != nil {
		t.Logf("recipient=%q type=%q", m.Recipient, m.Type)
	}
}
