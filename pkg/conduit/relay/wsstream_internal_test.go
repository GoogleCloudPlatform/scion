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

package relay

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
	"google.golang.org/protobuf/proto"
)

// TestSpliceCloseMapping (r2-F6): a splice leg that ended with a normal
// close (1000) ends the other leg with 1000, never 4504; other close codes
// pass through and non-close errors are 4504 upstream_unreachable.
func TestSpliceCloseMapping(t *testing.T) {
	for _, tc := range []struct {
		name       string
		err        error
		wantCode   uint32
		wantReason string
	}{
		{name: "normal close", err: &conduit.CloseError{Code: conduit.CloseNormal}, wantCode: conduit.CloseNormal},
		{name: "wrapped normal close", err: fmt.Errorf("write: %w", &conduit.CloseError{Code: conduit.CloseNormal, Reason: "bye"}), wantCode: conduit.CloseNormal},
		{name: "other close code", err: &conduit.CloseError{Code: conduit.CloseForbidden, Reason: "forbidden: x"}, wantCode: conduit.CloseForbidden, wantReason: "forbidden: x"},
		{name: "transport error", err: errors.New("broken pipe"), wantCode: conduit.CloseRelayTimeout, wantReason: reason(ReasonUpstreamUnreachable, "peer leg closed")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, why := spliceClose(tc.err, "peer leg closed")
			if code != tc.wantCode || why != tc.wantReason {
				t.Fatalf("spliceClose = %d %q, want %d %q", code, why, tc.wantCode, tc.wantReason)
			}
		})
	}
}

// failingWriteConn delivers queued frames to the reader and fails every
// write.
type failingWriteConn struct {
	frames chan []byte
	closed chan struct{}
	once   sync.Once
}

func newFailingWriteConn() *failingWriteConn {
	return &failingWriteConn{frames: make(chan []byte, 4), closed: make(chan struct{})}
}

func (c *failingWriteConn) ReadFrame() ([]byte, error) {
	select {
	case b := <-c.frames:
		return b, nil
	case <-c.closed:
		return nil, errors.New("closed")
	}
}

func (c *failingWriteConn) WriteFrame([]byte) error { return errors.New("write failed") }

func (c *failingWriteConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return nil
}

func (c *failingWriteConn) Transport() string { return "test" }

// TestWSStreamWindowUpdateSendFailure: when returning credit to the peer
// fails to send, the stream ends as a lost link, as a failed data write
// does. The bytes already read are still returned.
func TestWSStreamWindowUpdateSendFailure(t *testing.T) {
	conn := newFailingWriteConn()
	s := newWSStream(conn, 0, 8)
	b, err := proto.Marshal(&conduitv1.Frame{Body: &conduitv1.Frame_StreamData{StreamData: &conduitv1.StreamData{StreamId: hopStreamID, Data: []byte("12345678")}}})
	if err != nil {
		t.Fatal(err)
	}
	conn.frames <- b

	p := make([]byte, 8)
	n, err := s.Read(p)
	if err != nil || string(p[:n]) != "12345678" {
		t.Fatalf("Read = %d %q, %v; want the 8 bytes", n, p[:n], err)
	}
	select {
	case <-s.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("stream did not end after the window update failed to send")
	}
	if ended, endErr := s.endedErr(); !ended || !errors.Is(endErr, errLinkLost) {
		t.Fatalf("endedErr = %v, %v; want ended with errLinkLost", ended, endErr)
	}
	if _, err := s.Read(p); err == nil {
		t.Fatal("Read after the stream ended: want an error")
	}
}
