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

package cmd

import (
	"context"
	"fmt"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
)

// splitTemplatesByHubPresence splits local project templates into those not
// yet on the Hub in the project scope and those that already exist there. It
// uses the same lookup as syncTemplateToHub (exact name, project scope,
// active), so a template it reports as missing is one sync would create.
func splitTemplatesByHubPresence(ctx context.Context, hubCtx *HubContext, templates []*config.Template) (missing, existing []*config.Template, err error) {
	projectID, err := GetProjectID(hubCtx)
	if err != nil {
		return nil, nil, err
	}
	for _, tpl := range templates {
		resp, err := hubCtx.Client.Templates().List(ctx, &hubclient.ListTemplatesOptions{
			Name:      tpl.Name,
			Scope:     "project",
			ProjectID: projectID,
			Status:    "active",
		})
		if err != nil {
			return nil, nil, fmt.Errorf("failed to check Hub for template %q: %w", tpl.Name, err)
		}
		found := false
		for i := range resp.Templates {
			if resp.Templates[i].Name == tpl.Name {
				found = true
				break
			}
		}
		if found {
			existing = append(existing, tpl)
		} else {
			missing = append(missing, tpl)
		}
	}
	return missing, existing, nil
}
