---
title: Web Dashboard
description: Using the Scion Web Dashboard for visualization and control.
---

The Scion Web Dashboard provides a visual interface for managing your agents, projects, and runtime brokers. It complements the CLI by providing real-time status updates and easier management of complex environments.

## Overview

The dashboard is organized into several key areas:

### Dashboard Home
The landing page provides an overview of your active agents across all projects and the status of your runtime brokers.

### Notifications & Alerts
The dashboard features an integrated notification framework with real-time SSE delivery. 
- **Inbox Tray**: A dedicated tray accessible from the top navigation, providing a centralized view of all persistent messages sent by your agents. It features unread message badges and mark-as-read actions to help you track items requiring human input.
- **Notification Tray**: Provides agent-scoped filtering for status events, accessible directly from the top navigation.
- **Browser Push Notifications**: Opt-in native browser push notifications ensure you receive alerts even when the dashboard is in the background. Default triggers include `stalled` and `error` states, as well as requests for user input.

### Native Web Chat
When enabled via the `web.native_chat` feature flag, the dashboard includes a top-level **Native Web Chat** workspace (a fourth ShellType in the SPA). It offers a rich interface for direct communication and coordination with your running agents and team.
- **Project-Scoped Spaces & Shared Threads**: Conversations are organized into distinct spaces scoped to specific Projects. Within these spaces, users and agents can collaborate on shared discussion threads.
- **Direct Messages (DMs)**: Start 1-on-1 direct messages covering both human-to-human (H2H) and human-to-agent (H2A) communication, consolidated as a single "global pair" thread per pair.
- **Members Sidebar, Presence & Typing**: A right-hand sidebar displays active project members, showcasing real-time online presence status and typing indicators.
- **Composer Default-Agent Disambiguation**: When sending messages in spaces with multiple active agents, the composer helps resolve which agent is targeted if no explicit mention is used.
- **Attachments**: Upload file or image attachments directly within the composer.
- **Search**: Built-in chat search lets you query across historical messages and threads.
- **Chat/Log Switcher**: Instantly toggle between standard conversational chat with the agent and a real-time stream of the agent's raw execution logs inside the same view.
- **@-Mentions & Autocomplete**: Call other agents into the thread by typing `@` to trigger a fuzzy-matching, keyboard-navigable agent dropdown. Protected by code-fence guards to prevent triggering inside Markdown code snippets.
- **Visibility Density Filters**: Choose from three filter levels—**Conversation** (pure dialogue), **Verbose** (adds mentions/CCs), or **Full** (adds state updates and background processes)—with preferences saved individually per agent.
- **Coherence Sync**: Real-time sync ensures actions taken on external channels (e.g. Discord or Teams) propagate instantly to the Web UI, with delivery state tooltips indicating whether messages succeeded.

### Projects
View and manage your registered projects.
- **Create/Register Project**: Create a Hub-Managed workspace directly on the Hub, or connect a new remote Git repository. Includes a confirmation dialog when creating a project for an existing git repository.
- **Clone Project**: Deep-copy settings, labels, environment variables, skills, lifecycle hooks, harness configurations, and templates from an existing project to a new project. Built-in defer-driven rollback ensures transactions are atomic and safe. Supports an optional Git remote override so cloned templates and configurations carry over while target users can supply their own repository.
- **Project Settings**: Centralized configuration interface for managing project-scoped environment variables, secrets, and **injected skills**, including "Injection Mode" controls (Always vs. As-Needed). The settings page features a streamlined flow with a "Done" button, hides unnecessary registration options for git-backed projects, and displays Hub-default placeholders for unset configurations.
  - **Batch Skill Injection**: Paste a GitHub directory URL (e.g., referencing a folder containing multiple skill subdirectories) into the skill input to trigger directory-discovery. An interactive checkbox dialog allows selecting which skills to add, and submits them as a single atomic batch, with any embedded credentials/userinfo automatically stripped before logging for safety.
- **Workspace & File Management**: Access the comprehensive **inline file editor** to view and modify files directly in the browser, featuring integrated Markdown preview capabilities. The file browser supports **fuzzy and regex-based filtering** for fast navigation. You can also download individual workspace files or generate ZIP archives of entire projects directly from the UI.
- **Template Management**: Direct server-side importing of templates with immediate UI feedback. Includes full template file browsing, editing, and upload capabilities directly within the dashboard.
- **Shared Directory Management**: View and manage project shared directories directly from the Web UI (see [Project Shared Directories](/scion/local/workspace/#5-project-shared-directories)).
- **Agent List & Lineage Views**: See all agents belonging to the project, with options to toggle between card, list, and the interactive **Agent Lineage Graph** view.
  - **Lineage Graph View**: Renders parent/child agent relationships as a zoomable, pannable parent/child forest. Supports a left-to-right horizontal layout option (via a segmented toolbar toggle and `transposeLayout()` helper), custom keyboard shortcuts for fast navigation, updated edge and arrowhead geometry for precise alignment, and collapse pruning for complex trees.
  - **Inline Integration**: The lineage graph is an inline tree/graph component rendered directly in the main agent content slot on both the main Agents page and individual Project detail pages. Filters for agent status and labels apply automatically across card, list, and graph views.

### Agents
Detailed view for individual agents, featuring a high-density tabbed layout and improved breadcrumb navigation with a dedicated back button.
- **Unified Agent Creation**: A unified single-page interface for creating and configuring agents that merges the previous two-phase create/configure flow into a single step (removing the `editingAgentId` round-trip flow).
  - **Primary Fields**: The default visible section features fields for **Name**, **Project**, **Template**, **Harness** (Type), **Broker**, **Profile** (with a native Runtime Profile Selector that dynamically populates available profiles based on the selected broker), **Task**, and **Notify** settings.
  - **Collapsible Advanced Settings**: A collapsible advanced configuration area structured into 5 dedicated tabs:
    - **General**: Advanced execution settings, including the **auto-expose ports** checkbox and **Custom Branch Targeting** (which lets users direct agents to clone and check out specific git branches immediately upon creation).
    - **Auth & Security**: Late-binding authorization and role selections.
    - **Environment & Labels**: Key-value pairs for environment variables and metadata labels.
    - **Limits & Resources**: Granular control over maximum turns (`max_turns`), duration (`max_duration`), and other resource limits.
    - **Prompts**: Custom prompt overrides and initial system instruction settings.
- **Agent Identity & Roles**: The agent detail page includes an expanded GCP Identity card that displays the service account email and target Google Cloud project for all identity modes. This card also displays a color-coded role badge representing the agent's active authorization tier (`full`, `baseline`, `readonly`, or `none`).
- **Quick-Message Button**: Found on agent details, list, and graph cards. Instantly open an interactive modal dialog to chat with an agent (use `Enter` to send, `Shift+Enter` for a newline), gated by your existing message permissions.
- **Graph Card Terminal Shortcut**: Connect straight to an agent's interactive terminal directly from its card in the graph view via a dedicated, icon-only shortcut button. Gated by attach capability and disabled when the agent is offline.
- **Status Tab**: Real-time view of agent lifecycle (Starting, Thinking, Waiting, etc.), including the `suspended` and `error` phases. Includes **stalled agent detection** to flag agents that are alive but hung (activity `stalled`) and offline detection for agents whose heartbeat has gone silent (activity `offline`). A crashed agent (non-zero exit) is shown in the `error` phase with a message such as `Agent crashed with exit code N`, and can be restarted from the UI.
- **Logs Tab**: Streamed logs from the agent container via the integrated Cloud Log Viewer.
- **Messages Tab**: A dedicated tab for viewing structured messages sent to and from the agent.
- **Configuration Tab**: Dedicated tab for viewing the applied configuration of the agent, featuring a new telemetry configuration card.
- **Debug Panel**: A full-height panel providing a real-time stream of SSE events and internal state transitions for advanced troubleshooting and observability.
- **Terminal**: Interactive terminal access to the agent's workspace, featuring full Tmux support. Includes a dedicated terminal toolbar, seamless window switching (agent/shell), automatic window size adjustment, extended key sequence support (like `Shift+Enter`), and modifier-based text selection (`Shift`-drag or `Option`-drag on macOS). For detailed configuration, see [Interactive Sessions with Tmux](/scion/local/tmux/).
- **Workspace Content Previews**: Content preview capabilities for workspace files directly within the UI, allowing you to quickly inspect agent output and project data.
- **Lifecycle Control**: Start, stop, **suspend**, restart, or delete agents from the UI. Suspending an agent preserves its harness session so a later start *continues* the conversation rather than starting fresh, while restarting a crashed (`error`) agent runs a clean session. Includes bulk operations like the "Stop All" button for efficient bulk shutdown of all agents within a project. To reclaim resources, the Hub also **auto-suspends** agents that stay stalled past a grace period; they resume automatically on the next message. See [Agent Lifecycle](/scion/local/agent-lifecycle/).

### Runtime Brokers
Monitor the infrastructure nodes where your agents are executing.
- **Status**: See which brokers are online and their current load.
- **Configuration**: View broker capabilities (Docker, K8s, etc.).

### Admin Management Suite
Centralized views for managing the Scion infrastructure and access control (available to administrative users).
- **Users**: View and manage user accounts and roles.
- **Groups**: Create and manage organizational groups for policy-based authorization.
- **Service Accounts**: Manage and validate registered Google Service Accounts for use with the metadata emulation pipeline.
- **Brokers**: Comprehensive broker detail pages providing a grouped view of all active agents by their respective projects.
- **Server Configuration Editor**: A full-featured settings editor at `/admin/server-config`. Restructured the **General** settings tab into three dedicated cards (General, Agent Defaults with sub-tabs, and Project Default Settings). Adds `DefaultModel` and `DefaultThinkingLevel` fields to the defaults pipeline, moves the Telemetry toggle to Agent Defaults, and groups the Message Broker configuration in the Hub Server tab.
- **Maintenance Mode**: Toggle maintenance mode for the Hub and Web servers to facilitate safe infrastructure updates.

## Authentication

The dashboard supports several authentication methods:
- **OAuth (Google/GitHub)**: For standard user access.
- **Development Auto-login**: For local development.

See the [Authentication Guide](/scion/hosted/single-node/auth/) for setup instructions.

## API Proxying
The Go server handles API proxying, token injection, and session management so the browser never handles raw API keys or long-lived tokens directly.
