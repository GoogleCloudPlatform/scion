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
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// PeerRole is the role a relay peer is enrolled with: a relay, or a hub
// (whose ingress and drain calls relays serve).
type PeerRole string

const (
	PeerRoleRelay PeerRole = "relay"
	PeerRoleHub   PeerRole = "hub"
)

// peerSignatureVersion3 marks a per-peer key signature: an Ed25519
// signature by the key the peer is enrolled with, named by the key ID.
const peerSignatureVersion3 = "3"

// peerSignatureV3Domain starts the signed input of a per-peer key
// signature: domain, key ID and a newline, then the canonical request
// input (canonical), which names the peer.
const peerSignatureV3Domain = "scion-conduit-peer-v3\n"

// PeerKey is one enrolled public key of a peer.
type PeerKey struct {
	ID        string
	PublicKey ed25519.PublicKey
}

// PeerEnrollment is one enrolled peer: its instance ID, its role and its
// public keys. A key verifies only signatures made for this ID.
type PeerEnrollment struct {
	ID   string
	Role PeerRole
	Keys []PeerKey
}

// PeerDirectory is the operator's enrolled peers, by instance ID.
type PeerDirectory struct {
	peers map[string]PeerEnrollment
}

// NewPeerDirectory checks entries and returns their directory. IDs must be
// unique and non-empty, roles relay or hub, key IDs unique per peer and in
// the key ID syntax, and keys Ed25519 public keys.
func NewPeerDirectory(entries []PeerEnrollment) (*PeerDirectory, error) {
	d := &PeerDirectory{peers: make(map[string]PeerEnrollment, len(entries))}
	for i, e := range entries {
		if e.ID == "" {
			return nil, fmt.Errorf("conduit relay peers: entry %d has no id", i)
		}
		if _, dup := d.peers[e.ID]; dup {
			return nil, fmt.Errorf("conduit relay peers: id %q is listed twice", e.ID)
		}
		if e.Role != PeerRoleRelay && e.Role != PeerRoleHub {
			return nil, fmt.Errorf("conduit relay peers: id %q has role %q (want relay or hub)", e.ID, e.Role)
		}
		if len(e.Keys) == 0 {
			return nil, fmt.Errorf("conduit relay peers: id %q has no keys", e.ID)
		}
		seen := map[string]bool{}
		for _, k := range e.Keys {
			if !peerKeyIDPattern.MatchString(k.ID) {
				return nil, fmt.Errorf("conduit relay peers: id %q has a key id %q outside [A-Za-z0-9_-]{1,32}", e.ID, k.ID)
			}
			if seen[k.ID] {
				return nil, fmt.Errorf("conduit relay peers: id %q lists key id %q twice", e.ID, k.ID)
			}
			seen[k.ID] = true
			if len(k.PublicKey) != ed25519.PublicKeySize {
				return nil, fmt.Errorf("conduit relay peers: id %q key %q is not an Ed25519 public key", e.ID, k.ID)
			}
		}
		d.peers[e.ID] = e
	}
	return d, nil
}

// key returns the public key id has under kid, and id's role.
func (d *PeerDirectory) key(id, kid string) (ed25519.PublicKey, PeerRole, bool) {
	e, ok := d.peers[id]
	if !ok {
		return nil, "", false
	}
	for _, k := range e.Keys {
		if k.ID == kid {
			return k.PublicKey, e.Role, true
		}
	}
	return nil, "", false
}

// role returns id's enrolled role.
func (d *PeerDirectory) role(id string) (PeerRole, bool) {
	e, ok := d.peers[id]
	return e.Role, ok
}

// ownKeyID returns the key ID under which id is enrolled with pub.
func (d *PeerDirectory) ownKeyID(id string, pub ed25519.PublicKey) (string, bool) {
	e, ok := d.peers[id]
	if !ok {
		return "", false
	}
	for _, k := range e.Keys {
		if k.PublicKey.Equal(pub) {
			return k.ID, true
		}
	}
	return "", false
}

// ParsePeerPublicKey decodes a standard base64 Ed25519 public key.
func ParsePeerPublicKey(s string) (ed25519.PublicKey, error) {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil || len(b) != ed25519.PublicKeySize {
		return nil, errors.New("not a base64 Ed25519 public key")
	}
	return ed25519.PublicKey(b), nil
}

// LoadPeerPrivateKey reads a PEM PKCS #8 Ed25519 private key from path.
// A file readable by group or other is refused, as SSH refuses such a key:
// the key is the node's relay-peer identity. The error names the path,
// never key material.
func LoadPeerPrivateKey(path string) (ed25519.PrivateKey, error) {
	// The mode is checked on the open file and the key read from the same
	// descriptor, so both apply to one inode.
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("conduit relay peer key %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("conduit relay peer key %s: %w", path, err)
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		return nil, fmt.Errorf("conduit relay peer key %s: mode %04o: group or other has access to it; chmod 600", path, perm)
	}
	b, err := io.ReadAll(io.LimitReader(f, 64<<10))
	if err != nil {
		return nil, fmt.Errorf("conduit relay peer key %s: %w", path, err)
	}
	blk, _ := pem.Decode(b)
	if blk == nil || blk.Type != "PRIVATE KEY" {
		return nil, fmt.Errorf("conduit relay peer key %s: not a PEM PRIVATE KEY", path)
	}
	k, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
	if err != nil {
		return nil, fmt.Errorf("conduit relay peer key %s: not a PKCS #8 key", path)
	}
	priv, ok := k.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("conduit relay peer key %s: not an Ed25519 key", path)
	}
	return priv, nil
}

// PeerRoleResolver is implemented by an authenticator that knows the role
// a verified peer is enrolled with.
type PeerRoleResolver interface {
	// PeerRole returns the enrolled role of peer, and false when peer is
	// not enrolled.
	PeerRole(peer string) (PeerRole, bool)
}

// ErrPeerRole is returned by RequirePeerRole for a peer enrolled with
// another role than the endpoint serves.
var ErrPeerRole = errors.New("conduit relay: peer is not enrolled for this call")

// RequirePeerRole checks that a peer verified by auth may make a call that
// only peers of role want make: a peer enrolled with another role is
// refused. A peer that is not enrolled (or an authenticator without
// enrolled peers) is not checked here.
func RequirePeerRole(auth PeerAuth, peer string, want PeerRole) error {
	r, ok := auth.(PeerRoleResolver)
	if !ok {
		return nil
	}
	if role, enrolled := r.PeerRole(peer); enrolled && role != want {
		return fmt.Errorf("%w (enrolled as %s, the call needs %s)", ErrPeerRole, role, want)
	}
	return nil
}

// KeyedPeerAuthConfig configures KeyedPeerAuth.
type KeyedPeerAuthConfig struct {
	// Legacy signs when Key is nil and verifies every request that is not
	// a per-peer key signature. It also holds the replay cache and the
	// skew window both formats share.
	Legacy *HMACPeerAuth
	// Directory is the enrolled peers.
	Directory *PeerDirectory
	// Key is this node's private key, or nil: the node then signs with
	// Legacy. When set, this node's instance ID (Legacy's SelfID) must be
	// enrolled with Key's public key.
	Key ed25519.PrivateKey
}

// KeyedPeerAuth signs with this node's enrolled key (when it has one) and
// verifies per-peer key signatures against the enrolled peers: a key
// verifies only for the ID it is enrolled under. Requests in the earlier
// formats are verified by the legacy authenticator, unchanged.
type KeyedPeerAuth struct {
	legacy *HMACPeerAuth
	dir    *PeerDirectory
	key    ed25519.PrivateKey
	kid    string
}

// NewKeyedPeerAuth returns a KeyedPeerAuth. It refuses a Key whose public
// key is not enrolled for this node's instance ID.
func NewKeyedPeerAuth(cfg KeyedPeerAuthConfig) (*KeyedPeerAuth, error) {
	if cfg.Legacy == nil || cfg.Directory == nil {
		return nil, errors.New("conduit relay: a keyed peer authenticator needs the legacy authenticator and the enrolled peers")
	}
	a := &KeyedPeerAuth{legacy: cfg.Legacy, dir: cfg.Directory}
	if cfg.Key != nil {
		self := cfg.Legacy.cfg.SelfID
		kid, ok := cfg.Directory.ownKeyID(self, cfg.Key.Public().(ed25519.PublicKey))
		if !ok {
			return nil, fmt.Errorf("conduit relay: this node's peer key is not enrolled for its instance id %q", self)
		}
		a.key, a.kid = cfg.Key, kid
	}
	return a, nil
}

// Sign implements PeerAuth.
func (a *KeyedPeerAuth) Sign(req *http.Request) error {
	if a.key == nil {
		return a.legacy.Sign(req)
	}
	now := a.legacy.cfg.Now()
	var nb [16]byte
	if _, err := rand.Read(nb[:]); err != nil {
		return err
	}
	self := a.legacy.cfg.SelfID
	ts := strconv.FormatInt(now.Unix(), 10)
	nonce := hex.EncodeToString(nb[:])
	req.Header.Set(HeaderPeerID, self)
	req.Header.Set(HeaderPeerTimestamp, ts)
	req.Header.Set(HeaderPeerNonce, nonce)
	req.Header.Set(HeaderPeerKeyID, a.kid)
	req.Header.Set(HeaderPeerSignatureVersion, peerSignatureVersion3)
	sig := ed25519.Sign(a.key, peerV3Input(a.kid, canonical(req, self, ts, nonce)))
	req.Header.Set(HeaderPeerSignature, base64.RawURLEncoding.EncodeToString(sig))
	return nil
}

// Verify implements PeerAuth. A per-peer key signature is checked with the
// key the asserted peer is enrolled with under the named key ID; then the
// shared skew window and replay cache. Any other request goes to the legacy
// authenticator.
func (a *KeyedPeerAuth) Verify(req *http.Request) (string, error) {
	versions := req.Header.Values(HeaderPeerSignatureVersion)
	if len(versions) != 1 || versions[0] != peerSignatureVersion3 {
		return a.legacy.Verify(req)
	}
	peer := req.Header.Get(HeaderPeerID)
	ts := req.Header.Get(HeaderPeerTimestamp)
	nonce := req.Header.Get(HeaderPeerNonce)
	sig := req.Header.Get(HeaderPeerSignature)
	kids := req.Header.Values(HeaderPeerKeyID)
	if peer == "" || ts == "" || nonce == "" || sig == "" || len(nonce) > 64 ||
		len(kids) != 1 || !peerKeyIDPattern.MatchString(kids[0]) {
		return "", ErrPeerUnauthenticated
	}
	pub, _, ok := a.dir.key(peer, kids[0])
	if !ok {
		return "", ErrPeerUnauthenticated
	}
	got, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil || !ed25519.Verify(pub, peerV3Input(kids[0], canonical(req, peer, ts, nonce)), got) {
		return "", ErrPeerUnauthenticated
	}
	if err := a.legacy.checkFreshAndRecord(peer, ts, nonce, a.legacy.cfg.Now()); err != nil {
		return "", err
	}
	return peer, nil
}

// PeerRole implements PeerRoleResolver.
func (a *KeyedPeerAuth) PeerRole(peer string) (PeerRole, bool) {
	return a.dir.role(peer)
}

// peerV3Input is the signed input of a per-peer key signature.
func peerV3Input(kid string, input []byte) []byte {
	b := make([]byte, 0, len(peerSignatureV3Domain)+len(kid)+1+len(input))
	b = append(b, peerSignatureV3Domain...)
	b = append(b, kid...)
	b = append(b, '\n')
	return append(b, input...)
}

// PeerKeyOptions returns the enrolled peers and this node's private key
// from the conduit settings (server.hub.conduit.peers and peer_key_file),
// for ConduitPeerAuthOptions. A node with a key file must have its
// instance id configured (the id it is enrolled under is otherwise
// regenerated on restart); the error names the setting, never key
// material.
func PeerKeyOptions(c config.HubConduitConfig) ([]PeerEnrollment, ed25519.PrivateKey, error) {
	var peers []PeerEnrollment
	for _, p := range c.Peers {
		e := PeerEnrollment{ID: p.ID, Role: PeerRole(p.Role)}
		for _, k := range p.Keys {
			pub, err := ParsePeerPublicKey(k.PublicKey)
			if err != nil {
				return nil, nil, &SettingError{Key: peersSetting, Err: fmt.Errorf("server.hub.conduit.peers %q key %q: %w", p.ID, k.KeyID, err)}
			}
			e.Keys = append(e.Keys, PeerKey{ID: k.KeyID, PublicKey: pub})
		}
		peers = append(peers, e)
	}
	if c.PeerKeyFile == "" {
		return peers, nil, nil
	}
	if c.InstanceID == "" {
		return nil, nil, &SettingError{Key: peerKeyFileSetting, Err: errors.New("server.hub.conduit.peer_key_file requires server.hub.conduit.instance_id: the id this node is enrolled under must not change on restart")}
	}
	key, err := LoadPeerPrivateKey(c.PeerKeyFile)
	if err != nil {
		return nil, nil, &SettingError{Key: peerKeyFileSetting, Err: err}
	}
	return peers, key, nil
}
