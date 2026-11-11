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

/**
 * In-page recorders for the async create journey, installed with
 * addInitScript before the app loads:
 *
 * - SSE: window.EventSource is wrapped. Each `update` event whose data
 *   names an agent is recorded (time, subject, agentId, phase, activity,
 *   launch) in window.__e2eSse. Only those fields are kept.
 * - DOM: a sampler reads the agent page's shadow root every 50 ms
 *   and appends a row to window.__e2eTimeline whenever what it reads
 *   changes: path, Status tab phase, header lifecycle controls present,
 *   launch banner and step text, incomplete create panel and its code.
 *   Every element is read by its data-testid (selectors.ts).
 * - Document id: window.__e2eDocId is a random id per document. It is the
 *   same at the end only if the page was never reloaded or navigated as a
 *   document (the app's client-side routing keeps it).
 *
 * The recorders live in the page, so a reload would also empty them.
 */

import type { Page } from '@playwright/test';

import { FORBIDDEN_HEADER_CONTROLS, SELECTORS } from './selectors.js';

export interface SseLaunchRecord {
  t: number;
  subject: string;
  agentId: string;
  phase?: string;
  activity?: string;
  launch?: {
    state?: string;
    active?: boolean;
    kind?: string;
    step?: string;
    error?: string;
    endReason?: string;
  } | null;
}

export interface TimelineRow {
  t: number;
  path: string;
  /** Status tab phase badge (agent-phase status attribute). */
  phase: string;
  /** Header lifecycle controls present, by test id (header-start, ...). */
  headerControls: string[];
  launchBanner: boolean;
  /** launch-step text ("Step: <step label>"; see uiStepName). */
  step: string;
  incompleteBanner: boolean;
  /** launch-code text (the failure code alone). */
  errorCode: string;
}

interface RecorderSelectors {
  launchBanner: string;
  launchStep: string;
  incompleteBanner: string;
  launchCode: string;
  agentPhase: string;
  /** Header control test id -> selector. */
  headerControls: Record<string, string>;
}

/** The init script. Runs in the browser; it must not close over anything. */
function installRecorders(sel: RecorderSelectors): void {
  const w = window as unknown as Record<string, unknown>;
  if (w.__e2eDocId) return;
  w.__e2eDocId = `${Date.now()}-${Math.random().toString(36).slice(2)}`;
  const sse: unknown[] = [];
  const timeline: unknown[] = [];
  w.__e2eSse = sse;
  w.__e2eTimeline = timeline;

  // SSE wrapper.
  const Original = window.EventSource;
  class RecordingEventSource extends Original {
    constructor(url: string | URL, init?: EventSourceInit) {
      super(url, init);
      this.addEventListener('update', (event: MessageEvent<string>) => {
        try {
          const parsed = JSON.parse(event.data) as { subject?: string; data?: unknown };
          const data = (parsed.data ?? {}) as Record<string, unknown>;
          if (typeof data.agentId !== 'string') return;
          const launch = data.launch as Record<string, unknown> | null | undefined;
          sse.push({
            t: Date.now(),
            subject: String(parsed.subject ?? ''),
            agentId: data.agentId,
            phase: typeof data.phase === 'string' ? data.phase : undefined,
            activity: typeof data.activity === 'string' ? data.activity : undefined,
            launch:
              launch && typeof launch === 'object'
                ? {
                    state: launch.state,
                    active: launch.active,
                    kind: launch.kind,
                    step: launch.step,
                    error: launch.error,
                    endReason: launch.endReason,
                  }
                : launch === null
                  ? null
                  : undefined,
          });
        } catch {
          // Not JSON: not an update this recorder reads.
        }
      });
    }
  }
  window.EventSource = RecordingEventSource;

  // DOM sampler.
  const deepQuery = (root: ParentNode, selector: string): Element | null => {
    const found = root.querySelector(selector);
    if (found) return found;
    for (const el of Array.from(root.querySelectorAll('*'))) {
      if (el.shadowRoot) {
        const inner = deepQuery(el.shadowRoot, selector);
        if (inner) return inner;
      }
    }
    return null;
  };
  // The agent page element is found once (a full shadow-tree walk) and
  // kept while it stays connected; every selector read below lives directly
  // in its shadow root, so a sample is a few querySelector calls.
  let pageEl: Element | null = null;
  let lastLookup = 0;
  let last = '';
  const sample = (): void => {
    if (!pageEl?.isConnected) {
      pageEl = null;
      const now = Date.now();
      // Walk the tree at most every 200 ms while the page is not there.
      if (now - lastLookup >= 200) {
        lastLookup = now;
        pageEl = deepQuery(document, 'scion-page-agent-detail');
      }
    }
    const root: ParentNode | null = pageEl?.shadowRoot ?? null;
    const q = (s: string): Element | null => (root ? root.querySelector(s) : null);
    const text = (s: string): string => (q(s)?.textContent ?? '').replace(/\s+/g, ' ').trim();
    const row = {
      path: location.pathname,
      phase: q(sel.agentPhase)?.getAttribute('status') ?? '',
      headerControls: Object.keys(sel.headerControls).filter((id) => !!q(sel.headerControls[id])),
      launchBanner: !!q(sel.launchBanner),
      step: text(sel.launchStep),
      incompleteBanner: !!q(sel.incompleteBanner),
      errorCode: text(sel.launchCode),
    };
    const key = JSON.stringify(row);
    if (key !== last) {
      last = key;
      timeline.push({ t: Date.now(), ...row });
    }
  };
  setInterval(sample, 50);
}

/** Installs the recorders in every document the page loads. */
export async function installPageRecorders(page: Page): Promise<void> {
  const sel: RecorderSelectors = {
    launchBanner: SELECTORS.launchBanner,
    launchStep: SELECTORS.launchStep,
    incompleteBanner: SELECTORS.incompleteBanner,
    launchCode: SELECTORS.launchCode,
    agentPhase: SELECTORS.agentPhase,
    headerControls: { ...FORBIDDEN_HEADER_CONTROLS },
  };
  await page.addInitScript(installRecorders, sel);
}

/** The current document's id (changes on a reload or document navigation). */
export async function documentId(page: Page): Promise<string> {
  return page.evaluate(() => String((window as unknown as { __e2eDocId?: string }).__e2eDocId));
}

/** SSE records for one agent. */
export async function sseRecords(page: Page, agentId: string): Promise<SseLaunchRecord[]> {
  const all = await page.evaluate(
    () => ((window as unknown as { __e2eSse?: unknown[] }).__e2eSse ?? []) as SseLaunchRecord[]
  );
  return all.filter((r) => r.agentId === agentId);
}

/** DOM timeline rows recorded on the given agent's page. */
export async function timelineRows(page: Page, agentId: string): Promise<TimelineRow[]> {
  const all = await page.evaluate(
    () =>
      ((window as unknown as { __e2eTimeline?: unknown[] }).__e2eTimeline ?? []) as TimelineRow[]
  );
  return all.filter((r) => r.path.endsWith(`/agents/${agentId}`));
}
