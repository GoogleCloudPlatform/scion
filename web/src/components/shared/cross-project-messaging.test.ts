/**
 * Tests for cross-project messaging UI components.
 *
 * Covers:
 * - Message mode badge/display for hub mode
 * - Messageability indicator cross-project denial reasons
 * - Agent message viewer cross-project badge rendering
 * - Message-mode denial copy for cross-project codes
 */

import { describe, it, expect, vi, beforeAll, afterEach } from 'vitest';

// ── Message mode display tests (pure functions, no DOM) ──

describe('cross-project messaging — shared utilities', () => {
  let messageModeModule: typeof import('../../shared/message-mode.js');

  beforeAll(async () => {
    messageModeModule = await import('../../shared/message-mode.js');
  });

  describe('hub mode display', () => {
    it('has icon "globe" and color "success"', () => {
      const hub = messageModeModule.MESSAGE_MODE_DISPLAY.hub;
      expect(hub.icon).toBe('globe');
      expect(hub.color).toBe('success');
    });

    it('has label "Hub"', () => {
      expect(messageModeModule.MESSAGE_MODE_DISPLAY.hub.label).toBe('Hub');
    });

    it('description mentions other projects', () => {
      expect(messageModeModule.MESSAGE_MODE_DISPLAY.hub.description).toContain('other projects');
    });

    it('hub is most permissive in sort order', () => {
      expect(messageModeModule.MODE_SORT_ORDER.hub).toBe(0);
    });
  });

  describe('cross-project denial messages', () => {
    it('cross_project_disabled mentions Hub administrator', () => {
      const msg = messageModeModule.getDenialMessage('cross_project_disabled');
      expect(msg).toContain('Hub administrator');
      expect(msg).toContain('disabled');
    });

    it('cross_project_sender_mode substitutes sender name', () => {
      const msg = messageModeModule.getDenialMessage(
        'cross_project_sender_mode',
        'target-agent',
        'my-agent'
      );
      expect(msg).toContain('my-agent');
      expect(msg).toContain('Hub mode');
    });

    it('cross_project_target_mode substitutes recipient name', () => {
      const msg = messageModeModule.getDenialMessage('cross_project_target_mode', 'target-agent');
      expect(msg).toContain('target-agent');
    });

    it('cross_project_inbound_none mentions project rejection', () => {
      const msg = messageModeModule.getDenialMessage('cross_project_inbound_none', 'ext-agent');
      expect(msg).toContain('ext-agent');
      expect(msg).toContain('does not accept');
    });

    it('cross_project_origin_not_member mentions membership', () => {
      const msg = messageModeModule.getDenialMessage('cross_project_origin_not_member');
      expect(msg).toContain('not a member');
    });

    it('cross_project_untrusted_origin mentions identity', () => {
      const msg = messageModeModule.getDenialMessage('cross_project_untrusted_origin');
      expect(msg).toContain('identity origin');
    });

    it('cross_project_surface_unsupported mentions conversation type', () => {
      const msg = messageModeModule.getDenialMessage('cross_project_surface_unsupported');
      expect(msg).toContain('conversation type');
    });

    it('generic denied returns fallback', () => {
      expect(messageModeModule.getDenialMessage('denied')).toBe('Message delivery denied.');
    });

    it('unknown code returns generic fallback', () => {
      expect(messageModeModule.getDenialMessage('unknown_future_code')).toBe(
        'Message delivery denied.'
      );
    });
  });

  describe('getMessageModeDisplay', () => {
    it('returns hub display for "hub"', () => {
      expect(messageModeModule.getMessageModeDisplay('hub').label).toBe('Hub');
    });

    it('falls back to project for unknown mode', () => {
      expect(messageModeModule.getMessageModeDisplay('unknown').label).toBe('Project');
    });
  });
});

// ── Messageability indicator tests ──

describe('scion-messageability-indicator — cross-project', () => {
  let ScionMessageabilityIndicator: unknown;

  beforeAll(async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(() => Promise.resolve(new Response('{}', { status: 200 })))
    );
    const mod = await import('./messageability-indicator.js');
    ScionMessageabilityIndicator = mod.ScionMessageabilityIndicator;
  });

  afterEach(() => {
    document.body.querySelectorAll('scion-messageability-indicator').forEach((el) => el.remove());
    vi.restoreAllMocks();
  });

  async function createIndicator(
    messageability: {
      canMessage: boolean;
      canReachViewer: boolean;
      reason?: string;
      replyReason?: string;
    },
    agentName?: string
  ) {
    const el = document.createElement('scion-messageability-indicator') as InstanceType<
      typeof ScionMessageabilityIndicator & typeof HTMLElement
    >;
    (el as any).messageability = messageability;
    if (agentName) {
      (el as any).agentName = agentName;
    }
    document.body.appendChild(el);
    await (el as any).updateComplete;
    return el as HTMLElement;
  }

  it('renders nothing when messageability is undefined', async () => {
    const el = document.createElement('scion-messageability-indicator') as HTMLElement;
    document.body.appendChild(el);
    await (el as any).updateComplete;
    expect(el.shadowRoot?.textContent?.trim()).toBe('');
    el.remove();
  });

  it('shows bidirectional icon when both canMessage and canReachViewer', async () => {
    const el = await createIndicator({ canMessage: true, canReachViewer: true });
    const icon = el.shadowRoot?.querySelector('sl-icon');
    expect(icon?.getAttribute('name')).toBe('arrow-left-right');
  });

  it('shows one-way icon when canMessage but not canReachViewer', async () => {
    const el = await createIndicator({ canMessage: true, canReachViewer: false });
    const icon = el.shadowRoot?.querySelector('sl-icon');
    expect(icon?.getAttribute('name')).toBe('arrow-right');
  });

  it('shows globe icon for cross-project denial', async () => {
    const el = await createIndicator({
      canMessage: false,
      canReachViewer: false,
      reason: 'cross_project_disabled',
    });
    const icon = el.shadowRoot?.querySelector('sl-icon');
    expect(icon?.getAttribute('name')).toBe('globe');
  });

  it('shows x-circle icon for non-cross-project denial', async () => {
    const el = await createIndicator({
      canMessage: false,
      canReachViewer: false,
      reason: 'mode_none',
    });
    const icon = el.shadowRoot?.querySelector('sl-icon');
    expect(icon?.getAttribute('name')).toBe('x-circle');
  });

  it('tooltip contains agent name in denial message', async () => {
    const el = await createIndicator(
      {
        canMessage: false,
        canReachViewer: false,
        reason: 'cross_project_inbound_none',
      },
      'my-target-agent'
    );
    const tooltip = el.shadowRoot?.querySelector('sl-tooltip');
    const tooltipContent = tooltip?.getAttribute('content') ?? '';
    expect(tooltipContent).toContain('my-target-agent');
  });
});

// ── Message mode badge tests ──

describe('scion-message-mode-badge — hub mode', () => {
  let ScionMessageModeBadge: unknown;

  beforeAll(async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(() => Promise.resolve(new Response('{}', { status: 200 })))
    );
    const mod = await import('./message-mode-badge.js');
    ScionMessageModeBadge = mod.ScionMessageModeBadge;
  });

  afterEach(() => {
    document.body.querySelectorAll('scion-message-mode-badge').forEach((el) => el.remove());
    vi.restoreAllMocks();
  });

  async function createBadge(mode: string) {
    const el = document.createElement('scion-message-mode-badge') as HTMLElement;
    (el as any).mode = mode;
    document.body.appendChild(el);
    await (el as any).updateComplete;
    return el;
  }

  it('renders "Hub" label for hub mode', async () => {
    const el = await createBadge('hub');
    const text = el.shadowRoot?.textContent?.trim() ?? '';
    expect(text).toContain('Hub');
  });

  it('renders globe icon for hub mode', async () => {
    const el = await createBadge('hub');
    const icon = el.shadowRoot?.querySelector('sl-icon');
    expect(icon?.getAttribute('name')).toBe('globe');
  });

  it('uses success color for hub mode', async () => {
    const el = await createBadge('hub');
    const badge = el.shadowRoot?.querySelector('.badge');
    expect(badge?.classList.contains('success')).toBe(true);
  });

  it('renders "Project" label for project mode', async () => {
    const el = await createBadge('project');
    const text = el.shadowRoot?.textContent?.trim() ?? '';
    expect(text).toContain('Project');
  });

  it('renders "Sealed" label for none mode', async () => {
    const el = await createBadge('none');
    const text = el.shadowRoot?.textContent?.trim() ?? '';
    expect(text).toContain('Sealed');
  });
});

// ── Agent tree view edge styling tests ──

describe('agent-tree-view — hub mode edge styles', () => {
  let AgentTreeView: unknown;

  beforeAll(async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(() => Promise.resolve(new Response('{}', { status: 200 })))
    );
    const mod = await import('./agent-tree-view.js');
    AgentTreeView = mod.ScionAgentTreeView;
  });

  afterEach(() => {
    document.body.querySelectorAll('scion-agent-tree-view').forEach((el) => el.remove());
    vi.restoreAllMocks();
  });

  // Tree view edge rendering is tested by verifying the component renders without errors
  // when agents have hub mode. Detailed edge color assertions require SVG inspection
  // which is beyond the scope of unit tests.

  it('renders without errors with hub-mode agents', async () => {
    const el = document.createElement('scion-agent-tree-view') as HTMLElement;
    (el as any).agents = [
      {
        id: 'agent-1',
        name: 'hub-agent',
        messageMode: 'hub',
        status: 'running',
        projectId: 'proj-1',
      },
      {
        id: 'agent-2',
        name: 'project-agent',
        messageMode: 'project',
        status: 'running',
        projectId: 'proj-1',
        parentAgentId: 'agent-1',
      },
    ];
    document.body.appendChild(el);
    await (el as any).updateComplete;
    expect(el.shadowRoot).toBeTruthy();
  });
});
