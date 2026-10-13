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

package hub

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// template_blob_gc.go deletes template blobs that no manifest references and
// abandoned staged uploads (ptone/scion#4221). A commit never deletes blobs,
// so a hydration that read an older manifest keeps finding its files; this
// collector removes them once they are older than the grace period.
//
// An object's age is measured from max(Created, Updated). A commit that
// re-references a blob that was present but unreferenced rewrites it first
// (F5b), which resets its age, so a collection pass that saw it as old and
// unreferenced cannot delete it before the commit lands. The collector also
// re-reads the row before deleting and never deletes a blob the current row
// references, so it is safe with several hub replicas running it at once.

const (
	// defaultTemplateBlobGCGrace is the default grace period. It is far
	// longer than a signed URL's lifetime (SignedURLExpiry), so a download
	// that started from an older manifest finishes long before its blobs
	// can be collected.
	defaultTemplateBlobGCGrace = 24 * time.Hour
	// minTemplateBlobGCGrace is the shortest grace period the collector
	// accepts. A commit writes its blobs before its compare-and-swap; a
	// grace shorter than the time between the two could delete a blob
	// that the commit then references. It is also well above
	// SignedURLExpiry, so a download in flight keeps its blobs.
	minTemplateBlobGCGrace = time.Hour
	// templateBlobGCInterval is how often the hub runs the collector.
	templateBlobGCInterval = time.Hour
	// templateBlobGCPageSize is the template list page size of one pass.
	templateBlobGCPageSize = 500
)

// templateBlobGCReport counts what one collection pass did.
type templateBlobGCReport struct {
	Templates     int `json:"templates"`
	DeletedBlobs  int `json:"deletedBlobs"`
	KeptYoung     int `json:"keptYoung"`
	DeletedStaged int `json:"deletedStaged"`
	Errors        int `json:"errors"`
}

// templateBlobGCGrace returns the configured grace period, at least
// minTemplateBlobGCGrace.
func (s *Server) templateBlobGCGrace() time.Duration {
	if g := s.config.TemplateBlobGCGrace; g > 0 {
		return clampTemplateBlobGCGrace(g)
	}
	return defaultTemplateBlobGCGrace
}

// clampTemplateBlobGCGrace raises a grace period below the minimum to the
// minimum. Every collection pass goes through it.
func clampTemplateBlobGCGrace(grace time.Duration) time.Duration {
	if grace < minTemplateBlobGCGrace {
		return minTemplateBlobGCGrace
	}
	return grace
}

// startTemplateBlobGC runs the collector every templateBlobGCInterval until
// ctx ends.
func (s *Server) startTemplateBlobGC(ctx context.Context) {
	if g := s.config.TemplateBlobGCGrace; g > 0 && g < minTemplateBlobGCGrace {
		s.templateLog.Warn("template blob gc: configured grace period is below the minimum; using the minimum",
			"configured", g, "minimum", minTemplateBlobGCGrace)
	}
	go func() {
		ticker := time.NewTicker(templateBlobGCInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				report, err := s.collectTemplateBlobGarbage(ctx, s.templateBlobGCGrace(), time.Now())
				if err != nil {
					s.templateLog.Warn("template blob gc: pass failed", "error", err)
				} else if report.DeletedBlobs > 0 || report.DeletedStaged > 0 {
					s.templateLog.Info("template blob gc: pass complete",
						"deletedBlobs", report.DeletedBlobs, "deletedStaged", report.DeletedStaged)
				}
			}
		}
	}()
}

// templateObjectAge is now - max(Created, Updated). An object without
// either time has an unknown age and counts as new, so it is never deleted.
func templateObjectAge(obj storage.Object, now time.Time) time.Duration {
	t := obj.Created
	if obj.Updated.After(t) {
		t = obj.Updated
	}
	if t.IsZero() {
		return 0
	}
	return now.Sub(t)
}

// templateBlobRefs returns the hex digests t's manifest references (none for
// a legacy row, whose files are not blobs).
func templateBlobRefs(t *store.Template) map[string]bool {
	refs := make(map[string]bool)
	if !isBlobLayout(t) {
		return refs
	}
	for _, f := range t.Files {
		if hex, ok := templateBlobHex(f.Hash); ok {
			refs[hex] = true
		}
	}
	return refs
}

// collectTemplateBlobGarbage runs one collection pass over every template.
// For each row it deletes the blobs under its content base that its
// manifest does not reference and that are older than grace, and staged
// uploads older than grace.
func (s *Server) collectTemplateBlobGarbage(ctx context.Context, grace time.Duration, now time.Time) (templateBlobGCReport, error) {
	grace = clampTemplateBlobGCGrace(grace)
	var report templateBlobGCReport
	stor := s.GetStorage()
	if stor == nil {
		return report, nil
	}
	cursor := ""
	for {
		res, err := s.store.ListTemplates(ctx, store.TemplateFilter{}, store.ListOptions{
			Limit:          templateBlobGCPageSize,
			Cursor:         cursor,
			SkipTotalCount: true,
		})
		if err != nil {
			return report, fmt.Errorf("list templates: %w", err)
		}
		if res == nil {
			return report, nil
		}
		for i := range res.Items {
			if err := ctx.Err(); err != nil {
				return report, err
			}
			report.Templates++
			s.collectTemplateGarbage(ctx, stor, &res.Items[i], grace, now, &report)
		}
		if res.NextCursor == "" {
			return report, nil
		}
		cursor = res.NextCursor
	}
}

// collectTemplateGarbage runs the collector for one template row.
func (s *Server) collectTemplateGarbage(ctx context.Context, stor storage.Storage, t *store.Template, grace time.Duration, now time.Time, report *templateBlobGCReport) {
	if t.ID == "" || (isBlobLayout(t) && t.StoragePath == "") {
		return
	}
	base := s.templateContentBase(t)

	// Unreferenced, aged blobs.
	blobPrefix := templateBlobPrefix(base)
	listed, err := stor.List(ctx, storage.ListOptions{Prefix: blobPrefix})
	if err != nil {
		report.Errors++
		s.templateLog.Warn("template blob gc: list blobs failed", "template", t.ID, "error", err)
		return
	}
	refs := templateBlobRefs(t)
	var candidates []storage.Object
	for _, obj := range listed.Objects {
		hex := strings.TrimPrefix(obj.Name, blobPrefix)
		if hex == "" || strings.Contains(hex, "/") || refs[hex] {
			continue
		}
		if templateObjectAge(obj, now) <= grace {
			report.KeptYoung++
			continue
		}
		candidates = append(candidates, obj)
	}
	if len(candidates) > 0 {
		// Re-read the row: a commit may have referenced a candidate since
		// the list above.
		fresh, err := s.store.GetTemplate(ctx, t.ID)
		switch {
		case errors.Is(err, store.ErrNotFound):
			candidates = nil
		case err != nil:
			report.Errors++
			s.templateLog.Warn("template blob gc: re-read failed", "template", t.ID, "error", err)
			candidates = nil
		case s.templateContentBase(fresh) != base:
			// The row moved to another path since the list; the next pass
			// looks at it again.
			candidates = nil
		default:
			refs = templateBlobRefs(fresh)
		}
		for _, obj := range candidates {
			if refs[strings.TrimPrefix(obj.Name, blobPrefix)] {
				continue
			}
			if s.deleteAgedTemplateObject(ctx, stor, obj, grace, now, report) {
				report.DeletedBlobs++
			}
		}
	}

	// Abandoned staged uploads.
	staged, err := stor.List(ctx, storage.ListOptions{Prefix: templateStagingPrefix(base)})
	if err != nil {
		report.Errors++
		s.templateLog.Warn("template blob gc: list staged uploads failed", "template", t.ID, "error", err)
		return
	}
	for _, obj := range staged.Objects {
		if templateObjectAge(obj, now) <= grace {
			continue
		}
		if s.deleteAgedTemplateObject(ctx, stor, obj, grace, now, report) {
			report.DeletedStaged++
		}
	}
}

// deleteAgedTemplateObject deletes obj only if it is still the object the
// list saw and still older than grace. On providers with object generations
// the delete is conditional on the listed generation, so a rewrite since the
// list (an F5b refresh) wins. Otherwise (local storage) the object is
// re-read just before the delete and kept if it has been rewritten within
// the grace period. Local storage has no conditional delete, so a refresh
// that lands between that re-read and the delete is not seen: the blob is
// deleted and the refreshing commit's row references a missing blob until
// the file is pushed again. The window is the few microseconds between two
// filesystem calls, on a hub with local storage.
func (s *Server) deleteAgedTemplateObject(ctx context.Context, stor storage.Storage, obj storage.Object, grace time.Duration, now time.Time, report *templateBlobGCReport) bool {
	if gd, ok := stor.(storage.GenerationDeleter); ok && obj.Generation != 0 {
		err := gd.DeleteIfGeneration(ctx, obj.Name, obj.Generation)
		switch {
		case err == nil:
			return true
		case errors.Is(err, storage.ErrNotFound), errors.Is(err, storage.ErrPreconditionFailed):
			return false
		default:
			report.Errors++
			s.templateLog.Warn("template blob gc: delete failed", "object", obj.Name, "error", err)
			return false
		}
	}
	cur, err := stor.GetObject(ctx, obj.Name)
	if errors.Is(err, storage.ErrNotFound) {
		return false
	}
	if err != nil {
		report.Errors++
		s.templateLog.Warn("template blob gc: stat failed", "object", obj.Name, "error", err)
		return false
	}
	if templateObjectAge(*cur, now) <= grace {
		return false
	}
	if err := stor.Delete(ctx, obj.Name); err != nil {
		if !errors.Is(err, storage.ErrNotFound) {
			report.Errors++
			s.templateLog.Warn("template blob gc: delete failed", "object", obj.Name, "error", err)
		}
		return false
	}
	return true
}

// TemplateBlobGCExecutor runs one template blob garbage collection pass as a
// maintenance operation. The optional "grace" parameter (a Go duration such
// as "48h") overrides the configured grace period for that run.
type TemplateBlobGCExecutor struct {
	srv *Server
}

// Run implements MaintenanceExecutor.
func (e *TemplateBlobGCExecutor) Run(ctx context.Context, logger io.Writer, params map[string]string) error {
	grace := e.srv.templateBlobGCGrace()
	if v := params["grace"]; v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < minTemplateBlobGCGrace {
			return fmt.Errorf("invalid grace %q: want a duration of at least %s, such as 24h", v, minTemplateBlobGCGrace)
		}
		grace = d
	}
	_, _ = fmt.Fprintf(logger, "Collecting unreferenced template blobs older than %s\n", grace)
	report, err := e.srv.collectTemplateBlobGarbage(ctx, grace, time.Now())
	_, _ = fmt.Fprintf(logger, "Templates: %d, deleted blobs: %d, kept (within grace): %d, deleted staged uploads: %d, errors: %d\n",
		report.Templates, report.DeletedBlobs, report.KeptYoung, report.DeletedStaged, report.Errors)
	return err
}
