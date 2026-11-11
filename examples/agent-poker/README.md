# Agent Poker: Texas Hold'em

A demonstration of multi-agent collaboration using Scion. Multiple LLM agents play a game of Texas Hold'em poker, coordinated through shared state and group messaging.

## Overview

This example uses three agent templates:

- **Dealer** (1 agent) - Manages the game: shuffles/deals cards, maintains the shared game state, enforces rules, and coordinates turns.
- **Player** (2+ agents) - Plays poker: receives cards, makes betting decisions, and tries to win chips.
- **Auditor** (1 agent) - Watches for cheating: receives shadow copies of all dealt cards and validates that player actions are legitimate.

## How It Works

### Communication
All game communication happens via **group (broadcast) messages** through the scion CLI. Players announce their actions (fold, call, raise) publicly. The dealer announces game state transitions (flop, turn, river, showdown).

The dealer sends **direct messages** to the auditor with private information (each player's hole cards) so the auditor can independently verify fair play.

### Shared State
The dealer maintains a `card-table.json` file in the workspace that represents the current state of the table. All agents can read this file, but **only the dealer writes to it**. This file contains:
- Community cards
- Current pot and side pots
- Player chip stacks
- Current betting round and active player
- Bet history for the current hand

### Card Management
The dealer has a Python script (`deck.py`) that manages a standard 52-card deck with persistent state. This ensures true randomness and prevents card duplication across draws.

## Setup

```bash
# Initialize a project for the poker game
scion init poker-night

# Import the templates
scion templates import --all https://github.com/GoogleCloudPlatform/scion/tree/main/examples/agent-poker/templates

# Create the dealer agent from template
scion create dealer --template poker-dealer

# The dealer's initial prompt should specify how many players to create.
# Example:
scion message dealer "Start a 4-player Texas Hold'em game"
```

The dealer will then:
1. Start the requested number of player agents
2. start one auditor agent
3. Establish the card table game state
4. Initialize the deck and deal the first hand
5. Begin coordinating play through group messages

## Game Flow

1. **Pre-hand**: Dealer shuffles (re-initializes) the deck, posts blinds, and deals 2 hole cards to each player via direct message.
2. **Pre-flop**: Players act in turn order (call, raise, or fold) via group messages.
3. **Flop**: Dealer reveals 3 community cards by updating `card-table.json` and announcing via group message.
4. **Turn**: Dealer reveals 1 more community card.
5. **River**: Dealer reveals the final community card.
6. **Showdown**: Remaining players reveal hands. Dealer evaluates and awards the pot.
7. **Repeat**: Dealer button rotates and a new hand begins.

## Anti-Cheat

The auditor independently tracks:
- Which cards were dealt to whom
- Whether player-claimed hands match what was actually dealt
- Whether bet amounts are valid given chip stacks
- Whether players act out of turn

If the auditor detects cheating, they announce it via group message. The dealer will then ban the offending player and forfeit their chips.
