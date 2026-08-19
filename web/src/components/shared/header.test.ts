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
 * Tests for the project-context helpers used by the dashboard ↔ chat mode
 * switch in scion-header. These are pure functions — no DOM needed.
 */

import { describe, it, expect } from 'vitest';

import {
  projectIdFromDashboardPath,
  projectIdFromChatSpacePath,
  slugFromChatPath,
} from './header.js';

describe('projectIdFromDashboardPath', () => {
  it('extracts the project ID from /projects/:id', () => {
    expect(projectIdFromDashboardPath('/projects/abc-123')).toBe('abc-123');
  });

  it('extracts the project ID from /projects/:id/settings', () => {
    expect(projectIdFromDashboardPath('/projects/abc-123/settings')).toBe('abc-123');
  });

  it('extracts the project ID from /projects/:id/schedules', () => {
    expect(projectIdFromDashboardPath('/projects/abc-123/schedules')).toBe('abc-123');
  });

  it('extracts the project ID from /projects/:id/metrics', () => {
    expect(projectIdFromDashboardPath('/projects/abc-123/metrics')).toBe('abc-123');
  });

  it('returns null for /projects/new (creation form)', () => {
    expect(projectIdFromDashboardPath('/projects/new')).toBeNull();
  });

  it('returns null for /projects (list page)', () => {
    expect(projectIdFromDashboardPath('/projects')).toBeNull();
  });

  it('returns null for the root path', () => {
    expect(projectIdFromDashboardPath('/')).toBeNull();
  });

  it('returns null for unrelated paths', () => {
    expect(projectIdFromDashboardPath('/agents/foo')).toBeNull();
    expect(projectIdFromDashboardPath('/chat')).toBeNull();
  });

  it('ignores query parameters and hash fragments', () => {
    expect(projectIdFromDashboardPath('/projects/abc-123?foo=bar')).toBe('abc-123');
    expect(projectIdFromDashboardPath('/projects/abc-123#hash')).toBe('abc-123');
  });
});

describe('projectIdFromChatSpacePath', () => {
  it('extracts project ID from /chat/space/:id', () => {
    expect(projectIdFromChatSpacePath('/chat/space/proj-42')).toBe('proj-42');
  });

  it('extracts project ID from /chat/space/:id/thread/:tid', () => {
    expect(projectIdFromChatSpacePath('/chat/space/proj-42/thread/topic-7')).toBe('proj-42');
  });

  it('returns null for /chat/:slug paths', () => {
    expect(projectIdFromChatSpacePath('/chat/my-project')).toBeNull();
  });

  it('returns null for /chat/dm paths', () => {
    expect(projectIdFromChatSpacePath('/chat/dm/abc')).toBeNull();
  });

  it('returns null for bare /chat', () => {
    expect(projectIdFromChatSpacePath('/chat')).toBeNull();
  });

  it('ignores query parameters and hash fragments', () => {
    expect(projectIdFromChatSpacePath('/chat/space/proj-42?thread=topic-7')).toBe('proj-42');
    expect(projectIdFromChatSpacePath('/chat/space/proj-42#hash')).toBe('proj-42');
  });
});

describe('slugFromChatPath', () => {
  it('extracts the slug from /chat/:slug', () => {
    expect(slugFromChatPath('/chat/my-project')).toBe('my-project');
  });

  it('extracts the slug from /chat/:slug/:threadId', () => {
    expect(slugFromChatPath('/chat/my-project/thread-1')).toBe('my-project');
  });

  it('returns null for /chat/space/:id (handled by projectIdFromChatSpacePath)', () => {
    expect(slugFromChatPath('/chat/space/proj-42')).toBeNull();
  });

  it('returns null for /chat/dm/:key (DMs have no project context)', () => {
    expect(slugFromChatPath('/chat/dm/abc')).toBeNull();
  });

  it('returns null for bare /chat', () => {
    expect(slugFromChatPath('/chat')).toBeNull();
  });

  it('returns null for non-chat paths', () => {
    expect(slugFromChatPath('/')).toBeNull();
    expect(slugFromChatPath('/projects/abc')).toBeNull();
  });

  it('ignores query parameters and hash fragments', () => {
    expect(slugFromChatPath('/chat/my-project?foo=bar')).toBe('my-project');
    expect(slugFromChatPath('/chat/my-project#hash')).toBe('my-project');
  });
});
