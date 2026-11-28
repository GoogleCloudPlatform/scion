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
 * Pure summary-of-state prose formatter for Inbox cards (design §6.2, §6.3, §14.3).
 * Mirrored in Go by `cmd/inbox_prose.go` and verified against the shared
 * fixture `pkg/hubclient/testdata/inbox_prose.json`.
 */

export const INBOX_NOTHING_WAITING = 'Nothing Waiting';

export type InboxLinkKey =
  | 'primary'
  | 'secrets-requests'
  | 'secrets-pending'
  | 'chat-unread'
  | 'chat-mentions'
  | 'chat-starred'
  | 'reviews-to-address'
  | 'reviews-requested'
  | 'reviews-drafts';

export interface ProseSegment {
  text: string;
  link?: InboxLinkKey;
}

function pluralize(n: number, singular: string, plural: string): string {
  return n === 1 ? `1 ${singular}` : `${n} ${plural}`;
}

function formatCountNoun(count: number, capped: boolean, singular: string, plural: string): string {
  if (capped) {
    return `${count}+ ${plural}`;
  }
  return pluralize(count, singular, plural);
}

function appendUnlinked(segments: ProseSegment[], text: string): void {
  if (!text) return;
  const last = segments[segments.length - 1];
  if (last && !last.link) {
    last.text += text;
    return;
  }
  segments.push({ text });
}

function buildAgentsProjectsSuffix(agents: number, projects: number): string {
  let out = '';
  if (agents > 0) {
    out += ` from ${pluralize(agents, 'agent', 'agents')}`;
  }
  if (projects > 0) {
    out += ` in ${pluralize(projects, 'project', 'projects')}`;
  }
  return out;
}

function primaryWithAgentsProjects(
  primaryText: string,
  agents: number,
  projects: number
): ProseSegment[] {
  const segments: ProseSegment[] = [{ text: primaryText, link: 'primary' }];
  appendUnlinked(segments, buildAgentsProjectsSuffix(agents, projects));
  return segments;
}

export function formatInboxProseSegments(
  category: string,
  count: number,
  capped: boolean,
  breakdown?: Record<string, number>
): ProseSegment[] | 'nothing' {
  const agents = breakdown?.agents ?? 0;
  const projects = breakdown?.projects ?? 0;

  switch (category) {
    case 'secrets': {
      const requests = breakdown?.secretRequests ?? 0;
      const pending = breakdown?.pendingSecrets ?? 0;
      const requestsCapped =
        Boolean(breakdown?.secretRequestsCapped) || (capped && requests > 0 && pending === 0);
      const pendingCapped =
        Boolean(breakdown?.pendingSecretsCapped) || (capped && pending > 0 && requests === 0);

      if (requests <= 0 && pending <= 0) {
        return 'nothing';
      }

      const segments: ProseSegment[] = [];
      if (requests > 0) {
        segments.push({
          text: formatCountNoun(requests, requestsCapped, 'secret request', 'secret requests'),
          link: 'secrets-requests',
        });
        if (agents > 0) {
          appendUnlinked(segments, ` from ${pluralize(agents, 'agent', 'agents')}`);
        }
      }
      if (pending > 0) {
        if (requests > 0) {
          appendUnlinked(segments, ', ');
        }
        segments.push({
          text: formatCountNoun(pending, pendingCapped, 'pending secret', 'pending secrets'),
          link: 'secrets-pending',
        });
        appendUnlinked(segments, ' to approve');
      }
      if (projects > 0) {
        appendUnlinked(segments, `, across ${pluralize(projects, 'project', 'projects')}`);
      }
      return segments;
    }

    case 'decisions':
      if (count <= 0) return 'nothing';
      return primaryWithAgentsProjects(
        formatCountNoun(count, capped, 'decision', 'decisions'),
        agents,
        projects
      );

    case 'agent_secrets':
      if (count <= 0) return 'nothing';
      return primaryWithAgentsProjects(
        formatCountNoun(count, capped, 'secret request', 'secret requests'),
        agents,
        projects
      );

    case 'agent_executions':
      if (count <= 0) return 'nothing';
      return primaryWithAgentsProjects(
        formatCountNoun(count, capped, 'execution request', 'execution requests'),
        agents,
        projects
      );

    case 'waiting': {
      if (count <= 0) return 'nothing';
      const segments: ProseSegment[] = [
        {
          text: formatCountNoun(count, capped, 'agent waiting', 'agents waiting'),
          link: 'primary',
        },
      ];
      if (projects > 0) {
        appendUnlinked(segments, ` in ${pluralize(projects, 'project', 'projects')}`);
      }
      return segments;
    }

    case 'pending_secrets':
      if (count <= 0) return 'nothing';
      return primaryWithAgentsProjects(
        formatCountNoun(count, capped, 'pending secret', 'pending secrets'),
        agents,
        projects
      );

    case 'reviews': {
      const hasToAddress = breakdown !== undefined && typeof breakdown.toAddress === 'number';
      const hasRequested = breakdown !== undefined && typeof breakdown.requested === 'number';
      const hasDrafts = breakdown !== undefined && typeof breakdown.drafts === 'number';
      if (!hasToAddress && !hasRequested && !hasDrafts) {
        if (count <= 0) return 'nothing';
        return [
          {
            text: formatCountNoun(count, capped, 'pending review', 'pending reviews'),
            link: 'primary',
          },
        ];
      }
      const toAddress = breakdown?.toAddress ?? 0;
      const requested = breakdown?.requested ?? 0;
      const drafts = breakdown?.drafts ?? 0;
      const explicitToAddressCapped = (breakdown?.toAddressCapped ?? 0) > 0;
      const explicitRequestedCapped = (breakdown?.requestedCapped ?? 0) > 0;
      // Fallback when a summary omits per-clause *Capped keys (mirrors CLI older-hub compat).
      const hasExplicitCap = explicitToAddressCapped || explicitRequestedCapped;
      const toAddressCapped = explicitToAddressCapped || (capped && !hasExplicitCap);
      const requestedCapped = explicitRequestedCapped || (capped && !hasExplicitCap);
      const draftsCapped = (breakdown?.draftsCapped ?? 0) > 0;

      const segments: ProseSegment[] = [];
      let hasClause = false;
      if (toAddress > 0) {
        segments.push({
          text: formatCountNoun(
            toAddress,
            toAddressCapped,
            'review to address',
            'reviews to address'
          ),
          link: 'reviews-to-address',
        });
        hasClause = true;
      }
      if (requested > 0) {
        if (hasClause) {
          appendUnlinked(segments, ', ');
        }
        segments.push({
          text: formatCountNoun(
            requested,
            requestedCapped,
            'review requested',
            'reviews requested'
          ),
          link: 'reviews-requested',
        });
        hasClause = true;
      }
      if (drafts > 0) {
        if (hasClause) {
          appendUnlinked(segments, ', ');
        }
        segments.push({
          text: formatCountNoun(drafts, draftsCapped, 'unfinished draft', 'unfinished drafts'),
          link: 'reviews-drafts',
        });
        hasClause = true;
      }
      if (!hasClause) {
        return 'nothing';
      }
      return segments;
    }

    case 'chat': {
      const unread =
        breakdown && typeof breakdown.unreadConversations === 'number'
          ? breakdown.unreadConversations
          : count;
      const mentions = breakdown?.mentions ?? 0;
      const starred = breakdown?.starredMessages ?? 0;
      const starredCapped = (breakdown?.starredMessagesCapped ?? 0) > 0;

      const segments: ProseSegment[] = [];
      let hasClause = false;

      if (unread > 0) {
        segments.push({
          text: formatCountNoun(unread, capped, 'unread conversation', 'unread conversations'),
          link: 'chat-unread',
        });
        hasClause = true;
      }
      if (mentions > 0) {
        if (hasClause) {
          appendUnlinked(segments, ', ');
        }
        segments.push({
          text: `${mentions} with mentions`,
          link: 'chat-mentions',
        });
        hasClause = true;
      }
      if (starred > 0) {
        if (hasClause) {
          appendUnlinked(segments, ', ');
        }
        segments.push({
          text: formatCountNoun(starred, starredCapped, 'starred message', 'starred messages'),
          link: 'chat-starred',
        });
        hasClause = true;
      }
      if (!hasClause) {
        return 'nothing';
      }
      if (projects > 0) {
        appendUnlinked(segments, ` across ${pluralize(projects, 'project', 'projects')}`);
      }
      return segments;
    }

    default:
      if (count <= 0) return 'nothing';
      return [
        { text: formatCountNoun(count, capped, 'item waiting', 'items waiting'), link: 'primary' },
      ];
  }
}

export function formatInboxProse(
  category: string,
  count: number,
  capped: boolean,
  breakdown?: Record<string, number>
): string {
  const segments = formatInboxProseSegments(category, count, capped, breakdown);
  if (segments === 'nothing') {
    return INBOX_NOTHING_WAITING;
  }
  return segments.map((s) => s.text).join('');
}
