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

package conduit

import (
	"fmt"

	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

// PrincipalKind is the canonical principal kind string (contracts §2).
type PrincipalKind string

// Principal kinds.
const (
	PrincipalBroker    PrincipalKind = "broker"
	PrincipalAgent     PrincipalKind = "agent"
	PrincipalUser      PrincipalKind = "user"
	PrincipalRelayPeer PrincipalKind = "relay-peer"
)

var principalToProto = map[PrincipalKind]conduitv1.PrincipalKind{
	PrincipalBroker:    conduitv1.PrincipalKind_PRINCIPAL_KIND_BROKER,
	PrincipalAgent:     conduitv1.PrincipalKind_PRINCIPAL_KIND_AGENT,
	PrincipalUser:      conduitv1.PrincipalKind_PRINCIPAL_KIND_USER,
	PrincipalRelayPeer: conduitv1.PrincipalKind_PRINCIPAL_KIND_RELAY_PEER,
}

// Proto converts k to its wire enum. Unknown kinds map to UNSPECIFIED,
// which receivers reject.
func (k PrincipalKind) Proto() conduitv1.PrincipalKind { return principalToProto[k] }

// PrincipalKindFromProto converts a wire enum to the canonical string. The
// UNSPECIFIED zero value and unknown values are errors.
func PrincipalKindFromProto(p conduitv1.PrincipalKind) (PrincipalKind, error) {
	for k, v := range principalToProto {
		if v == p {
			return k, nil
		}
	}
	return "", fmt.Errorf("conduit: invalid principal kind %v", p)
}

// StreamKind is the canonical stream kind string (contracts §2).
type StreamKind string

// Stream kinds.
const (
	StreamTCP    StreamKind = "tcp"
	StreamPTY    StreamKind = "pty"
	StreamSSH    StreamKind = "ssh"
	StreamLogs   StreamKind = "logs"
	StreamEvents StreamKind = "events"
)

var streamToProto = map[StreamKind]conduitv1.StreamKind{
	StreamTCP:    conduitv1.StreamKind_STREAM_KIND_TCP,
	StreamPTY:    conduitv1.StreamKind_STREAM_KIND_PTY,
	StreamSSH:    conduitv1.StreamKind_STREAM_KIND_SSH,
	StreamLogs:   conduitv1.StreamKind_STREAM_KIND_LOGS,
	StreamEvents: conduitv1.StreamKind_STREAM_KIND_EVENTS,
}

// Proto converts k to its wire enum (UNSPECIFIED for unknown kinds).
func (k StreamKind) Proto() conduitv1.StreamKind { return streamToProto[k] }

// StreamKindFromProto converts a wire enum to the canonical string. The
// UNSPECIFIED zero value and unknown values are errors.
func StreamKindFromProto(p conduitv1.StreamKind) (StreamKind, error) {
	for k, v := range streamToProto {
		if v == p {
			return k, nil
		}
	}
	return "", fmt.Errorf("conduit: invalid stream kind %v", p)
}

// StreamState is a stream lifecycle state (§3.5 "Stream lifecycle").
type StreamState int

// Stream states.
const (
	StateOpening StreamState = iota
	StateAccepted
	StateActive
	StateDraining
	StateClosed
	StateFailed
)

func (s StreamState) String() string {
	switch s {
	case StateOpening:
		return "opening"
	case StateAccepted:
		return "accepted"
	case StateActive:
		return "active"
	case StateDraining:
		return "draining"
	case StateClosed:
		return "closed"
	case StateFailed:
		return "failed"
	}
	return fmt.Sprintf("StreamState(%d)", int(s))
}
