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

import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import {
  TOKENOMICS_TRAJECTORY_CAPTURE_FLAG,
  TOKENOMICS_USAGE_LEDGER_FLAG,
  setFeatureFlag,
  resetServerFlagStateForTests,
} from '../../utils/feature-flags.js';
import { formatTrajectoryCaptureNotice } from '../../shared/trajectory-capture-notice.js';
import type { TrajectoryCapturePolicyStatus } from '../../shared/types.js';

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

describe('Trajectory capture notice banner (Usage tab only)', () => {
  const origEventSource = globalThis.EventSource;

  beforeEach(() => {
    class MockEventSource {
      addEventListener(): void {
        // no-op
      }
      removeEventListener(): void {
        // no-op
      }
      close(): void {
        // no-op
      }
    }
    vi.stubGlobal('EventSource', MockEventSource);
  });

  afterEach(() => {
    vi.restoreAllMocks();
    if (origEventSource) {
      vi.stubGlobal('EventSource', origEventSource);
    } else {
      vi.unstubAllGlobals();
    }
    setFeatureFlag(TOKENOMICS_TRAJECTORY_CAPTURE_FLAG, false);
    setFeatureFlag(TOKENOMICS_USAGE_LEDGER_FLAG, false);
    resetServerFlagStateForTests();
    document.body.innerHTML = '';
  });

  describe('agent-detail Usage tab banner', () => {
    beforeEach(async () => {
      await import('./agent-detail.js');
    });

    async function mountAgentDetail(
      trajectoryCapture?: TrajectoryCapturePolicyStatus
    ): Promise<HTMLElement & { updateComplete: Promise<boolean> }> {
      vi.spyOn(globalThis, 'fetch').mockImplementation((input) => {
        const url =
          typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
        if (url.endsWith('/api/v1/agents/agent-1')) {
          return Promise.resolve(
            jsonResponse({
              id: 'agent-1',
              name: 'test-agent',
              slug: 'test-agent',
              projectId: 'proj-1',
              project: 'Test Project',
              template: 'claude',
              phase: 'running',
              ...(trajectoryCapture ? { trajectory_capture: trajectoryCapture } : {}),
            })
          );
        }
        return Promise.resolve(jsonResponse({ error: 'not found' }, 404));
      });

      const el = document.createElement('scion-page-agent-detail') as HTMLElement & {
        agentId: string;
        updateComplete: Promise<boolean>;
      };
      el.agentId = 'agent-1';
      document.body.appendChild(el);

      await vi.waitFor(() => {
        expect(el.shadowRoot?.querySelector('sl-tab-group')).not.toBeNull();
      });
      await el.updateComplete;
      return el;
    }

    it('hides banner when trajectory capture experiment is off even if Usage tab is visible and payload has trajectory_capture', async () => {
      setFeatureFlag(TOKENOMICS_USAGE_LEDGER_FLAG, true);
      setFeatureFlag(TOKENOMICS_TRAJECTORY_CAPTURE_FLAG, false);
      const el = await mountAgentDetail({ policy: 'full', enforced_by_hub: false });
      expect(el.shadowRoot!.querySelector('[data-testid="agent-usage-panel"]')).not.toBeNull();
      expect(el.shadowRoot!.querySelector('[data-testid="trajectory-capture-banner"]')).toBeNull();
    });

    it('hides banner entirely when Usage tab is hidden even if trajectory capture is on', async () => {
      setFeatureFlag(TOKENOMICS_USAGE_LEDGER_FLAG, false);
      setFeatureFlag(TOKENOMICS_TRAJECTORY_CAPTURE_FLAG, true);
      const el = await mountAgentDetail({ policy: 'full', enforced_by_hub: true });
      expect(el.shadowRoot!.querySelector('[data-testid="agent-usage-panel"]')).toBeNull();
      expect(el.shadowRoot!.querySelector('[data-testid="trajectory-capture-banner"]')).toBeNull();
    });

    it('hides banner when both experiments are on but trajectory_capture is absent', async () => {
      setFeatureFlag(TOKENOMICS_USAGE_LEDGER_FLAG, true);
      setFeatureFlag(TOKENOMICS_TRAJECTORY_CAPTURE_FLAG, true);
      const el = await mountAgentDetail(undefined);
      expect(el.shadowRoot!.querySelector('[data-testid="agent-usage-panel"]')).not.toBeNull();
      expect(el.shadowRoot!.querySelector('[data-testid="trajectory-capture-banner"]')).toBeNull();
    });

    for (const status of [
      { policy: 'full', enforced_by_hub: false },
      { policy: 'full', enforced_by_hub: true },
      { policy: 'metadata_only', enforced_by_hub: false },
      { policy: 'off', enforced_by_hub: false },
    ] satisfies TrajectoryCapturePolicyStatus[]) {
      it(`renders canonical notice inside the Usage tab only for policy=${status.policy} enforced=${status.enforced_by_hub}`, async () => {
        setFeatureFlag(TOKENOMICS_USAGE_LEDGER_FLAG, true);
        setFeatureFlag(TOKENOMICS_TRAJECTORY_CAPTURE_FLAG, true);
        const el = await mountAgentDetail(status);

        // Banner is rendered exclusively inside the Usage tab panel (never above tabs).
        const allBanners = el.shadowRoot!.querySelectorAll(
          '[data-testid="trajectory-capture-banner"]'
        );
        expect(allBanners).toHaveLength(1);

        const usagePanel = el.shadowRoot!.querySelector(
          '[data-testid="agent-usage-panel"]'
        ) as HTMLElement | null;
        expect(usagePanel).not.toBeNull();

        const banner = usagePanel!.querySelector(
          '[data-testid="trajectory-capture-banner"]'
        ) as HTMLElement | null;
        expect(banner).not.toBeNull();
        expect(banner!.closest('sl-tab-panel')?.getAttribute('name')).toBe('usage');
        expect(banner!.getAttribute('data-policy')).toBe(status.policy);
        expect(banner!.getAttribute('data-enforced')).toBe(String(status.enforced_by_hub));
        expect(banner!.textContent).toContain(formatTrajectoryCaptureNotice(status));
      });
    }
  });

  describe('terminal-pane has no capture banner or input gating', () => {
    beforeEach(async () => {
      await import('../terminal/terminal-pane.js');
    });

    async function mountTerminalPane(
      trajectoryCapture?: TrajectoryCapturePolicyStatus
    ): Promise<HTMLElement & { updateComplete: Promise<boolean> }> {
      const el = document.createElement('scion-terminal-pane') as HTMLElement & {
        loading: boolean;
        agent: Record<string, unknown>;
        updateComplete: Promise<boolean>;
      };
      el.loading = false;
      el.agent = {
        id: 'agent-1',
        name: 'test-agent',
        projectId: 'proj-1',
        template: 'claude',
        phase: 'running',
        ...(trajectoryCapture ? { trajectory_capture: trajectoryCapture } : {}),
      };
      document.body.appendChild(el);
      await el.updateComplete;
      return el;
    }

    it('never renders a capture banner in the terminal pane even when experiments are on', async () => {
      setFeatureFlag(TOKENOMICS_USAGE_LEDGER_FLAG, true);
      setFeatureFlag(TOKENOMICS_TRAJECTORY_CAPTURE_FLAG, true);
      const el = await mountTerminalPane({ policy: 'full', enforced_by_hub: true });
      expect(
        el.shadowRoot!.querySelector('[data-testid="terminal-trajectory-capture-banner"]')
      ).toBeNull();
      expect(el.shadowRoot!.querySelector('[data-testid="trajectory-capture-banner"]')).toBeNull();
    });

    it('does not gate terminal input before agent metadata arrives when trajectory capture is on', async () => {
      setFeatureFlag(TOKENOMICS_TRAJECTORY_CAPTURE_FLAG, true);
      const sendDataSpy = vi.fn();
      const el = document.createElement('scion-terminal-pane') as HTMLElement & {
        loading: boolean;
        agent: Record<string, unknown> | null;
        session: { sendData: (d: string) => void } | null;
        sendData: (d: string) => void;
        updateComplete: Promise<boolean>;
      };
      el.loading = false;
      el.agent = null;
      Object.defineProperty(el, 'session', {
        value: { sendData: sendDataSpy, setFrontmost: vi.fn(), state: { error: null } },
        configurable: true,
      });
      document.body.appendChild(el);
      await el.updateComplete;

      el.sendData('ls\n');
      expect(sendDataSpy).toHaveBeenCalledTimes(1);
      expect(sendDataSpy).toHaveBeenCalledWith('ls\n');
    });
  });
});
