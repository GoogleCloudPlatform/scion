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
	"time"

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

// syncNewTemplatesOnLink is the interactive part of hub link's template
// offer. It offers only templates not yet on the Hub (syncing one that
// already exists mirrors the local directory, which also removes files added
// on the Hub, so that is left to an explicit templates sync), lists the ones
// it skips, and syncs the missing ones if confirm accepts.
func syncNewTemplatesOnLink(hubCtx *HubContext, projectTemplates []*config.Template, confirm func(prompt string) bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	missing, existing, err := splitTemplatesByHubPresence(ctx, hubCtx, projectTemplates)
	cancel()
	if err != nil {
		fmt.Printf("Warning: skipping template sync: %v\n", err)
		fmt.Println("Run 'scion templates sync <name>' to upload project templates.")
		return
	}

	if len(existing) > 0 {
		fmt.Printf("\nSkipping %d project template(s) already on the Hub:\n", len(existing))
		for _, t := range existing {
			fmt.Printf("  - %s\n", t.Name)
		}
		fmt.Println("Run 'scion templates sync <name>' to mirror one to the Hub, including removing files deleted locally.")
	}
	if len(missing) == 0 {
		return
	}

	fmt.Printf("\nFound %d project template(s) not yet on the Hub:\n", len(missing))
	for _, t := range missing {
		fmt.Printf("  - %s\n", t.Name)
	}

	if !confirm("Upload these templates to the Hub?") {
		fmt.Println("Skipping template sync.")
		fmt.Println("Run 'scion templates sync <name>' to upload project templates later.")
		return
	}

	fmt.Println("\nSyncing project templates to Hub...")
	var synced int
	for _, tpl := range missing {
		harnessType, err := detectHarnessType(tpl)
		if err != nil {
			fmt.Printf("  %s: skipped (failed to detect harness: %v)\n", tpl.Name, err)
			continue
		}

		// Only templates not yet on the Hub are synced here, so this
		// creates them and never overwrites an existing Hub template.
		err = syncTemplateToHub(hubCtx, tpl.Name, tpl.Path, "project", harnessType)
		if err != nil {
			fmt.Printf("  %s: failed: %v\n", tpl.Name, err)
			continue
		}
		synced++
	}
	fmt.Printf("%d template(s) synced to project scope.\n", synced)
}
