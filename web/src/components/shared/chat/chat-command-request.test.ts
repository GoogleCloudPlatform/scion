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

import { createHash } from 'node:crypto';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import {
  COMMAND_REQUESTS_FLAG,
  resetServerFlagStateForTests,
  setFeatureFlag,
} from '../../../utils/feature-flags.js';
import { stateManager } from '../../../client/state.js';
import {
  formatCommandInlineForDisplay,
  formatCommandScriptForDisplay,
  newerCommandRequestView,
  type CommandRequestDetailResponse,
  type CommandRequestRunView,
  type CommandRequestView,
} from '../../../client/command-requests.js';
import { ScionChatCommandRequest } from './chat-command-request.js';

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

function canonicalHashSync(script: string, workdir: string, timeoutS: number): string {
  const payload =
    'scion.command_request.v1\n' +
    JSON.stringify({ interpreter: 'bash-login', script, timeout_s: timeoutS, workdir });
  return createHash('sha256').update(payload).digest('hex');
}

function makeRequest(overrides: Partial<CommandRequestView> = {}): CommandRequestView {
  const script = overrides.script ?? 'gcloud config list\n';
  const workdir = overrides.workdir ?? '';
  const timeoutS = overrides.timeoutS ?? 600;
  return {
    rowId: 'row-cmd-1',
    messageId: 'msg-cmd-1',
    projectId: 'proj-1',
    agentId: 'agent-1',
    agentName: 'deploy-bot',
    key: 'deploy-preview',
    version: 1,
    description: 'Deploy PR 42 preview environment',
    script,
    workdir,
    workdirKind: 'home',
    timeoutS,
    shareOutputRequested: false,
    commandSha256: overrides.commandSha256 ?? canonicalHashSync(script, workdir, timeoutS),
    approverUserId: 'user-1',
    status: 'pending',
    createdAt: '2026-11-26T10:00:00Z',
    expiresAt: '2026-11-27T10:00:00Z',
    ...overrides,
  };
}

function makeDetail(
  overrides: Partial<CommandRequestDetailResponse> = {}
): CommandRequestDetailResponse {
  return {
    request: makeRequest(),
    exec_container: {
      state: 'ready',
      agentId: 'exec-1',
      agentName: 'alice-shell',
      projectId: 'proj-1',
      projectName: 'alpha',
      homePersistent: true,
    },
    projectName: 'alpha',
    threadId: 'dm:agent:agent-1:user:user-1',
    approverName: 'Alice',
    canApprove: true,
    ...overrides,
  };
}

describe('formatCommandScriptForDisplay (audit M1.2)', () => {
  it('renders tabs, trailing spaces, and EVERY non-ASCII code point as visible escapes', () => {
    const script =
      '\techo hi  \n' +
      'echo "café"\n' +
      'echo "vs:\uFE00\uFE0F\u{E0100} fvs:\u180B braille:\u2800"\n';
    const formatted = formatCommandScriptForDisplay(script);

    expect(formatted.lineCount).toBe(3);
    expect(formatted.nonAsciiCount).toBe(6); // é, U+FE00, U+FE0F, U+E0100, U+180B, U+2800

    // Line 1: tab + ascii + two trailing spaces
    expect(formatted.lines[0].tokens).toEqual([
      { kind: 'tab', text: '\\t', codePoint: 0x09 },
      { kind: 'ascii', text: 'echo hi' },
      { kind: 'trailing-space', text: '\\u{0020}', codePoint: 0x20 },
      { kind: 'trailing-space', text: '\\u{0020}', codePoint: 0x20 },
    ]);

    // Line 2: non-ASCII é escaped as \u{00E9}
    const line2Escapes = formatted.lines[1].tokens.filter((t) => t.kind === 'escape');
    expect(line2Escapes.map((t) => t.text)).toEqual(['\\u{00E9}']);

    // Line 3: variation selectors, astral U+E0100, Mongolian FVS, Braille blank all visible escapes
    const line3Escapes = formatted.lines[2].tokens.filter((t) => t.kind === 'escape');
    expect(line3Escapes.map((t) => t.text)).toEqual([
      '\\u{FE00}',
      '\\u{FE0F}',
      '\\u{E0100}',
      '\\u{180B}',
      '\\u{2800}',
    ]);
  });

  it('detects agent-writable path mentions in script text', () => {
    expect(formatCommandScriptForDisplay('cd /workspace && make').mentionsAgentWritablePaths).toBe(
      true
    );
    expect(
      formatCommandScriptForDisplay('cat /scion-volumes/shared/run.sh').mentionsAgentWritablePaths
    ).toBe(true);
    expect(formatCommandScriptForDisplay('gcloud config list').mentionsAgentWritablePaths).toBe(
      false
    );
  });
});

describe('newerCommandRequestView', () => {
  it('never regresses terminal request or run status and preserves exec IDs', () => {
    const running = makeRequest({
      status: 'approved',
      run: {
        runId: 'run-1',
        requestId: 'row-cmd-1',
        status: 'running',
        execAgentId: 'exec-1',
        execProjectId: 'proj-1',
        shareOutput: false,
        timeoutS: 600,
        createdAt: '2026-11-26T10:01:00Z',
      },
    });
    const staleQueued = makeRequest({
      status: 'approved',
      run: {
        runId: 'run-1',
        requestId: 'row-cmd-1',
        status: 'queued',
        shareOutput: false,
        timeoutS: 600,
        createdAt: '2026-11-26T10:01:00Z',
      },
    });
    expect(newerCommandRequestView(running, staleQueued).run?.status).toBe('running');

    const sseFinished = makeRequest({
      status: 'approved',
      run: {
        runId: 'run-1',
        requestId: 'row-cmd-1',
        status: 'succeeded',
        exitCode: 0,
        durationMs: 1200,
        shareOutput: false,
        timeoutS: 600,
        createdAt: '2026-11-26T10:01:00Z',
      },
    });
    const merged = newerCommandRequestView(running, sseFinished);
    expect(merged.run?.status).toBe('succeeded');
    expect(merged.run?.execAgentId).toBe('exec-1');
  });
});

describe('<scion-chat-command-request>', () => {
  beforeEach(() => {
    resetServerFlagStateForTests();
    delete window.__SCION_FEATURES__;
    localStorage.clear();
    setFeatureFlag(COMMAND_REQUESTS_FLAG, true);
  });

  afterEach(() => {
    document.body.innerHTML = '';
    vi.restoreAllMocks();
    resetServerFlagStateForTests();
    delete window.__SCION_FEATURES__;
  });

  it('renders nothing when hub.command_requests is off', async () => {
    setFeatureFlag(COMMAND_REQUESTS_FLAG, false);
    const el = document.createElement('scion-chat-command-request') as ScionChatCommandRequest;
    el.detail = makeDetail();
    document.body.appendChild(el);
    await el.updateComplete;
    expect(el.shadowRoot?.querySelector('[data-testid="command-request-card"]')).toBeNull();
  });

  it('renders description as plain text, M1.2 script escapes, workdir warning, and exec container', async () => {
    const el = document.createElement('scion-chat-command-request') as ScionChatCommandRequest;
    el.currentUserId = 'user-1';
    el.detail = makeDetail({
      request: makeRequest({
        description: '**not bold** <script>alert(1)</script>',
        script: 'echo "héllo"\n',
        workdir: '/workspace/app',
        workdirKind: 'workspace',
      }),
      exec_container: {
        state: 'suspended',
        agentId: 'exec-1',
        agentName: 'alice-shell',
        projectName: 'alpha',
        homePersistent: false,
        canWake: true,
      },
    });
    document.body.appendChild(el);
    await el.updateComplete;

    const desc = el.shadowRoot?.querySelector('[data-testid="command-description"]');
    expect(desc?.textContent).toBe('**not bold** <script>alert(1)</script>');
    expect(desc?.querySelector('strong')).toBeNull();
    expect(desc?.querySelector('script')).toBeNull();

    // M1.2 non-ASCII escape badge & warning
    expect(
      el.shadowRoot?.querySelector('[data-testid="non-ascii-warning"]')?.textContent
    ).toContain('1 non-ASCII');
    const escapeTokens = el.shadowRoot?.querySelectorAll('[data-testid="escape-token"]');
    expect(escapeTokens?.[0]?.textContent).toBe('\\u{00E9}');

    // Workdir warning for workspace
    expect(el.shadowRoot?.querySelector('[data-testid="workdir-warning"]')?.textContent).toContain(
      '/workspace'
    );

    // Suspended container with canWake=true shows wake note + non-persistent home note + Approve enabled
    const ecLine = el.shadowRoot?.querySelector('[data-testid="exec-container-line"]');
    expect(ecLine?.textContent).toContain('Execution Container:');
    expect(ecLine?.textContent).toContain('alpha');
    expect(ecLine?.textContent).toContain('alice-shell');
    expect(ecLine?.textContent).toContain('Will be resumed when you approve.');
    expect(el.shadowRoot?.querySelector('[data-testid="exec-home-nonpersistent"]')).not.toBeNull();
    const approveBtn = el.shadowRoot?.querySelector(
      '[data-testid="approve-btn"]'
    ) as HTMLElement & {
      disabled?: boolean;
    };
    expect(Boolean(approveBtn.hasAttribute('disabled') || approveBtn.disabled)).toBe(false);

    // Dismiss hidden until P5 sets canDismiss: true
    expect(el.shadowRoot?.querySelector('[data-testid="dismiss-btn"]')).toBeNull();
  });

  it('organizes card into What Runs and Execution Environment sections with container breadcrumb above metadata', async () => {
    const el = document.createElement('scion-chat-command-request') as ScionChatCommandRequest;
    el.currentUserId = 'user-1';
    el.detail = makeDetail({
      exec_container: {
        state: 'ready',
        agentId: 'exec-1',
        agentName: 'ptone-exec',
        projectId: 'proj-1',
        projectName: 'scion-fork',
        homePersistent: true,
      },
    });
    document.body.appendChild(el);
    await el.updateComplete;

    const whatRuns = el.shadowRoot?.querySelector('[data-testid="section-what-runs"]');
    const execEnv = el.shadowRoot?.querySelector('[data-testid="section-exec-env"]');
    expect(whatRuns).not.toBeNull();
    expect(execEnv).not.toBeNull();

    // What Runs contains heading, description, and script block
    expect(whatRuns?.querySelector('.command-section-label')?.textContent?.trim()).toBe(
      'What Runs'
    );
    expect(whatRuns?.querySelector('[data-testid="command-description"]')).not.toBeNull();
    expect(whatRuns?.querySelector('[data-testid="command-script-block"]')).not.toBeNull();

    // Execution Environment contains heading, Line 1 breadcrumb, and Line 2 metadata grid in order
    expect(execEnv?.querySelector('.command-section-label')?.textContent?.trim()).toBe(
      'Execution Environment'
    );
    const ecLine = execEnv?.querySelector('[data-testid="exec-container-line"]') as HTMLElement;
    const metaGrid = execEnv?.querySelector('.meta-grid') as HTMLElement;
    expect(ecLine).not.toBeNull();
    expect(metaGrid).not.toBeNull();
    expect(
      Boolean(ecLine.compareDocumentPosition(metaGrid) & Node.DOCUMENT_POSITION_FOLLOWING)
    ).toBe(true);

    // Line 1: Execution Container: <folder>scion-fork / <terminal>ptone-exec ready
    const crumb = ecLine.querySelector('[data-testid="exec-container-breadcrumb"]');
    expect(crumb).not.toBeNull();
    const icons = [...(crumb?.querySelectorAll('sl-icon') ?? [])].map((i) =>
      i.getAttribute('name')
    );
    expect(icons).toEqual(['folder', 'terminal']);
    expect(crumb?.querySelector('.exec-container-project')?.textContent).toBe('scion-fork');
    expect(crumb?.querySelector('.exec-crumb-sep')?.textContent).toBe('/');
    expect(crumb?.querySelector('.exec-container-name')?.textContent).toBe('ptone-exec');
    expect(ecLine.querySelector('[data-testid="exec-container-state"]')?.textContent).toBe('ready');

    // Line 2: Workdir, Timeout, Approver
    expect(metaGrid.querySelector('[data-testid="command-workdir"]')?.textContent).toBe('$HOME');
    expect(metaGrid.querySelector('[data-testid="command-timeout"]')?.textContent).toContain('10m');
    expect(metaGrid.querySelector('[data-testid="command-approver"]')?.textContent).toContain(
      'you'
    );

    // Approve button has no ellipsis and no second confirm dialog is rendered
    const approveBtn = el.shadowRoot?.querySelector('[data-testid="approve-btn"]');
    expect(approveBtn?.textContent?.trim()).toBe('Approve & run');
    expect(el.shadowRoot?.querySelector('sl-dialog.confirm-dialog')).toBeNull();

    // Execution container name is not bold
    expect(ScionChatCommandRequest.styles.cssText).not.toMatch(
      /\.exec-container-name\s*\{[^}]*font-weight:\s*(?:600|700|bold)/
    );
  });

  it('collapses scripts over 10 lines by default and toggles expand/collapse', async () => {
    const tenLineScript = Array.from({ length: 10 }, (_, i) => `echo line-${i + 1}`).join('\n');
    const el = document.createElement('scion-chat-command-request') as ScionChatCommandRequest;
    el.detail = makeDetail({
      request: makeRequest({ rowId: 'row-10', script: `${tenLineScript}\n` }),
    });
    document.body.appendChild(el);
    await el.updateComplete;

    // Exactly 10 lines -> rendered in full, no toggle bar
    expect(el.shadowRoot?.querySelectorAll('.script-line')).toHaveLength(10);
    expect(el.shadowRoot?.querySelector('[data-testid="script-expand-toggle"]')).toBeNull();

    // 14 lines -> collapsed by default to 10 lines with expand toggle
    const fourteenLineScript = Array.from({ length: 14 }, (_, i) => `echo line-${i + 1}`).join(
      '\n'
    );
    el.detail = makeDetail({
      request: makeRequest({ rowId: 'row-14', script: `${fourteenLineScript}\n` }),
    });
    await el.updateComplete;

    expect(el.shadowRoot?.querySelectorAll('.script-line')).toHaveLength(10);
    expect(el.shadowRoot?.querySelector('.script-scroll.collapsed')).not.toBeNull();
    const toggle = el.shadowRoot?.querySelector(
      '[data-testid="script-expand-toggle"]'
    ) as HTMLButtonElement;
    expect(toggle).not.toBeNull();
    expect(toggle.getAttribute('aria-expanded')).toBe('false');
    expect(toggle.textContent?.trim()).toBe('Show all 14 lines (4 more)');

    // Expand -> all 14 lines visible
    toggle.click();
    await el.updateComplete;
    expect(el.shadowRoot?.querySelectorAll('.script-line')).toHaveLength(14);
    expect(el.shadowRoot?.querySelector('.script-scroll.collapsed')).toBeNull();
    expect(toggle.getAttribute('aria-expanded')).toBe('true');
    expect(toggle.textContent?.trim()).toBe('Show fewer lines');

    // Collapse again -> back to 10 lines
    toggle.click();
    await el.updateComplete;
    expect(el.shadowRoot?.querySelectorAll('.script-line')).toHaveLength(10);
    expect(toggle.getAttribute('aria-expanded')).toBe('false');
  });

  it('disables Approve for suspended container when canWake is false (R2)', async () => {
    const el = document.createElement('scion-chat-command-request') as ScionChatCommandRequest;
    el.detail = makeDetail({
      exec_container: {
        state: 'suspended',
        agentId: 'exec-1',
        agentName: 'alice-shell',
        projectName: 'alpha',
        canWake: false,
      },
    });
    document.body.appendChild(el);
    await el.updateComplete;

    const approveBtn = el.shadowRoot?.querySelector(
      '[data-testid="approve-btn"]'
    ) as HTMLElement & {
      disabled?: boolean;
    };
    expect(approveBtn.hasAttribute('disabled') || approveBtn.disabled).toBe(true);
    expect(
      el.shadowRoot?.querySelector('[data-testid="exec-suspended-no-wake-note"]')?.textContent
    ).toContain('Resume your execution container to approve.');
    expect(el.shadowRoot?.querySelector('[data-testid="exec-suspended-wake-note"]')).toBeNull();
    expect(el.shadowRoot?.textContent).not.toContain('Will be resumed when you approve.');
  });

  it('wraps long script lines with internal spaces so payload is never hidden behind horizontal scroll (R1)', async () => {
    const stylesText = ScionChatCommandRequest.styles.cssText;
    expect(stylesText).toMatch(/\.script-code\s*\{[^}]*white-space:\s*pre-wrap/);
    expect(stylesText).toMatch(/\.script-code\s*\{[^}]*overflow-wrap:\s*anywhere/);
    expect(stylesText).not.toMatch(/\.script-line\s*\{[^}]*min-width:\s*max-content/);

    const paddedScript = `gcloud config list${' '.repeat(150)}; echo PAYLOAD\n`;
    const el = document.createElement('scion-chat-command-request') as ScionChatCommandRequest;
    el.detail = makeDetail({
      request: makeRequest({ script: paddedScript }),
    });
    document.body.appendChild(el);
    await el.updateComplete;

    const cardCode = el.shadowRoot?.querySelector(
      '[data-testid="command-script-block"] .script-code'
    );
    expect(cardCode?.textContent).toContain('; echo PAYLOAD');
  });

  it('renders escaped script tokens, homoglyph workdir, and approves directly with verified SHA-256 (R3, O1)', async () => {
    const trickyScript = '\techo "é\u2800\u{E0100}"  \n';
    const homoglyphWorkdir = '/w\u043Erkspace/app';
    const expectedHash = canonicalHashSync(trickyScript, homoglyphWorkdir, 600);
    const queuedRun: CommandRequestRunView = {
      runId: 'run-r3',
      requestId: 'row-cmd-1',
      status: 'queued',
      shareOutput: false,
      timeoutS: 600,
      createdAt: '2026-11-26T10:01:00Z',
    };
    const fetchMock = vi.fn().mockResolvedValue(
      jsonResponse(
        {
          request: makeRequest({
            script: trickyScript,
            workdir: homoglyphWorkdir,
            workdirKind: 'explicit',
            commandSha256: expectedHash,
            status: 'approved',
            run: queuedRun,
          }),
          run: queuedRun,
        },
        202
      )
    );
    vi.stubGlobal('fetch', fetchMock);

    // Also unit-check formatCommandInlineForDisplay directly (O1).
    const inline = formatCommandInlineForDisplay(homoglyphWorkdir);
    expect(inline.nonAsciiCount).toBe(1);
    expect(inline.tokens.filter((t) => t.kind === 'escape').map((t) => t.text)).toEqual([
      '\\u{043E}',
    ]);

    const el = document.createElement('scion-chat-command-request') as ScionChatCommandRequest;
    el.currentUserId = 'user-1';
    el.detail = makeDetail({
      request: makeRequest({
        script: trickyScript,
        workdir: homoglyphWorkdir,
        workdirKind: 'explicit',
        commandSha256: expectedHash,
      }),
    });
    document.body.appendChild(el);
    await el.updateComplete;

    // M1.2/R3/O1 invariants directly on the card
    const cardScriptBlock = el.shadowRoot?.querySelector('[data-testid="command-script-block"]');
    const cardEscapes = [
      ...(cardScriptBlock?.querySelectorAll('[data-testid="escape-token"]') ?? []),
    ].map((n) => n.textContent);
    expect(cardEscapes).toEqual(['\\u{00E9}', '\\u{2800}', '\\u{E0100}']);
    expect(cardScriptBlock?.querySelector('.ws-token.tab')?.textContent).toBe('\\t');
    expect(cardScriptBlock?.querySelectorAll('.ws-token.trailing-space')).toHaveLength(2);
    expect(cardScriptBlock?.textContent).not.toContain('é');
    expect(cardScriptBlock?.textContent).not.toContain('\u2800');
    expect(cardScriptBlock?.textContent).not.toContain('\u{E0100}');

    // O1 on card: homoglyph workdir escaped, raw Cyrillic 'о' absent
    const cardWorkdir = el.shadowRoot?.querySelector('[data-testid="command-workdir"]');
    expect(cardWorkdir?.textContent).toContain('\\u{043E}');
    expect(cardWorkdir?.textContent).not.toContain('\u043E');

    // Card displays timeout and container breadcrumb
    expect(el.shadowRoot?.textContent).toContain('Timeout: 10m');
    expect(el.shadowRoot?.textContent).toContain('alice-shell');

    // Clicking Approve & run submits directly without raising a second confirm dialog
    (el.shadowRoot?.querySelector('[data-testid="approve-btn"]') as HTMLElement).click();
    await el.updateComplete;
    expect(el.shadowRoot?.querySelector('[data-testid="confirm-dialog-body"]')).toBeNull();

    await vi.waitFor(() => {
      expect(fetchMock).toHaveBeenCalledTimes(1);
    });
    const [, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    const body = JSON.parse(init.body as string) as { command_sha256: string };
    expect(body.command_sha256).toBe(expectedHash);
  });

  it('shows shared_dir warning when workdirKind is shared_dir', async () => {
    const el = document.createElement('scion-chat-command-request') as ScionChatCommandRequest;
    el.detail = makeDetail({
      request: makeRequest({
        workdir: '/scion-volumes/data',
        workdirKind: 'shared_dir',
      }),
    });
    document.body.appendChild(el);
    await el.updateComplete;
    expect(el.shadowRoot?.querySelector('[data-testid="workdir-warning"]')?.textContent).toContain(
      'shared directory'
    );
  });

  it('disables Approve for stopped container and shows setup link for unconfigured', async () => {
    const el = document.createElement('scion-chat-command-request') as ScionChatCommandRequest;
    el.detail = makeDetail({
      exec_container: {
        state: 'stopped',
        agentId: 'exec-1',
        agentName: 'alice-shell',
      },
    });
    document.body.appendChild(el);
    await el.updateComplete;

    const approveBtn = el.shadowRoot?.querySelector(
      '[data-testid="approve-btn"]'
    ) as HTMLElement & {
      disabled?: boolean;
    };
    expect(approveBtn.hasAttribute('disabled') || approveBtn.disabled).toBe(true);
    expect(
      el.shadowRoot?.querySelector('[data-testid="exec-stopped-note"]')?.textContent
    ).toContain('Start your execution container to approve.');

    // Switch to unconfigured
    el.detail = makeDetail({
      request: makeRequest({ rowId: 'row-cmd-2' }),
      exec_container: { state: 'unconfigured' },
    });
    await el.updateComplete;
    const setupLink = el.shadowRoot?.querySelector(
      '[data-testid="exec-setup-link"]'
    ) as HTMLAnchorElement;
    expect(setupLink?.getAttribute('href')).toBe('/profile/settings');
  });

  it('approves directly without a second confirm dialog and updates run status live via SSE', async () => {
    const queuedRun: CommandRequestRunView = {
      runId: 'run-1',
      requestId: 'row-cmd-1',
      status: 'queued',
      shareOutput: false,
      timeoutS: 600,
      createdAt: '2026-11-26T10:01:00Z',
    };
    const fetchMock = vi.fn().mockResolvedValue(
      jsonResponse(
        {
          request: makeRequest({
            status: 'approved',
            approved: {
              at: '2026-11-26T10:01:00Z',
              byUserId: 'user-1',
              byName: 'Alice',
              surface: 'native',
            },
            run: queuedRun,
          }),
          run: queuedRun,
        },
        202
      )
    );
    vi.stubGlobal('fetch', fetchMock);

    const el = document.createElement('scion-chat-command-request') as ScionChatCommandRequest;
    el.currentUserId = 'user-1';
    el.detail = makeDetail();
    document.body.appendChild(el);
    await el.updateComplete;

    // Click Approve & run directly (no second confirm dialog)
    (el.shadowRoot?.querySelector('[data-testid="approve-btn"]') as HTMLElement).click();
    await el.updateComplete;
    expect(el.shadowRoot?.querySelector('[data-testid="confirm-dialog-body"]')).toBeNull();

    await vi.waitFor(() => {
      expect(
        el.shadowRoot?.querySelector('[data-testid="run-status-chip"]')?.textContent
      ).toContain('Queued');
    });

    expect(fetchMock).toHaveBeenCalledTimes(1);
    const [url, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(url).toBe('/api/v1/command-requests/row-cmd-1/approve');
    const body = JSON.parse(init.body as string) as {
      row_id: string;
      command_sha256: string;
      surface: string;
    };
    expect(body.row_id).toBe('row-cmd-1');
    expect(body.command_sha256).toBe(canonicalHashSync('gcloud config list\n', '', 600));
    expect(body.surface).toBe('native');

    // SSE update: running -> succeeded
    stateManager.dispatchEvent(
      new CustomEvent('message-action-updated', {
        detail: {
          data: {
            kind: 'command_request',
            messageId: 'msg-cmd-1',
            command_request: makeRequest({
              status: 'approved',
              run: { ...queuedRun, status: 'running' },
            }),
          },
        },
      })
    );
    await el.updateComplete;
    expect(el.shadowRoot?.querySelector('[data-testid="run-status-chip"]')?.textContent).toContain(
      'Running'
    );

    stateManager.dispatchEvent(
      new CustomEvent('message-action-updated', {
        detail: {
          data: {
            kind: 'command_request',
            messageId: 'msg-cmd-1',
            command_request: makeRequest({
              status: 'approved',
              run: { ...queuedRun, status: 'succeeded', exitCode: 0, durationMs: 2400 },
            }),
          },
        },
      })
    );
    await el.updateComplete;
    expect(el.shadowRoot?.querySelector('[data-testid="run-status-chip"]')?.textContent).toContain(
      'Succeeded'
    );
    expect(el.shadowRoot?.querySelector('[data-testid="run-exit-code"]')?.textContent).toContain(
      'exit 0'
    );
    expect(el.shadowRoot?.querySelector('[data-testid="run-duration"]')?.textContent).toContain(
      '2.4s'
    );
  });

  it('renders D7 banner when previousVersionUnknown is present', async () => {
    const el = document.createElement('scion-chat-command-request') as ScionChatCommandRequest;
    el.detail = makeDetail({
      request: makeRequest({ version: 2 }),
      previousVersionUnknown: { version: 1, rowId: 'row-v1', runId: 'run-v1' },
    });
    document.body.appendChild(el);
    await el.updateComplete;

    const banner = el.shadowRoot?.querySelector('[data-testid="d7-previous-unknown-banner"]');
    expect(banner?.textContent?.replace(/\s+/g, ' ').trim()).toContain(
      'Previous version v1 result unknown: it may have run.'
    );
  });

  it('feature-detects View output (P4) and Dismiss (P5)', async () => {
    const fetchMock = vi
      .fn()
      .mockResolvedValue(jsonResponse({ output: 'hello from container\n' }, 200));
    vi.stubGlobal('fetch', fetchMock);

    const el = document.createElement('scion-chat-command-request') as ScionChatCommandRequest;
    el.currentUserId = 'user-1';
    el.detail = makeDetail({
      outputAvailable: true,
      request: makeRequest({
        status: 'approved',
        run: {
          runId: 'run-1',
          requestId: 'row-cmd-1',
          status: 'succeeded',
          exitCode: 0,
          durationMs: 300,
          shareOutput: false,
          timeoutS: 600,
          createdAt: '2026-11-26T10:01:00Z',
        },
      }),
    });
    document.body.appendChild(el);
    await el.updateComplete;

    const viewBtn = el.shadowRoot?.querySelector('[data-testid="view-output-btn"]') as HTMLElement;
    expect(viewBtn).not.toBeNull();
    viewBtn.click();

    await vi.waitFor(() => {
      expect(
        el.shadowRoot?.querySelector('[data-testid="command-output-panel"]')?.textContent
      ).toContain('hello from container');
    });
    expect(fetchMock).toHaveBeenCalledWith(
      '/api/v1/command-requests/row-cmd-1/output',
      expect.objectContaining({ method: 'GET' })
    );
  });

  it('carry-forward (a): refuses to POST approve when recomputed WebCrypto hash disagrees with commandSha256', async () => {
    const fetchMock = vi.fn();
    vi.stubGlobal('fetch', fetchMock);

    const el = document.createElement('scion-chat-command-request') as ScionChatCommandRequest;
    el.currentUserId = 'user-1';
    el.detail = makeDetail({
      request: makeRequest({
        script: 'echo real\n',
        commandSha256: 'f'.repeat(64),
      }),
    });
    document.body.appendChild(el);
    await el.updateComplete;

    (el.shadowRoot?.querySelector('[data-testid="approve-btn"]') as HTMLElement).click();
    await vi.waitFor(() => {
      const err = el.shadowRoot?.querySelector('[data-testid="command-error-banner"]');
      expect(err?.textContent).toContain('Command hash verification failed');
    });
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it('carry-forward (b): approver card re-fetches detail via GET on SSE update so reason/wake show live', async () => {
    const fetchMock = vi.fn().mockResolvedValue(
      jsonResponse(
        makeDetail({
          request: makeRequest({
            status: 'approved',
            run: {
              runId: 'run-live-1',
              requestId: 'row-cmd-1',
              status: 'not_started',
              reason: 'exec_unsupported',
              wake: 'resume',
              shareOutput: false,
              timeoutS: 600,
              createdAt: '2026-11-26T10:01:00Z',
            },
          }),
        }),
        200
      )
    );
    vi.stubGlobal('fetch', fetchMock);

    const el = document.createElement('scion-chat-command-request') as ScionChatCommandRequest;
    el.currentUserId = 'user-1';
    el.detail = makeDetail();
    document.body.appendChild(el);
    await el.updateComplete;

    // Emit SSE update stripped of reason/wake (as the Hub broadcasts).
    const { stateManager } = await import('../../../client/state.js');
    stateManager.dispatchEvent(
      new CustomEvent('message-action-updated', {
        detail: {
          kind: 'command_request',
          messageId: 'msg-cmd-1',
          threadId: 'dm:agent:agent-1:user:user-1',
          command_request: makeRequest({
            status: 'approved',
            run: {
              runId: 'run-live-1',
              requestId: 'row-cmd-1',
              status: 'not_started',
              shareOutput: false,
              timeoutS: 600,
              createdAt: '2026-11-26T10:01:00Z',
            },
          }),
        },
      })
    );

    await vi.waitFor(() => {
      expect(fetchMock).toHaveBeenCalledWith(
        '/api/v1/command-requests/row-cmd-1',
        expect.objectContaining({ method: 'GET' })
      );
      expect(el.shadowRoot?.textContent).toContain(
        'Restart your execution container to enable approved commands'
      );
    });
  });
});
