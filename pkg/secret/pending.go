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

package secret

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	smpb "cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// HoldPending implements PendingHolder. The local backend holds the value
// inline: the reference is the value sealed by the backend's ValueCipher
// (the same envelope as a stored secret value), so the pending row itself
// carries it and nothing else is stored.
func (b *LocalBackend) HoldPending(_ context.Context, _ string, value []byte) (string, error) {
	if len(value) == 0 {
		return "", errors.New("pending secret value is empty")
	}
	ref, err := b.cipher.Encrypt(string(value))
	if err != nil {
		return "", fmt.Errorf("encrypting pending secret value: %w", err)
	}
	return ref, nil
}

// ReadPending implements PendingHolder: it opens a reference written by
// HoldPending. A Secret Manager reference is refused rather than read as a
// plaintext value.
func (b *LocalBackend) ReadPending(_ context.Context, ref string) ([]byte, error) {
	if ref == "" || strings.HasPrefix(ref, "gcpsm:") {
		return nil, errors.New("pending secret reference is not a local reference")
	}
	pt, err := b.cipher.Decrypt(ref)
	if err != nil {
		return nil, fmt.Errorf("decrypting pending secret value: %w", err)
	}
	return []byte(pt), nil
}

// DropPending implements PendingHolder. The local backend holds the value
// on the pending row, so deleting the row releases it; there is nothing
// else to drop.
func (b *LocalBackend) DropPending(context.Context, string) error {
	return nil
}

// ListHeldPending implements PendingHolder. The local backend holds every
// value on its pending row, so nothing is held elsewhere.
func (b *LocalBackend) ListHeldPending(context.Context) ([]HeldPendingValue, error) {
	return nil, nil
}

// pendingSecretNameSegment follows the hub prefix in the Secret Manager
// name of a held pending value: {secretNamePrefix()}pending-{id}. Approved
// secret names put their scope there ({secretNamePrefix()}{scope}-...),
// and no scope is named "pending", so the two never collide.
const pendingSecretNameSegment = "pending-"

// maxPendingIDLen bounds a pending id used in a Secret Manager name.
const maxPendingIDLen = 64

// pendingSMName is the Secret Manager secret ID holding the value of the
// pending secret id.
func (b *GCPBackend) pendingSMName(id string) string {
	return b.secretNamePrefix() + pendingSecretNameSegment + id
}

// pendingSMPathFromRef returns the Secret Manager path named by a reference
// HoldPending returned. Any other reference is refused, so ReadPending and
// DropPending never touch an approved secret or another hub's secret.
func (b *GCPBackend) pendingSMPathFromRef(ref string) (string, error) {
	path, ok := extractGCPSMPath(ref)
	if !ok {
		return "", errors.New("pending secret reference is not a Secret Manager reference")
	}
	prefix := b.smSecretPath(b.secretNamePrefix() + pendingSecretNameSegment)
	id, ok := strings.CutPrefix(path, prefix)
	if !ok || !validPendingID(id) {
		return "", errors.New("pending secret reference does not name a pending value of this hub")
	}
	return path, nil
}

func validPendingID(id string) bool {
	return id != "" && len(id) <= maxPendingIDLen && !invalidSecretIDChars.MatchString(id)
}

// HoldPending implements PendingHolder. The value is stored as the only
// version of a new Secret Manager secret named by pendingSMName; the
// reference is "gcpsm:" plus its path. The pending id must be new: an
// existing secret of that name is refused rather than given a version.
func (b *GCPBackend) HoldPending(ctx context.Context, id string, value []byte) (string, error) {
	if len(value) == 0 {
		return "", errors.New("pending secret value is empty")
	}
	if !validPendingID(id) {
		return "", errors.New("pending secret id is not valid in a Secret Manager name")
	}
	smName := b.pendingSMName(id)
	path := b.smSecretPath(smName)
	_, err := b.smClient.CreateSecret(ctx, &smpb.CreateSecretRequest{
		Parent:   fmt.Sprintf("projects/%s", b.projectID),
		SecretId: smName,
		Secret: &smpb.Secret{
			Replication: b.buildReplication(),
			Labels: map[string]string{
				"scion-pending":    "true",
				"scion-pending-id": sanitizeLabel(id),
				"scion-hub-name":   sanitizeLabel(b.resolveHubName()),
			},
		},
	})
	if err != nil {
		if permErr := b.wrapGCPError(err, "create secret"); permErr != nil {
			return "", permErr
		}
		return "", fmt.Errorf("failed to create GCP SM secret for a pending value: %w", err)
	}
	_, err = b.smClient.AddSecretVersion(ctx, &smpb.AddSecretVersionRequest{
		Parent:  path,
		Payload: &smpb.SecretPayload{Data: value},
	})
	if err != nil {
		// Remove the empty container; the caller records nothing.
		if delErr := b.smClient.DeleteSecret(ctx, &smpb.DeleteSecretRequest{Name: path}); delErr != nil && status.Code(delErr) != codes.NotFound {
			slog.Warn("pending secret: removing an empty Secret Manager secret failed", "error", delErr)
		}
		if permErr := b.wrapGCPError(err, "add secret version"); permErr != nil {
			return "", permErr
		}
		return "", fmt.Errorf("failed to add GCP SM secret version for a pending value: %w", err)
	}
	return "gcpsm:" + path, nil
}

// ReadPending implements PendingHolder: it reads the value held under a
// reference HoldPending returned.
func (b *GCPBackend) ReadPending(ctx context.Context, ref string) ([]byte, error) {
	path, err := b.pendingSMPathFromRef(ref)
	if err != nil {
		return nil, err
	}
	resp, err := b.smClient.AccessSecretVersion(ctx, &smpb.AccessSecretVersionRequest{
		Name: path + "/versions/latest",
	})
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return nil, store.ErrNotFound
		}
		if permErr := b.wrapGCPError(err, "access secret version"); permErr != nil {
			return nil, permErr
		}
		return nil, fmt.Errorf("failed to read a pending value from GCP SM: %w", err)
	}
	return resp.GetPayload().GetData(), nil
}

// DropPending implements PendingHolder: it deletes the Secret Manager
// secret holding the value. A secret already gone is not an error.
func (b *GCPBackend) DropPending(ctx context.Context, ref string) error {
	path, err := b.pendingSMPathFromRef(ref)
	if err != nil {
		return err
	}
	err = b.smClient.DeleteSecret(ctx, &smpb.DeleteSecretRequest{Name: path})
	if err == nil || status.Code(err) == codes.NotFound {
		return nil
	}
	if permErr := b.wrapGCPError(err, "delete secret"); permErr != nil {
		return permErr
	}
	return fmt.Errorf("failed to delete a pending value from GCP SM: %w", err)
}

// ListHeldPending implements PendingHolder: it lists this hub's Secret
// Manager secrets holding pending values. Only a secret carrying the
// scion-pending label whose name is this hub's pending name
// ({secretNamePrefix()}pending-{id}, with a valid id) is returned; every
// other secret, an approved one or another hub's included, is left out.
// A client that cannot list returns none.
func (b *GCPBackend) ListHeldPending(ctx context.Context) ([]HeldPendingValue, error) {
	lister, ok := b.smClient.(smSecretLister)
	if !ok {
		return nil, nil
	}
	namePrefix := b.secretNamePrefix() + pendingSecretNameSegment
	secrets, err := lister.ListSecrets(ctx, &smpb.ListSecretsRequest{
		Parent: fmt.Sprintf("projects/%s", b.projectID),
		Filter: fmt.Sprintf("labels.scion-pending=true AND name:%s", namePrefix),
	})
	if err != nil {
		if permErr := b.wrapGCPError(err, "list secrets"); permErr != nil {
			return nil, permErr
		}
		return nil, fmt.Errorf("failed to list pending values in GCP SM: %w", err)
	}
	pathPrefix := b.smSecretPath(namePrefix)
	var out []HeldPendingValue
	for _, sec := range secrets {
		if sec.GetLabels()["scion-pending"] != "true" {
			continue
		}
		id, ok := strings.CutPrefix(sec.GetName(), pathPrefix)
		if !ok || !validPendingID(id) {
			continue
		}
		held := HeldPendingValue{Ref: "gcpsm:" + sec.GetName(), PendingID: id}
		if ct := sec.GetCreateTime(); ct != nil {
			held.Created = ct.AsTime()
		}
		out = append(out, held)
	}
	return out, nil
}
