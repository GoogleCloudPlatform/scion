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
 * Pure summary-of-state prose formatter for Inbox cards (design §6.2, §6.3).
 * Mirrored in Go by `cmd/inbox_prose.go` and verified against the shared
 * fixture `testdata/inbox_prose.json`.
 */

export const INBOX_NOTHING_WAITING = 'Nothing Waiting';

function pluralize(n: number, singular: string, plural: string): string {
  return n === 1 ? `1 ${singular}` : `${n} ${plural}`;
}

function formatCountNoun(count: number, capped: boolean, singular: string, plural: string): string {
  if (capped) {
    return `${count}+ ${plural}`;
  }
  return pluralize(count, singular, plural);
}

function appendAgentsProjects(base: string, agents: number, projects: number): string {
  let out = base;
  if (agents > 0) {
    out += ` from ${pluralize(agents, 'agent', 'agents')}`;
  }
  if (projects > 0) {
    out += ` in ${pluralize(projects, 'project', 'projects')}`;
  }
  return out;
}

export function formatInboxProse(
  category: string,
  count: number,
  capped: boolean,
  breakdown?: Record<string, number>
): string {
  const agents = breakdown?.agents ?? 0;
  const projects = breakdown?.projects ?? 0;

  switch (category) {
    case 'decisions':
      if (count <= 0) return INBOX_NOTHING_WAITING;
      return appendAgentsProjects(
        formatCountNoun(count, capped, 'decision', 'decisions'),
        agents,
        projects
      );

    case 'agent_secrets':
      if (count <= 0) return INBOX_NOTHING_WAITING;
      return appendAgentsProjects(
        formatCountNoun(count, capped, 'secret request', 'secret requests'),
        agents,
        projects
      );

    case 'waiting': {
      if (count <= 0) return INBOX_NOTHING_WAITING;
      const base = formatCountNoun(count, capped, 'agent waiting', 'agents waiting');
      return projects > 0 ? `${base} in ${pluralize(projects, 'project', 'projects')}` : base;
    }

    case 'pending_secrets':
      if (count <= 0) return INBOX_NOTHING_WAITING;
      return appendAgentsProjects(
        formatCountNoun(count, capped, 'pending secret', 'pending secrets'),
        agents,
        projects
      );

    case 'reviews':
      if (count <= 0) return INBOX_NOTHING_WAITING;
      return formatCountNoun(count, capped, 'pending review', 'pending reviews');

    case 'chat': {
      const unread =
        breakdown && typeof breakdown.unreadConversations === 'number'
          ? breakdown.unreadConversations
          : count;
      const mentions = breakdown?.mentions ?? 0;
      const starred = breakdown?.starredMessages ?? 0;

      const clauses: string[] = [];
      if (unread > 0) {
        clauses.push(
          formatCountNoun(unread, capped, 'unread conversation', 'unread conversations')
        );
      }
      if (mentions > 0) {
        clauses.push(`${mentions} with mentions`);
      }
      if (starred > 0) {
        clauses.push(pluralize(starred, 'starred message', 'starred messages'));
      }
      if (clauses.length === 0) {
        return INBOX_NOTHING_WAITING;
      }
      const line = clauses.join(', ');
      return projects > 0 ? `${line} across ${pluralize(projects, 'project', 'projects')}` : line;
    }

    default:
      if (count <= 0) return INBOX_NOTHING_WAITING;
      return formatCountNoun(count, capped, 'item waiting', 'items waiting');
  }
}
