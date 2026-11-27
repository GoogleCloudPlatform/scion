/**
 * Copyright 2026 Google LLC
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

/**
 * Agent Trajectory Panel Component
 *
 * Lists committed Track 2 raw trajectory segments from
 * GET /api/v1/agents/{id}/trajectory/segments (gated by
 * tokenomics.trajectory_capture and project.trajectory.read), grouped by run,
 * native file path, and generation, with audit-logged signed downloads via
 * GET /api/v1/agents/{id}/trajectory/segments/{segment_id}/download.
 */

import { LitElement, html, css, nothing } from 'lit';
import type { PropertyValues, TemplateResult } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';

import type {
  TrajectorySegmentDownloadResponse,
  TrajectorySegmentSummary,
  TrajectorySegmentsListResponse,
} from '../../shared/types.js';
import { apiFetch, extractApiError } from '../../client/api.js';
import { DisplayZoneController } from '../../utils/display-zone-controller.js';
import { formatInstantWithZone, formatRelative } from '../../utils/time.js';
import { formatNumber } from '../../utils/format-number.js';

interface GenerationGroup {
  generation: number;
  segments: TrajectorySegmentSummary[];
}

interface FileGroup {
  nativeRelativePath: string;
  harness: string;
  generations: GenerationGroup[];
}

interface RunGroup {
  runId: string;
  files: FileGroup[];
}

function formatByteSize(bytes: number): string {
  if (!Number.isFinite(bytes) || bytes < 0) {
    return '0 B';
  }
  if (bytes < 1024) {
    return `${bytes} B`;
  }
  const units = ['KB', 'MB', 'GB', 'TB'];
  let value = bytes / 1024;
  let unitIdx = 0;
  while (value >= 1024 && unitIdx < units.length - 1) {
    value /= 1024;
    unitIdx++;
  }
  return `${value >= 100 ? Math.round(value) : value.toFixed(1)} ${units[unitIdx]}`;
}

function groupTrajectorySegments(segments: TrajectorySegmentSummary[]): RunGroup[] {
  const runs = new Map<
    string,
    Map<string, { harness: string; gens: Map<number, TrajectorySegmentSummary[]> }>
  >();

  for (const seg of segments) {
    let filesMap = runs.get(seg.run_id);
    if (!filesMap) {
      filesMap = new Map();
      runs.set(seg.run_id, filesMap);
    }
    let fileEntry = filesMap.get(seg.native_relative_path);
    if (!fileEntry) {
      fileEntry = { harness: seg.harness, gens: new Map() };
      filesMap.set(seg.native_relative_path, fileEntry);
    }
    let genList = fileEntry.gens.get(seg.generation);
    if (!genList) {
      genList = [];
      fileEntry.gens.set(seg.generation, genList);
    }
    genList.push(seg);
  }

  const result: RunGroup[] = [];
  for (const [runId, filesMap] of runs) {
    const files: FileGroup[] = [];
    for (const [nativeRelativePath, fileEntry] of filesMap) {
      const generations: GenerationGroup[] = Array.from(fileEntry.gens.entries())
        .sort(([a], [b]) => a - b)
        .map(([generation, segs]) => ({
          generation,
          segments: [...segs].sort((a, b) => a.byte_start - b.byte_start),
        }));
      files.push({
        nativeRelativePath,
        harness: fileEntry.harness,
        generations,
      });
    }
    result.push({ runId, files });
  }
  return result;
}

@customElement('scion-agent-trajectory-panel')
export class ScionAgentTrajectoryPanel extends LitElement {
  readonly _zone = new DisplayZoneController(this);

  @property({ type: String })
  agentId = '';

  @state()
  private loading = false;

  @state()
  private loadingMore = false;

  @state()
  private error: string | null = null;

  @state()
  private downloadError: string | null = null;

  @state()
  private downloadingSegmentId: string | null = null;

  @state()
  private segments: TrajectorySegmentSummary[] = [];

  @state()
  private nextCursor = '';

  private loadGeneration = 0;
  private loadedForAgentId = '';
  private loadingForAgentId = '';
  private hasAttemptedLoad = false;

  static override styles = css`
    :host {
      display: block;
    }

    .audit-notice {
      display: flex;
      align-items: flex-start;
      gap: 0.625rem;
      padding: 0.875rem 1rem;
      margin-bottom: 1rem;
      background: var(--scion-bg-subtle);
      border: 1px solid var(--scion-border);
      border-radius: var(--scion-radius);
      font-size: 0.875rem;
      color: var(--scion-text);
      line-height: 1.45;
    }

    .audit-notice sl-icon {
      flex-shrink: 0;
      margin-top: 0.125rem;
      color: var(--scion-warning-500, var(--scion-text-muted));
    }

    .toolbar {
      display: flex;
      flex-wrap: wrap;
      align-items: center;
      justify-content: space-between;
      gap: 0.75rem;
      margin-bottom: 1rem;
    }

    .summary-count {
      font-size: 0.875rem;
      color: var(--scion-text-muted);
    }

    .run-card {
      background: var(--scion-surface);
      border: 1px solid var(--scion-border);
      border-radius: var(--scion-radius-lg);
      padding: 1.25rem;
      margin-bottom: 1rem;
    }

    .run-header {
      display: flex;
      flex-wrap: wrap;
      align-items: center;
      justify-content: space-between;
      gap: 0.5rem;
      margin-bottom: 0.875rem;
      padding-bottom: 0.625rem;
      border-bottom: 1px solid var(--scion-border);
    }

    .run-title {
      margin: 0;
      font-size: 0.9375rem;
      font-weight: 600;
      color: var(--scion-text);
      font-family: var(--scion-font-mono);
    }

    .file-block {
      margin-bottom: 1rem;
    }

    .file-block:last-child {
      margin-bottom: 0;
    }

    .file-header {
      display: flex;
      flex-wrap: wrap;
      align-items: center;
      gap: 0.5rem;
      margin-bottom: 0.5rem;
      font-size: 0.875rem;
    }

    .file-path {
      font-family: var(--scion-font-mono);
      font-size: 0.8125rem;
      color: var(--scion-text);
      word-break: break-all;
    }

    .gen-block {
      margin-left: 0.5rem;
      margin-bottom: 0.75rem;
    }

    .gen-block:last-child {
      margin-bottom: 0;
    }

    .gen-label {
      font-size: 0.75rem;
      font-weight: 600;
      text-transform: uppercase;
      letter-spacing: 0.04em;
      color: var(--scion-text-muted);
      margin-bottom: 0.375rem;
    }

    .table-wrap {
      overflow-x: auto;
    }

    table {
      width: 100%;
      border-collapse: collapse;
      font-size: 0.875rem;
    }

    th,
    td {
      text-align: left;
      padding: 0.5rem 0.75rem;
      border-bottom: 1px solid var(--scion-border);
    }

    th {
      font-size: 0.75rem;
      font-weight: 600;
      text-transform: uppercase;
      letter-spacing: 0.04em;
      color: var(--scion-text-muted);
    }

    td.num,
    th.num {
      text-align: right;
      font-variant-numeric: tabular-nums;
      white-space: nowrap;
    }

    td.mono {
      font-family: var(--scion-font-mono);
      font-size: 0.8125rem;
      white-space: nowrap;
    }

    tbody tr:last-child td {
      border-bottom: none;
    }

    .loading-state {
      display: flex;
      align-items: center;
      gap: 0.5rem;
      padding: 1.5rem 0;
      color: var(--scion-text-muted);
      font-size: 0.875rem;
    }

    .empty-state {
      background: var(--scion-surface);
      border: 1px solid var(--scion-border);
      border-radius: var(--scion-radius-lg);
      padding: 1.5rem;
      color: var(--scion-text-muted);
      font-size: 0.875rem;
    }

    .error-state {
      display: flex;
      align-items: center;
      justify-content: space-between;
      gap: 1rem;
      padding: 0.875rem 1rem;
      margin-bottom: 1rem;
      background: var(--scion-bg-subtle);
      border: 1px solid var(--scion-border);
      border-radius: var(--scion-radius);
      color: var(--scion-danger-500);
      font-size: 0.875rem;
    }

    .load-more {
      display: flex;
      justify-content: center;
      margin-top: 0.75rem;
    }
  `;

  override updated(changed: PropertyValues): void {
    if (changed.has('agentId')) {
      const prev = changed.get('agentId') as string | undefined;
      if (prev && prev !== this.agentId) {
        this.segments = [];
        this.nextCursor = '';
        this.error = null;
        this.downloadError = null;
        this.loadedForAgentId = '';
        this.loadingForAgentId = '';
        if (this.hasAttemptedLoad && this.agentId) {
          void this.loadSegments();
        }
      }
    }
  }

  /**
   * Called by agent-detail when the Trajectory tab becomes active. Fetches once
   * per agentId unless the previous attempt failed.
   */
  public async loadIfNeeded(): Promise<void> {
    if (!this.agentId) return;
    if (this.loadedForAgentId === this.agentId && !this.error) return;
    if (this.loadingForAgentId === this.agentId) return;
    await this.loadSegments();
  }

  public async loadSegments(cursor = ''): Promise<void> {
    if (!this.agentId) return;
    this.hasAttemptedLoad = true;
    const isMore = cursor !== '';
    const gen = ++this.loadGeneration;
    const targetAgentId = this.agentId;

    if (isMore) {
      this.loadingMore = true;
    } else {
      this.loading = true;
      this.error = null;
      this.loadingForAgentId = targetAgentId;
    }

    try {
      const params = new URLSearchParams();
      if (cursor) {
        params.set('cursor', cursor);
      }
      const query = params.toString();
      const url = `/api/v1/agents/${encodeURIComponent(targetAgentId)}/trajectory/segments${query ? `?${query}` : ''}`;
      const res = await apiFetch(url);
      if (gen !== this.loadGeneration) return;
      if (!res.ok) {
        throw new Error(await extractApiError(res, `HTTP ${res.status}`));
      }
      const data = (await res.json()) as TrajectorySegmentsListResponse;
      if (gen !== this.loadGeneration) return;
      const incoming = Array.isArray(data.segments) ? data.segments : [];
      this.segments = isMore ? [...this.segments, ...incoming] : incoming;
      this.nextCursor = data.next_cursor ?? '';
      this.loadedForAgentId = targetAgentId;
    } catch (err) {
      if (gen !== this.loadGeneration) return;
      const msg = err instanceof Error ? err.message : 'Failed to load trajectory segments';
      if (isMore) {
        this.downloadError = msg;
      } else {
        this.error = msg;
      }
    } finally {
      if (gen === this.loadGeneration) {
        this.loading = false;
        this.loadingMore = false;
        if (this.loadingForAgentId === targetAgentId) {
          this.loadingForAgentId = '';
        }
      }
    }
  }

  private async handleDownload(seg: TrajectorySegmentSummary): Promise<void> {
    if (!this.agentId || this.downloadingSegmentId) return;
    this.downloadingSegmentId = seg.segment_id;
    this.downloadError = null;
    try {
      const url = `/api/v1/agents/${encodeURIComponent(this.agentId)}/trajectory/segments/${encodeURIComponent(seg.segment_id)}/download`;
      const res = await apiFetch(url);
      if (!res.ok) {
        throw new Error(await extractApiError(res, `HTTP ${res.status}`));
      }
      const data = (await res.json()) as TrajectorySegmentDownloadResponse;
      if (!data.url) {
        throw new Error('Download response did not include a signed URL');
      }
      this.triggerBrowserDownload(
        data.url,
        `g${seg.generation}-${String(seg.byte_start).padStart(16, '0')}.seg.gz`
      );
    } catch (err) {
      this.downloadError =
        err instanceof Error ? err.message : 'Failed to download trajectory segment';
    } finally {
      this.downloadingSegmentId = null;
    }
  }

  private triggerBrowserDownload(signedUrl: string, filename: string): void {
    const link = document.createElement('a');
    link.href = signedUrl;
    link.download = filename;
    link.rel = 'noopener noreferrer';
    link.style.display = 'none';
    document.body.appendChild(link);
    link.click();
    link.remove();
  }

  private renderAuditNotice(): TemplateResult {
    return html`
      <div class="audit-notice" data-testid="trajectory-audit-notice" role="note">
        <sl-icon name="exclamation-triangle"></sl-icon>
        <div>
          <strong>Unredacted audit trail:</strong> Raw trajectory segments contain verbatim harness
          session content (including user prompts, assistant outputs, and tool output). Every
          download issues a short-lived signed URL and is recorded in the Hub audit log.
        </div>
      </div>
    `;
  }

  private renderCommittedAt(iso?: string): string {
    if (!iso) return '—';
    return `${formatRelative(iso)} (${formatInstantWithZone(iso)})`;
  }

  override render(): TemplateResult {
    const groups = groupTrajectorySegments(this.segments);

    return html`
      ${this.renderAuditNotice()}

      <div class="toolbar">
        <span class="summary-count" data-testid="trajectory-segment-count">
          ${this.segments.length === 1
            ? '1 committed segment'
            : `${formatNumber(this.segments.length)} committed segments`}
        </span>
        <sl-button
          size="small"
          variant="default"
          ?loading=${this.loading}
          @click=${(): void => {
            void this.loadSegments();
          }}
          data-testid="trajectory-refresh-btn"
        >
          <sl-icon slot="prefix" name="arrow-clockwise"></sl-icon>
          Refresh
        </sl-button>
      </div>

      ${this.downloadError
        ? html`
            <div class="error-state" data-testid="trajectory-download-error" role="alert">
              <span>${this.downloadError}</span>
              <sl-button
                size="small"
                variant="default"
                @click=${(): void => {
                  this.downloadError = null;
                }}
              >
                Dismiss
              </sl-button>
            </div>
          `
        : nothing}
      ${this.loading && this.segments.length === 0
        ? html`
            <div class="loading-state" data-testid="trajectory-loading">
              <sl-spinner></sl-spinner>
              <span>Loading trajectory segments…</span>
            </div>
          `
        : this.error
          ? html`
              <div class="error-state" data-testid="trajectory-error" role="alert">
                <span>${this.error}</span>
                <sl-button
                  size="small"
                  variant="default"
                  @click=${(): void => {
                    void this.loadSegments();
                  }}
                >
                  Retry
                </sl-button>
              </div>
            `
          : groups.length === 0
            ? html`
                <div class="empty-state" data-testid="trajectory-empty">
                  No committed raw trajectory segments have been captured for this agent yet.
                </div>
              `
            : html`
                ${groups.map(
                  (run) => html`
                    <section
                      class="run-card"
                      data-testid="trajectory-run-group"
                      data-run-id=${run.runId}
                    >
                      <div class="run-header">
                        <h3 class="run-title">Run ${run.runId}</h3>
                      </div>
                      ${run.files.map(
                        (file) => html`
                          <div
                            class="file-block"
                            data-testid="trajectory-file-group"
                            data-file-path=${file.nativeRelativePath}
                          >
                            <div class="file-header">
                              <sl-icon name="file-earmark-code"></sl-icon>
                              <span class="file-path">${file.nativeRelativePath}</span>
                              <sl-badge variant="neutral">${file.harness}</sl-badge>
                            </div>
                            ${file.generations.map(
                              (gen) => html`
                                <div
                                  class="gen-block"
                                  data-testid="trajectory-gen-group"
                                  data-generation=${String(gen.generation)}
                                >
                                  <div class="gen-label">Generation ${gen.generation}</div>
                                  <div class="table-wrap">
                                    <table>
                                      <thead>
                                        <tr>
                                          <th>Byte Range</th>
                                          <th class="num">Raw Size</th>
                                          <th class="num">Compressed</th>
                                          <th>Committed</th>
                                          <th class="num">Action</th>
                                        </tr>
                                      </thead>
                                      <tbody>
                                        ${gen.segments.map(
                                          (seg) => html`
                                            <tr
                                              data-testid="trajectory-segment-row"
                                              data-segment-id=${seg.segment_id}
                                            >
                                              <td class="mono">
                                                ${formatNumber(seg.byte_start)} –
                                                ${formatNumber(seg.byte_end)}
                                                ${seg.incomplete_tail
                                                  ? html`<sl-badge variant="warning"
                                                      >incomplete tail</sl-badge
                                                    >`
                                                  : nothing}
                                              </td>
                                              <td class="num">${formatByteSize(seg.raw_size)}</td>
                                              <td class="num">
                                                ${formatByteSize(seg.compressed_size)}
                                              </td>
                                              <td>${this.renderCommittedAt(seg.committed_at)}</td>
                                              <td class="num">
                                                <sl-button
                                                  size="small"
                                                  variant="default"
                                                  ?loading=${this.downloadingSegmentId ===
                                                  seg.segment_id}
                                                  ?disabled=${this.downloadingSegmentId !== null &&
                                                  this.downloadingSegmentId !== seg.segment_id}
                                                  data-testid="trajectory-download-btn"
                                                  @click=${(): void => {
                                                    void this.handleDownload(seg);
                                                  }}
                                                >
                                                  <sl-icon slot="prefix" name="download"></sl-icon>
                                                  Download
                                                </sl-button>
                                              </td>
                                            </tr>
                                          `
                                        )}
                                      </tbody>
                                    </table>
                                  </div>
                                </div>
                              `
                            )}
                          </div>
                        `
                      )}
                    </section>
                  `
                )}
                ${this.nextCursor
                  ? html`
                      <div class="load-more">
                        <sl-button
                          size="small"
                          variant="default"
                          ?loading=${this.loadingMore}
                          data-testid="trajectory-load-more-btn"
                          @click=${(): void => {
                            void this.loadSegments(this.nextCursor);
                          }}
                        >
                          Load more segments
                        </sl-button>
                      </div>
                    `
                  : nothing}
              `}
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-agent-trajectory-panel': ScionAgentTrajectoryPanel;
  }
}
