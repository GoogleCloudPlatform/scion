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

import { describe, it, expect, beforeAll, beforeEach, afterEach, vi } from 'vitest';
import type { ScionAgentTrajectoryPanel } from './agent-trajectory-panel.js';

describe('scion-agent-trajectory-panel', () => {
  let container: HTMLElement;
  let fetchMock: ReturnType<typeof vi.fn>;

  beforeAll(async () => {
    await import('./agent-trajectory-panel.js');
  }, 30_000);

  beforeEach(() => {
    container = document.createElement('div');
    document.body.appendChild(container);
    fetchMock = vi.fn();
    vi.stubGlobal('fetch', fetchMock);
  });

  afterEach(() => {
    container.remove();
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
  });

  it('renders unredacted audit notice and groups segments by run, file path, and generation', async () => {
    fetchMock.mockResolvedValueOnce(
      new Response(
        JSON.stringify({
          agent_id: 'agent-1',
          project_id: 'proj-1',
          segments: [
            {
              segment_id: 'seg-1',
              run_id: 'run-a',
              harness: 'claude',
              native_relative_path: '.claude/projects/-workspace/sess-1.jsonl',
              artifact_kind: 'jsonl_append',
              generation: 0,
              byte_start: 0,
              byte_end: 2048,
              raw_size: 2048,
              compressed_size: 512,
              raw_sha256: 'a'.repeat(64),
              compressed_sha256: 'b'.repeat(64),
              incomplete_tail: false,
              committed_at: '2026-11-27T12:00:00Z',
            },
            {
              segment_id: 'seg-2',
              run_id: 'run-a',
              harness: 'claude',
              native_relative_path: '.claude/projects/-workspace/sess-1.jsonl',
              artifact_kind: 'jsonl_append',
              generation: 1,
              byte_start: 0,
              byte_end: 1024,
              raw_size: 1024,
              compressed_size: 256,
              raw_sha256: 'c'.repeat(64),
              compressed_sha256: 'd'.repeat(64),
              incomplete_tail: true,
              committed_at: '2026-11-27T12:05:00Z',
            },
            {
              segment_id: 'seg-3',
              run_id: 'run-b',
              harness: 'claude',
              native_relative_path: '.claude/projects/-workspace/sess-2.jsonl',
              artifact_kind: 'jsonl_append',
              generation: 0,
              byte_start: 0,
              byte_end: 500,
              raw_size: 500,
              compressed_size: 200,
              raw_sha256: 'e'.repeat(64),
              compressed_sha256: 'f'.repeat(64),
              incomplete_tail: false,
              committed_at: '2026-11-27T13:00:00Z',
            },
          ],
        }),
        { status: 200, headers: { 'Content-Type': 'application/json' } }
      )
    );

    const el = document.createElement('scion-agent-trajectory-panel') as ScionAgentTrajectoryPanel;
    el.agentId = 'agent-1';
    container.appendChild(el);
    await el.loadIfNeeded();
    await el.updateComplete;

    const notice = el.shadowRoot!.querySelector('[data-testid="trajectory-audit-notice"]');
    expect(notice?.textContent).toContain('Unredacted audit trail');
    expect(notice?.textContent).toContain('audit log');

    const runGroups = el.shadowRoot!.querySelectorAll('[data-testid="trajectory-run-group"]');
    expect(runGroups.length).toBe(2);
    expect(runGroups[0].getAttribute('data-run-id')).toBe('run-a');
    expect(runGroups[1].getAttribute('data-run-id')).toBe('run-b');

    const runAGens = runGroups[0].querySelectorAll('[data-testid="trajectory-gen-group"]');
    expect(runAGens.length).toBe(2);
    expect(runAGens[0].getAttribute('data-generation')).toBe('0');
    expect(runAGens[1].getAttribute('data-generation')).toBe('1');

    const rows = el.shadowRoot!.querySelectorAll('[data-testid="trajectory-segment-row"]');
    expect(rows.length).toBe(3);
    expect(rows[0].textContent?.replace(/\s+/g, ' ')).toContain('0 – 2,048');
    expect(rows[0].textContent).toContain('2.0 KB');
    expect(rows[1].textContent).toContain('incomplete tail');
  });

  it('calls the signed download route when clicking Download on a segment', async () => {
    fetchMock
      .mockResolvedValueOnce(
        new Response(
          JSON.stringify({
            agent_id: 'agent-1',
            project_id: 'proj-1',
            segments: [
              {
                segment_id: 'seg-dl-1',
                run_id: 'run-a',
                harness: 'claude',
                native_relative_path: '.claude/projects/-workspace/sess-1.jsonl',
                artifact_kind: 'jsonl_append',
                generation: 0,
                byte_start: 0,
                byte_end: 128,
                raw_size: 128,
                compressed_size: 64,
                raw_sha256: 'a'.repeat(64),
                compressed_sha256: 'b'.repeat(64),
                incomplete_tail: false,
                committed_at: '2026-11-27T12:00:00Z',
              },
            ],
          }),
          { status: 200, headers: { 'Content-Type': 'application/json' } }
        )
      )
      .mockResolvedValueOnce(
        new Response(
          JSON.stringify({
            segment_id: 'seg-dl-1',
            object_path: 'hub/trajectories/v1/raw/.../g0-0000000000000000.seg.gz',
            url: 'https://storage.example/signed-get?sig=abc',
            method: 'GET',
            expires_at: '2026-11-27T12:10:00Z',
            content_type: 'application/gzip',
            compressed_size: 64,
            raw_sha256: 'a'.repeat(64),
            compressed_sha256: 'b'.repeat(64),
          }),
          { status: 200, headers: { 'Content-Type': 'application/json' } }
        )
      );

    const el = document.createElement('scion-agent-trajectory-panel') as ScionAgentTrajectoryPanel;
    el.agentId = 'agent-1';
    const triggerSpy = vi
      .spyOn(
        el as unknown as { triggerBrowserDownload: (u: string, f: string) => void },
        'triggerBrowserDownload'
      )
      .mockImplementation(() => {});
    container.appendChild(el);
    await el.loadIfNeeded();
    await el.updateComplete;

    const btn = el.shadowRoot!.querySelector(
      '[data-testid="trajectory-download-btn"]'
    ) as HTMLElement;
    expect(btn).not.toBeNull();
    btn.click();

    await vi.waitFor(() => {
      expect(fetchMock).toHaveBeenCalledWith(
        '/api/v1/agents/agent-1/trajectory/segments/seg-dl-1/download',
        expect.any(Object)
      );
      expect(triggerSpy).toHaveBeenCalledWith(
        'https://storage.example/signed-get?sig=abc',
        'g0-0000000000000000.seg.gz'
      );
    });
  });

  it('renders empty state when no committed segments exist', async () => {
    fetchMock.mockResolvedValueOnce(
      new Response(
        JSON.stringify({
          agent_id: 'agent-1',
          project_id: 'proj-1',
          segments: [],
        }),
        { status: 200, headers: { 'Content-Type': 'application/json' } }
      )
    );

    const el = document.createElement('scion-agent-trajectory-panel') as ScionAgentTrajectoryPanel;
    el.agentId = 'agent-1';
    container.appendChild(el);
    await el.loadIfNeeded();
    await el.updateComplete;

    expect(el.shadowRoot!.querySelector('[data-testid="trajectory-empty"]')).not.toBeNull();
  });
});
