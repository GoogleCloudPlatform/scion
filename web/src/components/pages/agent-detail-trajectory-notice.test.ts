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

describe('Trajectory capture notice banners (P5.1c)', () => {
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
    resetServerFlagStateForTests();
    document.body.innerHTML = '';
  });

  describe('agent-detail page banner', () => {
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

    it('hides banner when experiment is off even if payload has trajectory_capture', async () => {
      setFeatureFlag(TOKENOMICS_TRAJECTORY_CAPTURE_FLAG, false);
      const el = await mountAgentDetail({ policy: 'full', enforced_by_hub: false });
      expect(el.shadowRoot!.querySelector('[data-testid="trajectory-capture-banner"]')).toBeNull();
    });

    it('hides banner when experiment is on but trajectory_capture is absent', async () => {
      setFeatureFlag(TOKENOMICS_TRAJECTORY_CAPTURE_FLAG, true);
      const el = await mountAgentDetail(undefined);
      expect(el.shadowRoot!.querySelector('[data-testid="trajectory-capture-banner"]')).toBeNull();
    });

    for (const status of [
      { policy: 'full', enforced_by_hub: false },
      { policy: 'full', enforced_by_hub: true },
      { policy: 'metadata_only', enforced_by_hub: false },
      { policy: 'off', enforced_by_hub: false },
    ] satisfies TrajectoryCapturePolicyStatus[]) {
      it(`renders canonical notice for policy=${status.policy} enforced=${status.enforced_by_hub}`, async () => {
        setFeatureFlag(TOKENOMICS_TRAJECTORY_CAPTURE_FLAG, true);
        const el = await mountAgentDetail(status);
        const banner = el.shadowRoot!.querySelector(
          '[data-testid="trajectory-capture-banner"]'
        ) as HTMLElement | null;
        expect(banner).not.toBeNull();
        expect(banner!.getAttribute('data-policy')).toBe(status.policy);
        expect(banner!.getAttribute('data-enforced')).toBe(String(status.enforced_by_hub));
        expect(banner!.textContent).toContain(formatTrajectoryCaptureNotice(status));
      });
    }
  });

  describe('terminal-pane banner', () => {
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

    it('hides terminal banner when experiment is off', async () => {
      setFeatureFlag(TOKENOMICS_TRAJECTORY_CAPTURE_FLAG, false);
      const el = await mountTerminalPane({ policy: 'full', enforced_by_hub: true });
      expect(
        el.shadowRoot!.querySelector('[data-testid="terminal-trajectory-capture-banner"]')
      ).toBeNull();
    });

    for (const status of [
      { policy: 'full', enforced_by_hub: false },
      { policy: 'full', enforced_by_hub: true },
      { policy: 'metadata_only', enforced_by_hub: false },
      { policy: 'off', enforced_by_hub: false },
    ] satisfies TrajectoryCapturePolicyStatus[]) {
      it(`renders terminal banner for policy=${status.policy} enforced=${status.enforced_by_hub}`, async () => {
        setFeatureFlag(TOKENOMICS_TRAJECTORY_CAPTURE_FLAG, true);
        const el = await mountTerminalPane(status);
        const banner = el.shadowRoot!.querySelector(
          '[data-testid="terminal-trajectory-capture-banner"]'
        ) as HTMLElement | null;
        expect(banner).not.toBeNull();
        expect(banner!.getAttribute('data-policy')).toBe(status.policy);
        expect(banner!.getAttribute('data-enforced')).toBe(String(status.enforced_by_hub));
        expect(banner!.textContent).toContain(formatTrajectoryCaptureNotice(status));
      });
    }

    it('gates terminal input until agent metadata and capture policy arrive', async () => {
      setFeatureFlag(TOKENOMICS_TRAJECTORY_CAPTURE_FLAG, true);
      const sendDataSpy = vi.fn();
      const el = document.createElement('scion-terminal-pane') as HTMLElement & {
        loading: boolean;
        agent: Record<string, unknown> | null;
        session: { sendData: (d: string) => void } | null;
        sendData: (d: string) => void;
        applyMetadata: (value: {
          agent: Record<string, unknown> | null;
          availability: 'loading' | 'ready' | 'deleted' | 'unavailable';
          error: string | null;
        }) => void;
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

      // Before metadata resolves, banner is absent and input is held back.
      expect(
        el.shadowRoot!.querySelector('[data-testid="terminal-trajectory-capture-banner"]')
      ).toBeNull();
      el.sendData('ls\n');
      expect(sendDataSpy).not.toHaveBeenCalled();

      // Once agent metadata arrives, banner renders and input is forwarded.
      const status: TrajectoryCapturePolicyStatus = { policy: 'full', enforced_by_hub: true };
      el.applyMetadata({
        agent: {
          id: 'agent-1',
          name: 'test-agent',
          projectId: 'proj-1',
          template: 'claude',
          phase: 'running',
          trajectory_capture: status,
        },
        availability: 'ready',
        error: null,
      });
      await el.updateComplete;

      const banner = el.shadowRoot!.querySelector(
        '[data-testid="terminal-trajectory-capture-banner"]'
      ) as HTMLElement | null;
      expect(banner).not.toBeNull();
      expect(banner!.textContent).toContain(formatTrajectoryCaptureNotice(status));

      el.sendData('ls\n');
      expect(sendDataSpy).toHaveBeenCalledTimes(1);
      expect(sendDataSpy).toHaveBeenCalledWith('ls\n');
    });

    it('does not gate terminal input when trajectory capture experiment is off', async () => {
      setFeatureFlag(TOKENOMICS_TRAJECTORY_CAPTURE_FLAG, false);
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
