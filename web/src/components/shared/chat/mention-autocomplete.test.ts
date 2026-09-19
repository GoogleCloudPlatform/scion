// @vitest-environment happy-dom

import { describe, expect, it } from 'vitest';
import { ScionMentionAutocomplete } from './mention-autocomplete.js';

describe('mention candidate deduplication', () => {
  it('keeps the agent mention slug while removing a member with the same normalized slug', () => {
    const autocomplete = new ScionMentionAutocomplete();
    autocomplete.agents = [
      { id: 'agent-1', name: 'Coder One', slug: 'Coder One' },
    ] as typeof autocomplete.agents;
    autocomplete.members = [
      { id: 'member-1', name: 'Coder One', email: 'coder@example.com', kind: 'agent' },
    ];

    const candidates = autocomplete['matchCandidates']('');

    expect(candidates).toHaveLength(1);
    expect(candidates[0].slug).toBe('Coder One');
  });
});
