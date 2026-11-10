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

package brokerconvert

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"

	"github.com/GoogleCloudPlatform/scion/pkg/brokeridentity"
	"github.com/GoogleCloudPlatform/scion/pkg/util/privatefs"
)

// maxIdentityFileSize bounds the identity file read.
const maxIdentityFileSize = 1 << 20

// identityBrokerID reads the Runtime Broker ID that the identity file of
// instance key records, read-only through privatefs (no link followed, the
// file must be owner-only). ok is false, with no error, when the file is
// absent or records no ID. Registration mints and saves this ID before it
// calls the Hub, so a row created by a registration that then failed or was
// cut short carries it.
func identityBrokerID(globalDir, key string) (id string, ok bool, err error) {
	if !instanceKeyRe.MatchString(key) {
		return "", false, fmt.Errorf("invalid instance key")
	}
	tree, err := privatefs.OpenTree(globalDir, privatefs.TreeOptions{})
	if err != nil {
		return "", false, err
	}
	rel := brokeridentity.RuntimeBrokersDirName + "/" + key + "/" + brokeridentity.IdentityFileName
	data, err := tree.ReadPrivate(rel, maxIdentityFileSize)
	if errors.Is(err, fs.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	var rec struct {
		RuntimeBrokerID string `json:"runtimeBrokerId"`
	}
	if err := json.Unmarshal(data, &rec); err != nil {
		return "", false, err
	}
	if rec.RuntimeBrokerID == "" {
		return "", false, nil
	}
	return rec.RuntimeBrokerID, true, nil
}
