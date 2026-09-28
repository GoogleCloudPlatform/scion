/**
 * Tests for cross-project messaging denial reason codes and display.
 */

import { describe, it, expect } from 'vitest';
import {
  MESSAGE_MODE_DISPLAY,
  MODE_SORT_ORDER,
  getMessageModeDisplay,
  getDenialMessage,
  DENIAL_REASON_COPY,
} from './message-mode.js';
import type { MessageMode } from './types.js';

describe('MESSAGE_MODE_DISPLAY', () => {
  it('includes hub mode with correct attributes', () => {
    const hub = MESSAGE_MODE_DISPLAY.hub;
    expect(hub).toBeDefined();
    expect(hub.icon).toBe('globe');
    expect(hub.color).toBe('success');
    expect(hub.label).toBe('Hub');
    expect(hub.description).toContain('other projects');
  });

  it('includes all five modes', () => {
    const modes: MessageMode[] = ['none', 'lineage', 'branch', 'project', 'hub'];
    for (const mode of modes) {
      expect(MESSAGE_MODE_DISPLAY[mode]).toBeDefined();
      expect(MESSAGE_MODE_DISPLAY[mode].label).toBeTruthy();
    }
  });
});

describe('MODE_SORT_ORDER', () => {
  it('ranks hub as most permissive (index 0)', () => {
    expect(MODE_SORT_ORDER.hub).toBe(0);
  });

  it('ranks project below hub', () => {
    expect(MODE_SORT_ORDER.project).toBeGreaterThan(MODE_SORT_ORDER.hub);
  });

  it('ranks none as least permissive', () => {
    expect(MODE_SORT_ORDER.none).toBe(4);
  });
});

describe('getMessageModeDisplay', () => {
  it('returns hub display for hub mode', () => {
    expect(getMessageModeDisplay('hub').label).toBe('Hub');
  });

  it('falls back to project for unknown mode', () => {
    expect(getMessageModeDisplay('unknown_future_mode').label).toBe('Project');
  });

  it('falls back to project for undefined', () => {
    expect(getMessageModeDisplay(undefined).label).toBe('Project');
  });
});

describe('getDenialMessage — cross-project codes', () => {
  it('returns message for cross_project_disabled', () => {
    const msg = getDenialMessage('cross_project_disabled');
    expect(msg).toContain('disabled');
    expect(msg).toContain('Hub administrator');
  });

  it('returns message for cross_project_sender_mode', () => {
    const msg = getDenialMessage('cross_project_sender_mode', 'target-agent', 'my-agent');
    expect(msg).toContain('my-agent');
    expect(msg).toContain('Hub mode');
  });

  it('returns message for cross_project_target_mode', () => {
    const msg = getDenialMessage('cross_project_target_mode', 'target-agent');
    expect(msg).toContain('target-agent');
    expect(msg).toContain('Project or Hub mode');
  });

  it('returns message for cross_project_inbound_none', () => {
    const msg = getDenialMessage('cross_project_inbound_none', 'target-agent');
    expect(msg).toContain('target-agent');
    expect(msg).toContain('does not accept');
  });

  it('returns message for cross_project_origin_not_member', () => {
    const msg = getDenialMessage('cross_project_origin_not_member', 'target-agent');
    expect(msg).toContain('not a member');
  });

  it('returns message for cross_project_untrusted_origin', () => {
    const msg = getDenialMessage('cross_project_untrusted_origin');
    expect(msg).toContain('identity origin');
  });

  it('returns message for cross_project_surface_unsupported', () => {
    const msg = getDenialMessage('cross_project_surface_unsupported');
    expect(msg).toContain('not supported');
    expect(msg).toContain('conversation type');
  });

  it('returns generic message for denied', () => {
    const msg = getDenialMessage('denied');
    expect(msg).toBe('Message delivery denied.');
  });

  it('returns fallback for unknown reason code', () => {
    const msg = getDenialMessage('some_totally_unknown_code');
    expect(msg).toBe('Message delivery denied.');
  });

  it('all defined denial reasons have copy', () => {
    for (const key of Object.keys(DENIAL_REASON_COPY)) {
      const msg = getDenialMessage(key);
      expect(msg).toBeTruthy();
      expect(msg).not.toBe('');
    }
  });
});
