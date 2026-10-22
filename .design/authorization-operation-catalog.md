# Authorization Operation Catalog

*Generated from Go-native OperationSpec definitions. Do not edit manually.*

**Operations:** 346

## Table of Contents

- [agent.lifecycle.create](#agentlifecyclecreate) — Create an agent in a project
- [agent.lifecycle.delete](#agentlifecycledelete) — Delete an agent
- [agent.lifecycle.control](#agentlifecyclecontrol) — Start, stop, suspend or restart an agent
- [agent.lifecycle.restore](#agentlifecyclerestore) — Restore a soft-deleted agent
- [agent.lifecycle.exec](#agentlifecycleexec) — Run a command in an agent's container
- [agent.lifecycle.env](#agentlifecycleenv) — Submit environment values to an agent
- [agent.lifecycle.resetauth](#agentlifecycleresetauth) — Reset an agent's harness authentication
- [agent.hold.lift](#agentholdlift) — Lift the holds of a suspended agent whose owners are admitted to its project again (hub admin)
- [agent.lifecycle.reincarnate](#agentlifecyclereincarnate) — Reincarnate an agent
- [agent.read](#agentread) — Read a single agent's metadata by ID
- [agent.list](#agentlist) — List agents within the caller's authorized project scope. A user access token needs agent:list on either boundary: a project token lists only its boundary project's agents. The project form lists one project's agents and decides agent.list on that project
- [agent.update](#agentupdate) — Update agent configuration or metadata
- [agent.status.update](#agentstatusupdate) — Report an agent's status: the agent itself, or a user who may update the agent
- [agent.workspace.read](#agentworkspaceread) — Read an agent's workspace sync status
- [agent.workspace.sync](#agentworkspacesync) — Sync an agent's workspace from or to the agent
- [agent.logs.read](#agentlogsread) — Read an agent's runtime logs and Cloud Logging entries, and stream its Cloud Logging entries. A stream opened by a token ends once the token stops validating or loses agent.read
- [agent.messages.read](#agentmessagesread) — Read and stream an agent's message history and message logs. agent.attach gives the full history; agent.read alone gives the messages the caller sent or received. A stream opened by a token ends once the token stops validating or loses the permission that admitted it
- [agent.metrics.read](#agentmetricsread) — Read an agent's aggregate session metrics
- [agent.lifecycle.keys](#agentlifecyclekeys) — Send keystrokes to an agent's session
- [agent.action.message](#agentactionmessage) — Send a message to an agent through its message action
- [agent.attach](#agentattach) — Attach to an agent session via WebSocket. A session opened by a token closes with 4401 once the token stops validating or loses agent.attach
- [agent.portaccess](#agentportaccess) — Access forwarded ports on an agent
- [agent.port.list](#agentportlist) — List an agent's exposed ports
- [agent.port.register](#agentportregister) — Register, delete or clear an agent's exposed ports
- [agent.port.tunnel](#agentporttunnel) — Open the calling agent's own port tunnel
- [agent.setmessagemode](#agentsetmessagemode) — Change an agent's message mode
- [agent.token.refresh](#agenttokenrefresh) — Refresh the calling agent's own hub token
- [agent.outbound.message](#agentoutboundmessage) — Deliver an outbound message from the calling agent
- [agent.metrics.report](#agentmetricsreport) — Report runtime metrics for the calling agent
- [agent.secrets.access](#agentsecretsaccess) — List, read and write the secrets available to the calling agent: its project's and its creating user's
- [agent.stopall](#agentstopall) — Stop all running agents on the hub
- [agent.stopall.project](#agentstopallproject) — Stop all running agents in a project
- [project.membership.add](#projectmembershipadd) — Add a member to a project with a specified role
- [project.membership.update](#projectmembershipupdate) — Change a project member's role
- [project.membership.remove](#projectmembershipremove) — Remove a member from a project
- [project.membership.list](#projectmembershiplist) — List project members and their roles
- [project.membership.transfer](#projectmembershiptransfer) — Atomically transfer project ownership from the actor to another user
- [project.membership.principal.set](#projectmembershipprincipalset) — Replace a principal's whole set of project roles
- [project.membership.principal.remove](#projectmembershipprincipalremove) — Remove every project role a principal holds
- [project.membership.rolebinding.create](#projectmembershiprolebindingcreate) — Create a project-scoped binding of a built-in project role through the role-binding API
- [project.membership.rolebinding.delete](#projectmembershiprolebindingdelete) — Delete a project-scoped role binding through the role-binding API
- [project.visitors.list](#projectvisitorslist) — List a project's visitor grants (federated accounts holding the project-visitor role)
- [project.visitors.add](#projectvisitorsadd) — Grant a user of a trusted issuer visitor access to a project by email; creates the federated account in status pending when none exists
- [project.visitors.update](#projectvisitorsupdate) — Replace the window (not before, expires at) of a project visitor grant
- [project.visitors.remove](#projectvisitorsremove) — Revoke a project visitor grant; the visitor's next request is refused. Revoking a binding a visitor rule created also excludes the visitor from that rule, so the rule does not bind them again
- [project.visitors.rules.list](#projectvisitorsruleslist) — List a project's visitor rules (visitor access by issuer and email domain)
- [project.visitors.rules.add](#projectvisitorsrulesadd) — Add a visitor rule: visitor access to a project for every user of a trusted issuer whose verified email is in a domain, bound per user on the user's first matching request
- [project.visitors.rules.update](#projectvisitorsrulesupdate) — Replace the window (not before, expires at) of a project visitor rule and of every visitor binding it created
- [project.visitors.rules.remove](#projectvisitorsrulesremove) — Remove a project visitor rule; every visitor binding it created and its exclusions are deleted in the same transaction, and grants by email are unchanged
- [project.lifecycle.create](#projectlifecyclecreate) — Create a new project. project.create is checked as a collection-level create at the hub scope; a token needs project:create on a hub boundary. Naming an existing project by id needs project.update on it
- [project.lifecycle.delete](#projectlifecycledelete) — Delete a project with cascading security state cleanup and atomic audit
- [project.read](#projectread) — Read a single project's metadata by ID or slug
- [project.list](#projectlist) — List projects within the caller's authorized scope. A user access token needs project:list on either boundary: a project token lists only its boundary project
- [project.update](#projectupdate) — Update project settings and metadata
- [project.register](#projectregister) — Register a project from an external source. Register checks project.create as a collection-level create at the hub scope; a token needs project:create on a hub boundary. An existing project named by id or matched by slug needs project.update on it. The embedded-broker form (a broker in the request body) is session-only (CREDENTIAL_MANAGEMENT): a token or any other non-session credential is refused before any lookup or write
- [project.clone](#projectclone) — Clone a project into a new project owned by the caller. The caller needs project.read on the source and project.create as a collection-level create at the hub scope; a token also needs the project:clone selector
- [schedule.event.list](#scheduleeventlist) — List scheduled events or recurring schedules in a project
- [schedule.event.read](#scheduleeventread) — Read a scheduled event, a recurring schedule or a schedule's run history
- [schedule.event.create](#scheduleeventcreate) — Create a scheduled event or recurring schedule of any event type. A user access token needs scheduled_event:create and passes the event type's target checks
- [schedule.event.update](#scheduleeventupdate) — Update or resume a recurring schedule of any event type. A user access token needs scheduled_event:update and passes the event type's target checks
- [schedule.event.pause](#scheduleeventpause) — Pause a recurring schedule. Pausing only stops future runs, so a token with scheduled_event:update is admitted
- [schedule.event.delete](#scheduleeventdelete) — Cancel a scheduled event or delete a recurring schedule
- [schedule.event.fire](#scheduleeventfire) — Fire a scheduled message or dispatch_agent event, one-shot or materialized from a recurring schedule, under the authority its revision recorded
- [artifact.read](#artifactread) — Read an artifact's metadata or file bytes (owner, home-project readers via the scope grant, or principal grants); unreadable artifacts answer 404
- [artifact.list](#artifactlist) — List the artifacts the caller owns, holds a grant on, or that are shared to a project it is a member of (?mine=1); each row passes the artifact.read check, so an artifact the caller cannot read is omitted, never denied
- [artifact.create](#artifactcreate) — Create an artifact homed in a project (the caller's own, or ?scope=): a single file published at once (?name=), or a manifest that starts a pending first version (JSON body), whose files are then uploaded and finalized through the artifact.version.upload and artifact.version.finalize routes
- [artifact.update](#artifactupdate) — Set or clear an artifact's expiry, or move it to another home project (PATCH); a move also requires artifact.create in the target project and cross-project sharing on; it removes the old home project's grant, so that project's members lose read unless another grant covers them, and gives the target project a read grant unless it already holds a scope grant (an admin one is refused with 409 home_admin_grant); a steward may change the expiry but a move from it is refused with 403 steward_action_not_allowed
- [artifact.grant.list](#artifactgrantlist) — List an artifact's principal and scope grants, home grant first, with whether cross-project sharing is on
- [artifact.grant.put](#artifactgrantput) — Give a user, agent or project read, write or admin on an artifact, or a user review, or change an existing grant's permission (POST); a steward (on an artifact owned by an agent: an owner of its home project, or the agent's delegating user who reads in the home project) gives read, review or write, and an admin grant from it is refused with 403 steward_action_not_allowed; a caller that may not manage the artifact may still give a user the review level on an artifact owned by an agent when it is that agent's delegating user or an admin of the artifact's home project (project.manage there; ReviewGrantClause), to a user who can already read it through the home project or a grant to one of the user's projects, never replacing a read, write or admin grant; a caller with that authority asking for a well-formed review grant is refused with 403 review_subject_cannot_read when the user cannot read the artifact and 403 review_replaces_access when the user holds a read, write or admin grant; every other request from such a caller is refused with the same 403, and an accepted one can still answer 409 too_many_grants, or 404 when the artifact went away meanwhile
- [artifact.grant.delete](#artifactgrantdelete) — Remove one grant from an artifact; the home project's grant cannot be removed
- [artifact.link.list](#artifactlinklist) — List an artifact's unexpired share links (ids, creator, expiry; never the tokens)
- [artifact.link.create](#artifactlinkcreate) — Create a share link: an unguessable token, shown once, that lets anyone holding it read the artifact's current version without signing in until it expires (at most the hub's maximum lifetime, never past the artifact's own expiry) or is revoked
- [artifact.link.revoke](#artifactlinkrevoke) — Revoke one share link of an artifact
- [artifact.delete](#artifactdelete) — Delete an artifact with its grants and share links (its files are removed after the hub's grace period); allowed for its managers (the owner or an admin grantee), on an artifact owned by an agent for its stewards (an owner of its home project, or the agent's delegating user who reads there), and, as custodian, for an admin of its home project (project.manage there), each a user whose credential permits artifact.manage in the home project
- [artifact.version.create](#artifactversioncreate) — Start a pending version of an artifact from a manifest; a review grantee may start review versions only
- [artifact.version.upload](#artifactversionupload) — Upload one manifest file of a pending version; only the principal that started the version may upload to it
- [artifact.version.finalize](#artifactversionfinalize) — Finalize a pending version once every file has arrived, making it ready (and current unless a later one is); only the principal that started it may finalize it, and a review is checked against its base
- [artifact.version.view](#artifactversionview) — Get a view URL for a ready version whose entry is HTML: a capability, valid for a limited time, that serves only that version's files under /api/v1/artifacts/view/ in the sandboxed viewer (see the view exemption); the caller needs only the read check
- [artifact.version.read](#artifactversionread) — List an artifact's versions (ready ones, and discarded ones by number and reason) or read one ready version's metadata and file list
- [project.env.read](#projectenvread) — Read a project's environment variables (list or one key)
- [project.env.write](#projectenvwrite) — Set or delete a project environment variable
- [project.secret.read](#projectsecretread) — Read a project's secret metadata (list or one key)
- [project.secret.write](#projectsecretwrite) — Set, patch or delete a project secret
- [project.providers.list](#projectproviderslist) — List the runtime brokers that provide for a project
- [project.shareddir.read](#projectshareddirread) — List a project's shared directories and read their files and archives
- [project.shareddir.write](#projectshareddirwrite) — Create or delete a project shared directory, and upload, write or delete its files
- [project.injectedskills.read](#projectinjectedskillsread) — List the skills injected into a project's agents
- [project.injectedskills.write](#projectinjectedskillswrite) — Add, replace or remove skills injected into a project's agents
- [project.gcpsa.create](#projectgcpsacreate) — Register a project-scoped GCP service account. Project-route service account writes use project.manage
- [project.messagelogs.read](#projectmessagelogsread) — Read and stream a project's message log. A stream opened by a token ends once the token stops validating or loses project.read
- [project.broadcast](#projectbroadcast) — Broadcast a message to a project's agents. Each recipient is then filtered by agent.message
- [project.metrics.read](#projectmetricsread) — Read a project's metrics summary, session metrics summary and metrics dashboard
- [project.prestarthooks.read](#projectprestarthooksread) — List or read a project's pre-start hooks
- [project.prestarthooks.write](#projectprestarthookswrite) — Create, update, delete or activate a project pre-start hook
- [project.settings.read](#projectsettingsread) — Read a project's settings and its resolved settings
- [project.settings.update](#projectsettingsupdate) — Replace a project's settings
- [project.messagingpolicy.read](#projectmessagingpolicyread) — Read a project's cross-project inbound messaging policy
- [project.messagingpolicy.update](#projectmessagingpolicyupdate) — Set a project's cross-project inbound messaging policy. The caller also must be an active direct project owner or a local unscoped hub admin
- [project.template.set](#projecttemplateset) — Mark or unmark a project as a template. The caller needs project.update and project.clone on the project
- [template.project.import](#templateprojectimport) — Discover or import templates into a project
- [harnessconfig.project.import](#harnessconfigprojectimport) — Discover or import harness configs into a project
- [project.workspace.read](#projectworkspaceread) — Read a project's workspace: WebDAV reads, sync status, cache status, archive and file reads
- [project.workspace.write](#projectworkspacewrite) — Change a project's workspace: WebDAV writes, cache refresh and notify, file upload, write and delete, and git pull
- [project.github.read](#projectgithubread) — Read a project's GitHub status, GitHub permissions and git identity
- [project.github.write](#projectgithubwrite) — Change a project's GitHub installation, status check, GitHub permissions and git identity
- [project.members.assignableroles](#projectmembersassignableroles) — List the roles the caller may assign in a project
- [agent.message.send](#agentmessagesend) — Send a message to an agent
- [inbox.message.read](#inboxmessageread) — List and read the caller's own inbox messages. A project token lists only messages of its boundary project
- [inbox.message.write](#inboxmessagewrite) — Mark the caller's own inbox messages read. Mark-all by a project token touches only messages of its boundary project
- [inbox.channels.list](#inboxchannelslist) — List the registered message channels: static capability metadata with no records
- [inbox.capabilities.read](#inboxcapabilitiesread) — Read the hub messaging capabilities: static capability metadata with no records
- [inbox.conversation.list](#inboxconversationlist) — List the caller's conversations. A group conversation is listed only while the caller can read it (project:read on its project, or the participant rule for a group with no project; a token is checked as its user); a participant row alone does not list it. A token lists only conversations inside its boundary, and a direct conversation with an agent only with agent:read on that agent; the project group union also needs project:read
- [inbox.conversation.create](#inboxconversationcreate) — Create a group conversation in a project. Needs project:read on the project; a token also needs inbox:write for it
- [project.conversation.read](#projectconversationread) — Read a group conversation, its messages and one message. Needs project:read on the conversation's project; a group with no project needs participation, and a token needs inbox:read on a hub boundary for it
- [inbox.conversation.direct.read](#inboxconversationdirectread) — Read a direct conversation, its messages and one message. A token needs inbox:read for the peer agent's project and agent:read on the peer agent; a direct conversation between users needs a hub boundary
- [inbox.conversation.defaultagent.set](#inboxconversationdefaultagentset) — Set the default agent of a group conversation. Needs project:read on the conversation's project; a token also needs inbox:write for it
- [inbox.conversation.participant.add](#inboxconversationparticipantadd) — Add a participant to a group conversation. Every caller needs project:read on the conversation's project; an added agent must be in that project and an added user must be a member of it; a token also needs inbox:write for it. A caller who is not a participant or cannot read the group gets the unknown-conversation answer, and an agent of another project the unknown-agent answer
- [inbox.conversation.leave](#inboxconversationleave) — Leave a conversation the caller takes part in. A token needs inbox:write for the conversation
- [inbox.conversation.resolve](#inboxconversationresolve) — Resolve a conversation reference. A group reference needs project:read on its project and an agent reference needs agent:read on the agent, for every user caller; a token also needs inbox:read for the result
- [project.threadgroup.list](#projectthreadgrouplist) — List a project's chat thread groups. Needs project:read on the project; an agent must belong to it
- [project.threadgroup.read](#projectthreadgroupread) — Read a chat thread group. Needs project:read on the group's project; an agent must belong to it
- [inbox.threadgroup.create](#inboxthreadgroupcreate) — Create a chat thread group in a project. Needs project:read on the project (an agent must belong to it); a token also needs inbox:write for it
- [inbox.threadgroup.update](#inboxthreadgroupupdate) — Rename a chat thread group, or add or remove one of its threads. Needs project:read on the group's project (an agent must belong to it); a token also needs inbox:write for it. An added thread must be a live group conversation of the same project
- [inbox.threadgroup.delete](#inboxthreadgroupdelete) — Delete a chat thread group; its threads become ungrouped. Users only (an agent gets 403). Needs project:read on the group's project; a token also needs inbox:write for it
- [agent.message.target.resolve](#agentmessagetargetresolve) — Resolve a messaging target in another project through the agent message authorization. Only a target the caller may message is answered; any other target gets the unknown-target answer
- [inbox.notification.read](#inboxnotificationread) — List the caller's notifications. A token lists only rows inside its boundary; with agentId, rows addressed to the agent subscriber need agent:read on that agent, for every user caller
- [inbox.notification.ack](#inboxnotificationack) — Acknowledge the caller's notifications. Ack-all by a project token touches only rows of its boundary project
- [inbox.notification.subscription.create](#inboxnotificationsubscriptioncreate) — Create notification subscriptions. A user caller needs project:read on the project and agent:read on a watched agent; a token also needs inbox:write for the project
- [inbox.notification.subscription.read](#inboxnotificationsubscriptionread) — List the caller's notification subscriptions. A token lists only rows inside its boundary
- [inbox.notification.subscription.write](#inboxnotificationsubscriptionwrite) — Update and delete the caller's notification subscriptions. A token changes only rows inside its boundary
- [inbox.notification.template.create](#inboxnotificationtemplatecreate) — Create a subscription template. A template filed under a project needs project:read on it; a token also needs inbox:write for it
- [inbox.notification.template.read](#inboxnotificationtemplateread) — List subscription templates: only templates of projects the caller may read, and for a token only templates inside its boundary
- [inbox.notification.template.delete](#inboxnotificationtemplatedelete) — Delete a subscription template the caller created. A token needs inbox:write for the template's project
- [project.conversation.gcsobject.read](#projectconversationgcsobjectread) — Fetch a gs:// object named in a message of a project group conversation, through the sending agent's assigned service account. Needs project:read on the conversation's project; a token needs it inside its boundary
- [web.events.stream](#webeventsstream) — Stream hub events to the web client. The web server admits only a signed-in web session; a token never opens the stream
- [chat.prefs.read](#chatprefsread) — Read the caller's visibility preference for an agent's chat thread. Needs agent:read on the agent; a token also needs inbox:read for the agent's project
- [chat.prefs.update](#chatprefsupdate) — Write the caller's visibility preference for an agent's chat thread. Needs agent:read on the agent; a token also needs inbox:write for the agent's project
- [chat.space.list](#chatspacelist) — List the chat spaces: the projects the caller can read. A project token lists only its boundary project; the caller's rail preferences need inbox:read on a hub boundary
- [chat.unreadcount.read](#chatunreadcountread) — Count the caller's conversations with unread messages. A token needs inbox:read; threads count only in projects it can read and DMs only where the DM rule passes
- [chat.thread.list](#chatthreadlist) — List the threads of a chat space. Needs project:read on the space's project
- [chat.thread.create](#chatthreadcreate) — Create a thread in a chat space. Needs project:read on the space's project; a token also needs inbox:write for it
- [chat.space.members.read](#chatspacemembersread) — List the members of a chat space. Needs project:read on the space's project; agent rows also need the agent list scope
- [chat.space.markread](#chatspacemarkread) — Mark every thread of a chat space read for the caller. Needs project:read on the space's project; a token also needs inbox:write for it
- [chat.space.emoji.update](#chatspaceemojiupdate) — Set or clear the emoji of a chat space. Needs project:update on the space's project
- [chat.topic.read](#chattopicread) — Read a chat thread. Needs project:read on the thread's project
- [chat.topic.update](#chattopicupdate) — Rename a chat thread or set its default agent. Needs project:read on the thread's project; a token also needs inbox:write for it
- [chat.topic.delete](#chattopicdelete) — Delete a chat thread (soft delete; the last thread of a space stays). Needs project:read on the thread's project; a token also needs inbox:write for it
- [chat.conversation.history.topic](#chatconversationhistorytopic) — Read the messages of a chat thread. Needs project:read on the thread's project
- [chat.conversation.history.dm](#chatconversationhistorydm) — Read the messages of one of the caller's DMs. A token needs inbox:read: a DM with an agent needs the agent's project inside its boundary and agent:read on the agent; a DM between users needs a hub boundary
- [chat.conversation.send.topic](#chatconversationsendtopic) — Post a message to a chat thread. Needs project:read on the thread's project; a token also needs inbox:write for it. Agent recipients need agent:message
- [chat.conversation.send.dmagent](#chatconversationsenddmagent) — Post a message to one of the caller's DMs with an agent. Needs agent:message on the agent; a token also needs inbox:write for the agent's project
- [chat.conversation.send.dmuser](#chatconversationsenddmuser) — Post a message to one of the caller's DMs with another user. Every caller needs read access to the peer user record; a token also needs inbox:write on a hub boundary
- [chat.message.edit](#chatmessageedit) — Edit one of the caller's chat messages. Only the author edits, and only before an agent replies. A token needs inbox:write, and the message's conversation is re-checked: project:read for a thread, the DM rule for a DM
- [chat.message.delete](#chatmessagedelete) — Delete one of the caller's chat messages (soft delete). Only the author deletes, and only before an agent replies. A token needs inbox:write, and the message's conversation is re-checked: project:read for a thread, the DM rule for a DM
- [chat.conversation.readstate.topic](#chatconversationreadstatetopic) — Read the caller's read state on a chat thread. Needs project:read on the thread's project
- [chat.conversation.readstate.dm](#chatconversationreadstatedm) — Read the caller's read state on one of its DMs. A token needs inbox:read under the DM rule
- [chat.conversation.markread.topic](#chatconversationmarkreadtopic) — Move the caller's read marker on a chat thread. Needs project:read on the thread's project; a token also needs inbox:write for it
- [chat.conversation.markread.dm](#chatconversationmarkreaddm) — Move the caller's read marker on one of the caller's DMs. A token needs inbox:write under the DM rule
- [chat.conversation.markunread.topic](#chatconversationmarkunreadtopic) — Mark unread for the caller a chat thread. Needs project:read on the thread's project; a token also needs inbox:write for it
- [chat.conversation.markunread.dm](#chatconversationmarkunreaddm) — Mark unread for the caller one of the caller's DMs. A token needs inbox:write under the DM rule
- [chat.conversation.mute.topic](#chatconversationmutetopic) — Mute or unmute for the caller a chat thread. Needs project:read on the thread's project; a token also needs inbox:write for it
- [chat.conversation.mute.dm](#chatconversationmutedm) — Mute or unmute for the caller one of the caller's DMs. A token needs inbox:write under the DM rule
- [chat.conversation.pin.topic](#chatconversationpintopic) — Pin or unpin for the caller a chat thread. Needs project:read on the thread's project; a token also needs inbox:write for it
- [chat.conversation.pin.dm](#chatconversationpindm) — Pin or unpin for the caller one of the caller's DMs. A token needs inbox:write under the DM rule
- [chat.conversation.typing.topic](#chatconversationtypingtopic) — Publish the caller's typing indicator on a chat thread. Needs project:read on the thread's project; a token also needs inbox:write for it
- [chat.conversation.typing.dm](#chatconversationtypingdm) — Publish the caller's typing indicator on one of the caller's DMs. A token needs inbox:write under the DM rule
- [chat.conversation.interagent.read](#chatconversationinteragentread) — Read the messages an agent in one of the caller's DMs exchanged with other agents. Needs DM participation and agent:attach on the agent
- [chat.dm.list](#chatdmlist) — List the caller's DMs. A token needs inbox:read and lists only the DMs the DM rule lets it read
- [chat.userprefs.read](#chatuserprefsread) — Read the caller's chat rail preferences, a project-less record. A token needs inbox:read on a hub boundary
- [chat.userprefs.update](#chatuserprefsupdate) — Write the caller's chat rail preferences, a project-less record. A federated caller is refused. A token needs inbox:write on a hub boundary
- [chat.presence.update](#chatpresenceupdate) — Publish the caller's presence. It fans out only on the named projects the caller can read (project:read), each once, at most 1000; the others are left out. A federated caller is refused. A token also needs inbox:write
- [chat.search.topic](#chatsearchtopic) — Search the messages of one chat thread. Needs project:read on the thread's project
- [chat.search.project](#chatsearchproject) — Search the messages of one project's chat. Needs project:read on the project; a token keeps a DM row only when the DM rule lets it read the DM under inbox:read
- [chat.search.unscoped](#chatsearchunscoped) — Search the messages of every project the caller can read. A project token searches only its boundary project; a token keeps a DM row only when the DM rule lets it read the DM under inbox:read
- [chat.search.dm](#chatsearchdm) — Search the messages of one of the caller's DMs. A token needs inbox:read under the DM rule
- [chat.attachment.upload.project](#chatattachmentuploadproject) — Upload chat attachments to a space. Needs project:read on the space's project; a token also needs inbox:write for it
- [chat.attachment.upload.noproject](#chatattachmentuploadnoproject) — Upload project-less chat attachments, reachable only through the messages that reference them. A federated caller is refused. A token needs inbox:write on a hub boundary
- [chat.attachment.download.project](#chatattachmentdownloadproject) — Download a chat attachment of a project. Needs project:read on the project; a session also reads its own uploads and files attached to messages it can read
- [chat.conversation.promote](#chatconversationpromote) — Promote one of the caller's DMs with an agent into a thread of the agent's space. Needs DM participation and project:read on the agent's project; a token also needs inbox:write under the DM rule
- [role.definition.create](#roledefinitioncreate) — Create a custom role definition (create, import, duplicate). A token needs role:create and role:read on a hub boundary and the holder's live role.create authority; the holder must hold every permission of the role within the token's selectors. A project-scoped definition can be bound in every project, so it is judged on the holder's system-scope authority; a system-scoped definition is refused to every token
- [role.definition.update](#roledefinitionupdate) — Update a custom role definition. A token needs role:update and role:read on a hub boundary and the holder's live role.update authority; the holder must hold every permission of the updated role within the token's selectors, judged as for create
- [role.definition.delete](#roledefinitiondelete) — Delete a custom role definition. A token needs role:delete and role:read on a hub boundary and the holder's live role.delete authority; system roles and roles that are still bound are kept
- [role.binding.create](#rolebindingcreate) — Create a role binding (grant authority to a principal) for a custom project role or at system scope. A token needs role_binding:create on a hub boundary and the holder's live role_binding.create authority. A system-scoped binding is refused to every token. For a custom project role the holder must also have current project.manage access to the binding's project and hold every permission of the role in that project within the token's selectors
- [role.binding.delete](#rolebindingdelete) — Delete a system-scoped role binding (revoke authority from a principal). A token needs role_binding:delete on a hub boundary and the holder's live role_binding.delete authority; the constraint-admin lockout guard applies. Deleting a super-admin binding is refused to every token (session only)
- [group.member.add](#groupmemberadd) — Add a member to a group. A user access token needs group:addMember on a hub boundary, and the group's closure may carry no authority beyond the token's own
- [group.member.remove](#groupmemberremove) — Remove a member from a group
- [group.delete](#groupdelete) — Delete a group
- [access.constraint.create](#accessconstraintcreate) — Create an access constraint (tighten boundary). A token needs access_constraint:admin and access_constraint:read on a hub boundary and the holder's live access_constraint.admin authority; the preview token and governance checks apply, and the commit re-checks judge the holder's authority within the token's selectors
- [access.constraint.update](#accessconstraintupdate) — Update an access constraint (may relax or tighten boundary). A token needs access_constraint:admin and access_constraint:read on a hub boundary and the holder's live access_constraint.admin authority, and may only tighten: an update that relaxes the boundary (adds permissions, changes subject or scope, or narrows or adds its time window) is session only. The commit re-checks judge the holder's authority within the token's selectors
- [access.constraint.delete](#accessconstraintdelete) — Delete an access constraint (relax boundary). Session only: deleting a boundary relaxes it, and a user access token is refused before the preview token is used
- [access.constraint.preview](#accessconstraintpreview) — Generate an access constraint preview and read a preview job and its result. A token needs access_constraint:admin on a hub boundary and the holder's live access_constraint.admin authority
- [credential.token.read](#credentialtokenread) — List or read the caller's own user access tokens
- [credential.token.create](#credentialtokencreate) — Create a user access token (UAT)
- [credential.token.revoke](#credentialtokenrevoke) — Revoke or delete a user access token
- [federation.token.mint](#federationtokenmint) — Obtain a short-lived user access credential for the caller, signed with the hub's OIDC key, for one peer hub audience listed in server.oidc.user_token_audiences. Only a local human user with status active; a user access token needs federation_token:mint on a hub boundary. 404 while federation.project_visitors is off
- [user.admin.suspend](#useradminsuspend) — Suspend or reactivate a user account (dispatched from PATCH /api/v1/users/{id} when status field is present). A token needs user:suspend on a hub boundary, checked on the hub-level user record. The caller's own status and activating an invited user are refused for every credential
- [user.admin.invite](#useradmininvite) — Invite users to the platform (single or bulk) and manage invitation codes (list, create, read stats, read, revoke, delete). The invited record has status invited, the placeholder role member and no role binding or group membership; an invitation code names no role. A token needs user:invite on the hub boundary and the holder's live user.invite authority
- [user.admin.provision](#useradminprovision) — Pre-register a user (status invited) through POST /api/v1/users; invitation-equivalent, shares the invite creation core; no role, no grants
- [user.admin.promote](#useradminpromote) — Promote or demote a user's administrative level (dispatched from PATCH /api/v1/users/{id} when role field is present)
- [user.admin.delete](#useradmindelete) — Delete a user account
- [group.read](#groupread) — Read group details
- [group.list](#grouplist) — List groups. Each row is shown only when the caller may read it (group.read); a user access token needs group:read on a hub boundary, and a group with no project resolves to the hub
- [group.create](#groupcreate) — Create a new group. A user access token needs group:create on a hub boundary; the target is the hub's group collection. Creating under a parent group runs the member-add delegation check on the parent
- [group.update](#groupupdate) — Update group metadata. A user access token that changes the owner needs group:update and live update access on the group, and for the new owner it must pass the delegation check of a member add on the group's role bindings; group:addMember and the group role hierarchy do not apply. A refused owner change returns one fixed 403, including for a group that does not exist
- [user.read](#userread) — Read a user profile
- [user.list](#userlist) — List users. A session sees the users it may read. A token needs user:list at the hub collection on a hub boundary, and then sees only the users it may read, each checked as a hub-level user record, so it also needs user:read
- [user.update](#userupdate) — Update another user's profile (display name and preferences). The same PATCH dispatches user.admin.suspend for a status field and user.admin.promote for a role field, and every field's check passes before any write. A token needs user:update on a hub boundary, checked on the hub-level user record; a token PATCH that names no status field is checked as a profile update
- [user.self.update](#userselfupdate) — Update the caller's own profile (display name and preferences) through PATCH /api/v1/users/{id} naming the caller's own record. A status field on the caller's own record is refused by the self-status guard
- [role.read](#roleread) — Read role definitions and the permission registry. A token needs role:read on a hub boundary; the write methods on the same routes also need their own selector
- [role.binding.read](#rolebindingread) — Read role binding assignments
- [access.constraint.read](#accessconstraintread) — Read access constraint definitions and the principals a constraint affects. A token needs access_constraint:read on a hub boundary; the write methods on the same routes also need access_constraint:admin
- [hub.audit.constraint](#hubauditconstraint) — Read one access constraint's audit history. hub.audit.read is a hub-level permission: a token needs hub_audit:read on a hub boundary and is checked against the hub-level constraint record; a denied caller gets the same not-found answer as for a missing constraint
- [user.session.logout](#usersessionlogout) — Sign-in flow logout step; the hub holds no server-side session state for it to change
- [user.session.revoke](#usersessionrevoke) — Revoke every cookie session of a user (platform admin only)
- [user.terminalworkspace](#userterminalworkspace) — Read or replace the caller's own terminal workspace
- [user.skillinjection.update](#userskillinjectionupdate) — Add, replace or remove the skills injected into the caller's own agents. A user access token needs user_skill_injection:update on a hub boundary
- [user.skillinjection.read](#userskillinjectionread) — List the skills injected into the caller's own agents. Any token of the holder reads its own entries; the entries belong to no project
- [template.user.read](#templateuserread) — List, read or download the caller's own user-scope templates. A user access token needs template:read on a hub boundary
- [template.user.create](#templateusercreate) — Create a template in the caller's own user scope; the scope and owner are the caller. A user access token needs template:create on a hub boundary
- [template.user.update](#templateuserupdate) — Update, upload to or finalize one of the caller's own user-scope templates. A user access token needs template:update on a hub boundary
- [template.user.delete](#templateuserdelete) — Delete one of the caller's own user-scope templates, through /api/v1/users/me/templates/{id} or /api/v1/templates/{id}. A user access token needs template:delete on a hub boundary
- [hub.audit.effectiveaccess](#hubauditeffectiveaccess) — Read any principal's effective access composition at the system scope. A token needs hub_audit:read on a hub boundary; HEAD answers whether the caller may read it
- [hub.audit.explain](#hubauditexplain) — Explain another principal's authorization, or compare two principals' effective permissions. A token needs hub_audit:read on a hub boundary; grant details of the other principal are redacted
- [user.self.explain](#userselfexplain) — Explain the caller's own authorization. A token's decisions apply its ceiling and boundary, and its effective permission list holds only the permissions it may use; a project-boundary token explains only targets in its boundary project, while its holder is a current member of that project
- [user.self.profile](#userselfprofile) — Read the caller's own profile and preferences. Any token reads its holder's own profile. A visitor reads its own federated account; an unlinked federated user is refused
- [user.self.scopes](#userselfscopes) — List the token selectors and, for a requested boundary, the caller's own mint eligibility for each. Any token reads it
- [user.self.adminstatus](#userselfadminstatus) — Read the caller's own admin status and system-scope permissions. For a token the permission list holds only the permissions its ceiling allows that are eligible for its boundary kind
- [user.self.groups](#userselfgroups) — List the caller's own effective groups. A project-boundary token lists groups that belong to no project and the groups of its boundary project, the latter only while its holder is a current member of that project
- [user.invite.redeem](#userinviteredeem) — Redeem an invitation code for the caller's identity. Session-only: a token does not bind an invitation to its holder
- [user.chatlink.verify](#userchatlinkverify) — Link a pending chat account to the caller's identity with a verification code. Session-only: a token does not bind an external account to its holder
- [hub.authreset](#hubauthreset) — Reset all agent authentication credentials (emergency action)
- [hub.authreset.reissue](#hubauthresetreissue) — Re-issue an agent's role scopes from its delegator's current authority (dispatched from POST .../agents/{id}/reset-auth when reissue_scopes is set; hub super-admin only)
- [hub.authreset.reissueall](#hubauthresetreissueall) — Re-issue every agent's role scopes from its delegator's current authority (dispatched from POST /api/v1/admin/agents/reset-auth-all when reissue_scopes is set; dry run by default; hub super-admin only)
- [hub.config.read](#hubconfigread) — Read server configuration and schema
- [hub.config.update](#hubconfigupdate) — Update server configuration sections. The route guard checks hub.config.read, so a token needs hub_config:read and hub_config:update, and writes configuration keys only
- [hub.messaging.update](#hubmessagingupdate) — Read and update messaging configuration switches
- [hub.profiling.update](#hubprofilingupdate) — Read and update the profiling switches (session only)
- [hub.experiments.update](#hubexperimentsupdate) — Read and update hub-wide experiment overrides
- [hub.conduitgrantkeys.rotate](#hubconduitgrantkeysrotate) — Rotate the conduit grant signing key (kids and timestamps only in the response)
- [hub.conduitgrantkeys.revoke](#hubconduitgrantkeysrevoke) — Revoke a conduit grant key at once (the kid only in the response)
- [hub.maintenance.execute](#hubmaintenanceexecute) — Execute maintenance operations including migrations and restarts
- [hub.adminmode.read](#hubadminmoderead) — Read admin/maintenance mode state
- [hub.adminmode.update](#hubadminmodeupdate) — Toggle admin/maintenance mode
- [hub.allowlist.read](#huballowlistread) — Read the platform email allow list and its domains. A token needs hub_allow_list:read on the hub boundary; it does not admit the allow-list writes
- [hub.allowlist.update](#huballowlistupdate) — Manage the platform email allow list: add and import create invited records (status invited, placeholder role member, no role binding or group membership); delete removes an invited record under the last-project-owner guard and binding cascade of user.admin.delete. The route guard checks hub.allow_list.read, so a token needs hub_allow_list:read and hub_allow_list:update on the hub boundary
- [hub.health.read](#hubhealthread) — Read platform health summary and GCP quota status
- [hub.diagnostics.read](#hubdiagnosticsread) — Read diagnostic logs, the diagnostic log stream and messaging divergence data. The log stream re-checks a token credential on every heartbeat and ends once the token stops validating or loses hub.diagnostics.read
- [hub.scheduler.read](#hubschedulerread) — Read scheduler status and configuration
- [hub.projectdefaults.read](#hubprojectdefaultsread) — Read project default settings
- [hub.lifecyclehooks.read](#hublifecyclehooksread) — Read lifecycle hook definitions
- [hub.projectdefaults.update](#hubprojectdefaultsupdate) — Update project default settings. The route guard checks hub.project_defaults.read, so a token needs hub_project_defaults:read and hub_project_defaults:update, and writes configuration keys only
- [hub.lifecyclehooks.update](#hublifecyclehooksupdate) — Create, update, delete and activate hub lifecycle hooks and hub pre-start hooks. The admin lifecycle-hook route guard checks hub.lifecycle_hooks.read, so a token writing there needs hub_lifecycle_hooks:read and hub_lifecycle_hooks:update
- [hub.settings.update](#hubsettingsupdate) — Set the user-defined hub injected skills; system entries are preserved
- [hub.validate.execute](#hubvalidateexecute) — Validate resource definitions against schema
- [hub.integrations.read](#hubintegrationsread) — Read integration configurations, the available-integrations list, integration health and integration update status
- [hub.integrations.update](#hubintegrationsupdate) — Update an integration's settings and restart an integration. The route guard checks hub.integrations.read, so a token needs hub_integrations:read and hub_integrations:update. A config update that sets secrets or any settings key outside the configuration set requires an interactive session
- [hub.integrations.install](#hubintegrationsinstall) — Install an integration and start an integration update; both build and install code on the hub host, so an interactive session only
- [hub.teamsmanifest.read](#hubteamsmanifestread) — Read Teams integration manifest
- [hub.metrics.read](#hubmetricsread) — Read metrics dashboard data
- [hub.githubapp.read](#hubgithubappread) — Read GitHub App configuration and installations
- [hub.githubapp.update](#hubgithubappupdate) — Create, update and delete GitHub App installations, discover installations and sync permissions
- [hub.githubapp.config.update](#hubgithubappconfigupdate) — Update the GitHub App configuration, which sets the hub's app credentials; an interactive session only
- [quota.read](#quotaread) — Read limit definitions, entitlements, and usage. The route guard checks quota.read on the hub-scoped quota target
- [quota.create](#quotacreate) — Create limit definitions and entitlement bindings. The route guard checks quota.read, so a token needs quota:read and quota:create
- [quota.update](#quotaupdate) — Update limit definitions and entitlement bindings. The route guard checks quota.read, so a token needs quota:read and quota:update
- [quota.brokersettings.update](#quotabrokersettingsupdate) — Replace a runtime broker's settings document. Requires broker.read on the broker and quota.update on the hub-scoped quota target for every write, including one that changes no key, and each changed key's declared permission (maxAgents declares quota.update)
- [quota.delete](#quotadelete) — Delete limit definitions and entitlement bindings. The route guard checks quota.read, so a token needs quota:read and quota:delete
- [quota.usage.self.read](#quotausageselfread) — Read the caller's own quota usage: each limit's effective value and active count for the caller only
- [hub.policies.removed](#hubpoliciesremoved) — Removed policy API; every method and sub-path answers 410 Gone and points callers to role bindings
- [skill.list](#skilllist) — List skills; each row is filtered by skill read. A user access token needs skill:list on either boundary; its page, total and cursor count only the rows it may read
- [skill.read](#skillread) — Read a skill, its versions, its download URLs, its resolved version and its files. A record the caller may not read answers as not found
- [skill.create](#skillcreate) — Create a skill in a project named by the request body. A user access token needs skill:create on a boundary that contains the project
- [skill.update](#skillupdate) — Update a skill: its metadata, version publishing and deprecation, file uploads and finalize. A user-scope skill refuses a federated caller (requireProfileWriter)
- [skill.delete](#skilldelete) — Delete a skill definition. A user-scope skill refuses a federated caller (requireProfileWriter)
- [skill.register](#skillregister) — Manage skill registries and their pinned hashes. The route guard checks skill.register on the hub-scoped registry target, so a user access token needs skill:register on a hub boundary
- [template.list](#templatelist) — List templates; each row is filtered by template read. The unfiltered list is used only with hub-level evidence for template.list: a user access token needs template:list on a hub boundary and hub-wide list authority for it. Every other token, project tokens included, gets the per-row template read scan
- [template.read](#templateread) — Read a template, its download URLs, its storage validation and its files. A record the caller may not read answers as not found
- [template.create](#templatecreate) — Create a template in the global, project or caller's own user scope the request body names. A user-scope create refuses a federated caller (requireProfileWriter)
- [template.update](#templateupdate) — Update a template: its definition and metadata, file uploads, finalize and file writes. A user-scope template refuses a federated caller (requireProfileWriter)
- [template.delete](#templatedelete) — Delete a global or project template. A user-scope template is template.user.delete
- [harnessconfig.list](#harnessconfiglist) — List harness configurations; each row is filtered by harness config read. The unfiltered list is used only with hub-level evidence for harness_config.list: a user access token needs harness_config:list on a hub boundary and hub-wide list authority for it. Every other token, project tokens included, gets the per-row harness config read scan
- [harnessconfig.read](#harnessconfigread) — Read a harness configuration, its download URLs, its storage validation, its image status and its files. A record the caller may not read answers as not found
- [harnessconfig.create](#harnessconfigcreate) — Create a harness configuration in the global, project or caller's own user scope the request body names; a user-scope config's scope and owner are the caller. A user-scope harness config refuses a federated caller (requireProfileWriter)
- [harnessconfig.update](#harnessconfigupdate) — Update a harness configuration: its definition and metadata, file uploads, finalize, the image check and file writes. On a user-scope harness config the existing route gate (authorizeHarnessConfigRoute) already refuses a federated caller, with requireProfileWriter behind it
- [harnessconfig.delete](#harnessconfigdelete) — Delete a global or project harness configuration. The route gate needs harness_config read and the delete needs harness_config delete on the owning scope. A user-scope config is harnessconfig.user.delete
- [skill.global.create](#skillglobalcreate) — Create a skill in the global catalog (global or core scope). The check is skill.create_global on a collection-level request at the hub scope, so a user access token needs skill:create_global on a hub boundary
- [skill.discover.project](#skilldiscoverproject) — Discover the skills in a remote GitHub directory for a project named by the request body, spending that project's GitHub credentials. A user access token needs skill:create on a boundary that contains the project
- [skill.discover.global](#skilldiscoverglobal) — Discover the skills in a public remote GitHub directory with no project named. A user access token needs skill:create_global on a hub boundary
- [skill.resolve.local](#skillresolvelocal) — Resolve skill:// references against the hub catalog. Each candidate is checked for skill read; a candidate the caller may not read answers as not found
- [skill.resolve.github.project](#skillresolvegithubproject) — Resolve gh:// references with a project named by the request body, using that project's GitHub credentials. The caller needs skill read on the project, so a user access token needs skill:read on a boundary that contains the project; a refused reference answers forbidden
- [skill.resolve.github.public](#skillresolvegithubpublic) — Resolve gh:// references from public repositories with no project named; the hub fetches them and writes its resolution cache. A user access token needs skill:read on the global catalog, on a hub boundary; a refused reference answers forbidden
- [skill.file.signedread](#skillfilesignedread) — Read one skill version file through a signed URL issued by the skill download. The signature alone authorizes exactly the skill, version and file it was issued for; no principal is evaluated
- [template.clone](#templateclone) — Clone a readable template into the global scope or a project the request body names. The source needs template read; the destination needs template create on its scope, so a user access token needs template:read and template:create on a boundary that contains both
- [template.reimport](#templatereimport) — Reimport a global or project template from its source. The template needs template read and the reimport needs template create on the owning scope, as import does
- [template.import.global](#templateimportglobal) — Import or discover templates for the global catalog. The check is template create on a collection-level request at the hub scope, so a user access token needs template:create on a hub boundary
- [template.import.project](#templateimportproject) — Import or discover templates for a project named by the request body. The check is template create on a collection-level request inside that project
- [harnessconfig.clone](#harnessconfigclone) — Clone a readable harness configuration into the global scope or a project the request body names. The route gate needs harness_config read on the source; the destination needs harness_config create on its scope
- [harnessconfig.reimport](#harnessconfigreimport) — Reimport a global or project harness configuration from its source. The route gate needs harness_config read and the reimport needs harness_config create on the owning scope, as import does
- [harnessconfig.import.global](#harnessconfigimportglobal) — Import or discover harness configurations for the global catalog. The check is harness_config create on a collection-level request at the hub scope, so a user access token needs harness_config:create on a hub boundary
- [harnessconfig.import.project](#harnessconfigimportproject) — Import or discover harness configurations for a project named by the request body. The check is harness_config create on a collection-level request inside that project
- [template.user.clone](#templateuserclone) — Clone a readable template into the caller's own user scope. The source needs template:read; a user access token needs template:create on a hub boundary for the destination
- [template.user.reimport](#templateuserreimport) — Reimport one of the caller's own user-scope templates from its source. The read gate needs template:read and the reimport needs template:update on the record, on a hub boundary for a user access token
- [skill.user.create](#skillusercreate) — Create a skill in the caller's own user scope; the scope and owner are the caller. A user access token needs skill:create on a hub boundary
- [harnessconfig.user.clone](#harnessconfiguserclone) — Clone a readable harness configuration into the caller's own user scope. The source needs harness_config:read; a user access token needs harness_config:create on a hub boundary for the destination
- [harnessconfig.user.delete](#harnessconfiguserdelete) — Delete one of the caller's own user-scope harness configurations. The route gate needs harness_config:read and the delete needs harness_config:delete, on a hub boundary for a user access token
- [harnessconfig.user.reimport](#harnessconfiguserreimport) — Reimport one of the caller's own user-scope harness configurations from its source. The route gate needs harness_config:read and the reimport needs harness_config:update, on a hub boundary for a user access token
- [template.user.import](#templateuserimport) — Import or discover templates for the caller's own user scope. A user access token needs template:create on a hub boundary
- [harnessconfig.user.import](#harnessconfiguserimport) — Import or discover harness configurations for the caller's own user scope. A user access token needs harness_config:create on a hub boundary
- [broker.read](#brokerread) — Read runtime broker status or list brokers
- [broker.material.read](#brokermaterialread) — Read a runtime broker's environment variables (sensitive values masked) and secret metadata. A user needs broker.read on the broker and, beyond it, the owner relationship or broker.update; the broker reads its own material
- [broker.material.write](#brokermaterialwrite) — Set, patch or delete a runtime broker's environment variables and secrets. A user needs an interactive session and broker.update on the broker; the broker writes its own material
- [broker.update](#brokerupdate) — Update a runtime broker's name, labels or host service account. A user needs an interactive session and broker.update on the broker
- [broker.delete](#brokerdelete) — Deregister a runtime broker: unlink it from every project, clear project defaults that name it, and remove its credentials. A user needs an interactive session and broker.delete on the broker
- [broker.heartbeat](#brokerheartbeat) — Record a broker heartbeat: broker liveness and the state of the agents it runs. The broker sends its own; a user needs an interactive session and broker.update on the broker from a role binding (the owner relationship does not apply)
- [broker.agent.launchreport](#brokeragentlaunchreport) — Record a broker's launch report for an agent it runs
- [broker.messagefailures.report](#brokermessagefailuresreport) — Record buffered message delivery failures reported by a broker
- [broker.controlchannel.call](#brokercontrolchannelcall) — Carry a call between the hub and a connected broker over the control channel; each call runs under the operation that initiated it
- [gcp.identity.create](#gcpidentitycreate) — Create a GCP service account binding
- [gcp.identity.delete](#gcpidentitydelete) — Delete a GCP service account binding
- [gcp.identity.assign](#gcpidentityassign) — Assign a GCP service account to an agent
- [gcp.identity.mint](#gcpidentitymint) — Mint a GCP access token for a service account
- [secret.read](#secretread) — Read project secrets or environment variables containing secrets
- [secret.write](#secretwrite) — Create, update or delete secrets. At user scope, the default, the secrets are the caller's own and a federated caller is refused (requireProfileWriter)
- [gcp.identity.read](#gcpidentityread) — Read GCP service account details or list accounts. A project's list requires project.read on it (members, and agents of that project only); the hub-scoped list requires gcp_service_account.list at hub scope (hub members)
- [gcp.identity.verify](#gcpidentityverify) — Verify a GCP service account's IAM configuration
- [env.read](#envread) — Read project environment variables
- [env.hub.list](#envhublist) — List hub-level environment variables (scope=hub), without secret entries
- [testidentity.create](#testidentitycreate) — Issue a short-lived synthetic member or viewer test identity and one access token for it (no refresh token, no cookie)
- [testidentity.list](#testidentitylist) — List test identities: the caller's own, or every identity for an unscoped platform admin session
- [testidentity.token.issue](#testidentitytokenissue) — Re-issue one access token for a live test identity, for its issuer or an unscoped platform admin session; an admin-kind identity's token is re-issued only through the admin-scope workload delegation that issued it (phase X4b)
- [testidentity.webcode.issue](#testidentitywebcodeissue) — Mint a single-use, five-minute web-login code for a live test identity, for its issuer, a workload issuer delegation on the identities it issued (member scope: member-tier identities; admin scope, phase X4b: admin-kind identities of its kind), or an unscoped platform admin session; only on hubs where test identity web sessions are available. Kind test_fixture needs test_identity.issue; an admin kind (test_hub_admin, test_super_admin) only while server.hub.test_identities.admin_web_sessions is on at an admin --test-mode level, and only with the issuer checks of that kind's issuance (its capability, a live super-admin through current bindings, an access token with exactly the kind's scope)
- [testidentity.delete](#testidentitydelete) — Delete a test identity (its role bindings, group memberships and user-scope data go with it), for its issuer, an unscoped platform admin session, or a holder of test_identity.issue and user.delete; refused with 409 while it owns agents or is a project's last owner. With ?purge=true it also removes the agents, schedules, groups and projects stamped as created under the identity (the identity ID is the only input), keeps stamped projects that have another owner or other resources, and reports unstamped agents as unproven; for an admin-kind identity (purged by its issuer or a super-admin) it first removes every grant stamped with its issuance and reports its residue
- [testidentity.cleanup](#testidentitycleanup) — Remove the agents, schedules, groups and projects stamped as created under the caller's expired or deleted test identities (and the expired identities themselves), with the purge engine; the hub selects the targets from the provenance stamps, the request names none (no body; ?dryRun=true lists the targets and writes nothing). An issuer reaches its own identities, a workload delegation only the identities issued through it, an unscoped platform admin session every issuer's; a live identity is never touched
- [testidentity.provenance](#testidentityprovenance) — Explain whether the hub recorded one agent, project, schedule or group as created under a test identity of the caller's issuer scope (its own, its own delegation's, or every issuer's for an unscoped platform admin session): proven with the stamp and the identity's state, proven deleted by an in-scope deletion record for a resource that is gone, or unproven with only the reason (a missing resource and another issuer's stamp get the same answer, and no owner or creator data is returned)
- [testidentity.admin.create](#testidentityadmincreate) — Issue a short-lived test identity holding an expiring hub-admin role (on a hub started with --test-mode=admin or above) or super-admin role (--test-mode=super-admin), and one access token for it (no refresh token, no cookie)
- [testidentity.admin.revokeall](#testidentityadminrevokeall) — Revoke every admin-kind test identity: remove every grant stamped with its issuance, then delete it (or, when the user delete refuses it, suspend it and remove its role bindings), for a human super-admin session
- [testidentity.admin.residue](#testidentityadminresidue) — Read the residue report of an admin-kind test identity, live or purged: the grants and resources stamped with it, and its changes to rows without its stamp, for a human super-admin session
- [testidentity.delegation.create](#testidentitydelegationcreate) — Delegate test identity issuance to one pinned workload identity (a subject in the server.hub.test_identities.workload_issuers allowlist) with one scope: test_identity:issue (member and viewer; member cap, lifetime at most 7 days), or on a hub running the admin tier test_identity:issue_admin (admin cap 1-2, at most 8 h) or test_identity:issue_super_admin (admin cap 1, at most 2 h)
- [testidentity.delegation.list](#testidentitydelegationlist) — List test identity issuer delegations: the caller's own, or every delegation for an unscoped platform admin session
- [testidentity.delegation.revoke](#testidentitydelegationrevoke) — Revoke a test identity issuer delegation, for its grantor or an unscoped platform admin session; the next mint through it is refused
- [agent.delegation.create](#agentdelegationcreate) — Issue an agent delegation grant binding one agent the issuer controls to a boundary, a frozen permission ceiling and an expiry
- [agent.delegation.exchange](#agentdelegationexchange) — Exchange an agent delegation grant, with the bound agent's own agent token, for a short-lived opaque delegated credential
- [agent.delegation.revoke](#agentdelegationrevoke) — Revoke an agent delegation grant and every credential exchanged from it
- [agent.delegation.credential.revoke](#agentdelegationcredentialrevoke) — Revoke one delegated credential exchanged from an agent delegation grant
- [agent.delegation.list](#agentdelegationlist) — List agent delegation grants: those bound to one agent, or those the caller issued; grants outside the caller's authority are left out
- [agent.delegation.read](#agentdelegationread) — Read one agent delegation grant; a grant outside the caller's authority answers as missing

---

## agent.lifecycle.create

**Domain:** agent

**Description:** Create an agent in a project

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/agents` |
| http_route | POST | `/api/v1/projects/{projectId}/agents` |

**Principals:** `user`, `agent`

**Credentials:** `session_jwt`, `scoped_uat`, `agent_jwt`

**Bearer:** `admit` (target `project_body`; boundaries `project`, `hub`)

**Base Permission:** `agent.create`

**Resource Resolver:** project-from-body

**Effects:** `create-resource`, `grant-authority`

### Delegation

- **Kind:** `non_amplification`
- Actor must hold the role and scopes delegated to the new agent (CanDelegate non-amplification); an agent actor is also evaluated against the delegation ceiling of its live delegation chain for agent.create on the target project

### Audit

- **Event Type:** `agent_delegation`
- **Context Fields:** actor_id
- **After Fields:** agent_id, can_delegate_result
- **Atomic:** Yes

**Denial Codes:** `forbidden`, `conflict`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`
- `pkg/hub:TestAgentCreate_ExplicitRoleAboveParentDenied`
- `pkg/hub:TestAgentCreate_RequiresLiveDelegator`
- `pkg/hub:TestCreateAuditFailureRollsBack`

---

## agent.lifecycle.delete

**Domain:** agent

**Description:** Delete an agent

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | DELETE | `/api/v1/agents/{id}` |
| http_route | DELETE | `/api/v1/projects/{projectId}/agents/{id}` |

**Principals:** `user`, `agent`

**Credentials:** `session_jwt`, `scoped_uat`, `agent_jwt`

**Bearer:** `admit` (target `agent_record`; boundaries `project`, `hub`)

**Base Permission:** `agent.delete`

**Resource Resolver:** agent-from-url

**Effects:** `delete-resource`

### Audit

- **Event Type:** `agent.lifecycle.delete`
- **Context Fields:** actor_id, project_id
- **Before Fields:** agent_id, agent_name
- **Atomic:** Yes

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`

---

## agent.lifecycle.control

**Domain:** agent

**Description:** Start, stop, suspend or restart an agent

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/agents/{id}/start` |
| http_route | POST | `/api/v1/agents/{id}/stop` |
| http_route | POST | `/api/v1/agents/{id}/suspend` |
| http_route | POST | `/api/v1/agents/{id}/restart` |
| http_route | POST | `/api/v1/projects/{projectId}/agents/{id}/start` |
| http_route | POST | `/api/v1/projects/{projectId}/agents/{id}/stop` |
| http_route | POST | `/api/v1/projects/{projectId}/agents/{id}/suspend` |
| http_route | POST | `/api/v1/projects/{projectId}/agents/{id}/restart` |

**Principals:** `user`, `agent`

**Credentials:** `session_jwt`, `scoped_uat`, `agent_jwt`

**Bearer:** `admit` (target `agent_record`; boundaries `project`, `hub`)

**Base Permission:** `agent.lifecycle`

**Resource Resolver:** agent-from-url

**Effects:** `update-resource`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestAgentSubRoute_CatalogDrift`

---

## agent.lifecycle.restore

**Domain:** agent

**Description:** Restore a soft-deleted agent

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/agents/{id}/restore` |
| http_route | POST | `/api/v1/projects/{projectId}/agents/{id}/restore` |

**Principals:** `user`, `agent`

**Credentials:** `session_jwt`, `scoped_uat`, `agent_jwt`

**Bearer:** `admit` (target `agent_record`; boundaries `project`, `hub`)

**Base Permission:** `agent.lifecycle`

**Resource Resolver:** agent-from-url

**Effects:** `update-resource`

**Denial Codes:** `forbidden`, `conflict`

### Tests

- `pkg/hub:TestAgentSubRoute_CatalogDrift`

---

## agent.lifecycle.exec

**Domain:** agent

**Description:** Run a command in an agent's container

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/agents/{id}/exec` |
| http_route | POST | `/api/v1/projects/{projectId}/agents/{id}/exec` |

**Principals:** `user`, `agent`

**Credentials:** `session_jwt`, `scoped_uat`, `agent_jwt`

**Bearer:** `admit` (target `agent_record`; boundaries `project`, `hub`)

**Base Permission:** `agent.attach`

**Resource Resolver:** agent-from-url

**Effects:** `update-resource`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestAgentSubRoute_CatalogDrift`

---

## agent.lifecycle.env

**Domain:** agent

**Description:** Submit environment values to an agent

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/agents/{id}/env` |
| http_route | POST | `/api/v1/projects/{projectId}/agents/{id}/env` |

**Principals:** `user`, `agent`

**Credentials:** `session_jwt`, `scoped_uat`, `agent_jwt`

**Bearer:** `admit` (target `agent_record`; boundaries `project`, `hub`)

**Base Permission:** `agent.attach`

**Resource Resolver:** agent-from-url

**Effects:** `update-resource`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestAgentSubRoute_CatalogDrift`

---

## agent.lifecycle.resetauth

**Domain:** agent

**Description:** Reset an agent's harness authentication

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/agents/{id}/reset-auth` |
| http_route | POST | `/api/v1/projects/{projectId}/agents/{id}/reset-auth` |

**Principals:** `user`, `agent`

**Credentials:** `session_jwt`, `scoped_uat`, `agent_jwt`

**Bearer:** `admit` (target `agent_record`; boundaries `project`, `hub`)

**Base Permission:** `agent.attach`

**Resource Resolver:** agent-from-url

**Effects:** `update-resource`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestAgentSubRoute_CatalogDrift`

---

## agent.hold.lift

**Domain:** agent

**Description:** Lift the holds of a suspended agent whose owners are admitted to its project again (hub admin)

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/agents/{id}/hold/lift` |

**Principals:** `user`

**Credentials:** `session_jwt`

**Bearer:** `session_only` (reason `GOV_PENDING`)

**Base Permission:** `agent.update`

**Resource Resolver:** agent-from-url

**Effects:** `update-resource`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestAgentSubRoute_CatalogDrift`
- `pkg/hub:TestAgentHoldLift`

---

## agent.lifecycle.reincarnate

**Domain:** agent

**Description:** Reincarnate an agent

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/agents/{id}/reincarnate` |
| http_route | POST | `/api/v1/projects/{projectId}/agents/{id}/reincarnate` |

**Principals:** `user`, `agent`

**Credentials:** `session_jwt`, `scoped_uat`, `agent_jwt`

**Bearer:** `admit` (target `agent_record`; boundaries `project`, `hub`)

**Base Permission:** `agent.lifecycle`

**Resource Resolver:** agent-from-url

**Effects:** `update-resource`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestAgentSubRoute_CatalogDrift`

---

## agent.read

**Domain:** agent

**Description:** Read a single agent's metadata by ID

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/agents/{id}` |
| http_route | GET | `/api/v1/projects/{projectId}/agents/{id}` |

**Principals:** `user`, `agent`, `agent_delegated`

**Credentials:** `session_jwt`, `scoped_uat`, `agent_jwt`, `delegated_agent`

**Bearer:** `admit` (target `agent_record`; boundaries `project`, `hub`)

**Base Permission:** `agent.read`

**Resource Resolver:** agent-from-url

**Effects:** `read-one`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`

---

## agent.list

**Domain:** agent

**Description:** List agents within the caller's authorized project scope. A user access token needs agent:list on either boundary: a project token lists only its boundary project's agents. The project form lists one project's agents and decides agent.list on that project

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/agents` |
| http_route | GET | `/api/v1/projects/{id}/agents` |

**Principals:** `user`, `agent`

**Credentials:** `session_jwt`, `scoped_uat`, `agent_jwt`

**Bearer:** `admit` (target `list_scope`; boundaries `project`, `hub`)

**Base Permission:** `agent.list`

**Resource Resolver:** list-scope-resolver

**Effects:** `list-scoped`

### Invariants

| ID | Kind | Description | Fail-Closed |
|----|------|-------------|-------------|
| scope-pushed-query | security | Rows, totalCount, and nextCursor come from the same SQL predicate that includes the authorization scope | Yes |
| cursor-scope-binding | security | Cursor binding includes endpoint, caller filters, authorization scope, and principal/credential context | Yes |
| no-broad-query-on-none | security | ScopeSetNone produces empty list without issuing any resource query | Yes |
| slug-not-oracle | security | Project slug lookup for agent list filter must not distinguish unauthorized from nonexistent | Yes |

**Denial Codes:** `forbidden`, `credential_insufficient`, `user_suspended`

### Tests

- `pkg/hub:TestRS2_AgentListScopePushed`
- `pkg/hub:TestRS2_AgentListMineSharedClassification`
- `pkg/hub:TestRS2_AgentListSlugOracle`
- `pkg/hub:TestRS2_AgentListMultiPageInterleaved`
- `pkg/hub:TestRS2_FailureInjection_PrincipalGroupClosure`
- `pkg/hub:TestRS2_FailureInjection_StoreListCount`
- `pkg/hub:TestRS2_CursorReplayAfterGrantRemoval`
- `pkg/hub:TestRS2_CursorReplayAfterBindingExpiry`
- `pkg/hub:TestRS2_AllPlusConstraint_EndToEnd`
- `pkg/hub:TestRS2_ProductionAgentJWT`
- `pkg/hub:TestRS2_SystemAllSharedSemantics`
- `pkg/hub:TestRS2_GroupChangeCursorReplay`
- `pkg/hub:TestRS2_ConstraintChangeCursorReplay`
- `pkg/hub:TestRS2_TransitiveGroupAccess`
- `pkg/hub:TestRS2_FilterCompositionMatrix`
- `pkg/hub:TestListCaveats_ProjectBoundaryRequiresListPermissionInCeiling`
- `pkg/hub:TestProjectAgentListToken_RequiresAgentListOnTheProject`
- `pkg/hub:TestListFilters_CannotEnlargeTokenResults`

---

## agent.update

**Domain:** agent

**Description:** Update agent configuration or metadata

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | PATCH | `/api/v1/agents/{id}` |
| http_route | PATCH | `/api/v1/projects/{projectId}/agents/{id}` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `agent_record`; boundaries `project`, `hub`)

**Base Permission:** `agent.update`

**Resource Resolver:** agent-from-url

**Effects:** `update-resource`

**Denial Codes:** `forbidden`, `conflict`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`
- `pkg/hub:TestAgentUpdateToken_PassthroughGateStillApplies`
- `pkg/hub:TestAgentUpdateToken_ServiceAccountAssignRequiresAssignSelector`

---

## agent.status.update

**Domain:** agent

**Description:** Report an agent's status: the agent itself, or a user who may update the agent

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/agents/{id}/status` |
| http_route | POST | `/api/v1/projects/{projectId}/agents/{id}/status` |

**Principals:** `user`, `agent`

**Credentials:** `session_jwt`, `scoped_uat`, `agent_jwt`

**Bearer:** `admit` (target `agent_record`; boundaries `project`, `hub`)

**Base Permission:** `agent.update`

**Resource Resolver:** agent-from-url

**Effects:** `update-resource`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestAgentSubRoute_CatalogDrift`

---

## agent.workspace.read

**Domain:** agent

**Description:** Read an agent's workspace sync status

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/agents/{id}/workspace` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `agent_record`; boundaries `project`, `hub`)

**Base Permission:** `agent.read`

**Resource Resolver:** agent-from-url

**Effects:** `read-one`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestBearerDispositionMatrix_CatalogEntryPoints`

---

## agent.workspace.sync

**Domain:** agent

**Description:** Sync an agent's workspace from or to the agent

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/agents/{id}/workspace/sync-from` |
| http_route | POST | `/api/v1/agents/{id}/workspace/sync-to` |
| http_route | POST | `/api/v1/agents/{id}/workspace/sync-to/finalize` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `agent_record`; boundaries `project`, `hub`)

**Base Permission:** `agent.update`

**Resource Resolver:** agent-from-url

**Effects:** `update-resource`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestBearerDispositionMatrix_CatalogEntryPoints`

---

## agent.logs.read

**Domain:** agent

**Description:** Read an agent's runtime logs and Cloud Logging entries, and stream its Cloud Logging entries. A stream opened by a token ends once the token stops validating or loses agent.read

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/agents/{id}/logs` |
| http_route | GET | `/api/v1/projects/{projectId}/agents/{id}/logs` |
| http_route | GET | `/api/v1/agents/{id}/cloud-logs` |
| http_route | GET | `/api/v1/projects/{projectId}/agents/{id}/cloud-logs` |
| sse | GET | `/api/v1/agents/{id}/cloud-logs/stream` |
| sse | GET | `/api/v1/projects/{projectId}/agents/{id}/cloud-logs/stream` |

**Principals:** `user`, `agent`

**Credentials:** `session_jwt`, `scoped_uat`, `agent_jwt`

**Bearer:** `admit` (target `agent_record`; boundaries `project`, `hub`)

**Base Permission:** `agent.read`

**Resource Resolver:** agent-from-url

**Effects:** `read-one`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestBearerDispositionMatrix_CatalogEntryPoints`

---

## agent.messages.read

**Domain:** agent

**Description:** Read and stream an agent's message history and message logs. agent.attach gives the full history; agent.read alone gives the messages the caller sent or received. A stream opened by a token ends once the token stops validating or loses the permission that admitted it

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/agents/{id}/messages` |
| http_route | GET | `/api/v1/agents/{id}/message-logs` |
| http_route | GET | `/api/v1/projects/{projectId}/agents/{id}/message-logs` |
| sse | GET | `/api/v1/agents/{id}/messages/stream` |
| sse | GET | `/api/v1/agents/{id}/message-logs/stream` |
| sse | GET | `/api/v1/projects/{projectId}/agents/{id}/message-logs/stream` |

**Principals:** `user`, `agent`

**Credentials:** `session_jwt`, `scoped_uat`, `agent_jwt`

**Bearer:** `admit` (target `agent_record`; boundaries `project`, `hub`)

**Base Permission:** `agent.read`

**Resource Resolver:** agent-from-url

**Effects:** `read-one`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestBearerDispositionMatrix_CatalogEntryPoints`

---

## agent.metrics.read

**Domain:** agent

**Description:** Read an agent's aggregate session metrics

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/agents/{id}/metrics/summary` |

**Principals:** `user`, `agent`

**Credentials:** `session_jwt`, `scoped_uat`, `agent_jwt`

**Bearer:** `admit` (target `agent_record`; boundaries `project`, `hub`)

**Base Permission:** `agent.read`

**Resource Resolver:** agent-from-url

**Effects:** `read-one`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestBearerDispositionMatrix_CatalogEntryPoints`

---

## agent.lifecycle.keys

**Domain:** agent

**Description:** Send keystrokes to an agent's session

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/agents/{id}/keys` |
| http_route | POST | `/api/v1/projects/{projectId}/agents/{id}/keys` |

**Principals:** `user`, `agent`

**Credentials:** `session_jwt`, `scoped_uat`, `agent_jwt`

**Bearer:** `admit` (target `agent_record`; boundaries `project`, `hub`)

**Base Permission:** `agent.attach`

**Resource Resolver:** agent-from-url

**Effects:** `update-resource`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestBearerDispositionMatrix_CatalogEntryPoints`

---

## agent.action.message

**Domain:** agent

**Description:** Send a message to an agent through its message action

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/agents/{id}/message` |
| http_route | POST | `/api/v1/projects/{projectId}/agents/{id}/message` |

**Principals:** `user`, `agent`

**Credentials:** `session_jwt`, `scoped_uat`, `agent_jwt`

**Bearer:** `admit` (target `agent_record`; boundaries `project`, `hub`)

**Base Permission:** `agent.message`

**Resource Resolver:** agent-from-url

**Effects:** `emit-external-effect`

### Audit

- **Event Type:** `agent.message.send`
- **Context Fields:** actor_id, project_id
- **After Fields:** message_id, target_agent_id
- **Atomic:** No
- **Non-Atomic Justification:** Message dispatch is fire-and-forget; audit recorded before dispatch

### External Effect Policy

- **Delivery:** `fire_and_forget`
- **Failure Mode:** `log_and_continue`
- **Idempotency:** message ID
- **Retry:** no retry for user-sent messages
- **Auth Before Emit:** Yes

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestBearerDispositionMatrix_CatalogEntryPoints`

---

## agent.attach

**Domain:** agent

**Description:** Attach to an agent session via WebSocket. A session opened by a token closes with 4401 once the token stops validating or loses agent.attach

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| websocket | GET | `/api/v1/agents/{id}/pty` |

**Principals:** `user`, `agent`

**Credentials:** `session_jwt`, `scoped_uat`, `agent_jwt`

**Bearer:** `admit` (target `agent_record`; boundaries `project`, `hub`)

**Base Permission:** `agent.attach`

**Resource Resolver:** agent-from-url

**Effects:** `read-one`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`

---

## agent.portaccess

**Domain:** agent

**Description:** Access forwarded ports on an agent

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/agents/{id}/ports/{port}/proxy` |
| http_route | POST | `/api/v1/agents/{id}/ports/{port}/proxy` |
| http_route | PUT | `/api/v1/agents/{id}/ports/{port}/proxy` |
| http_route | DELETE | `/api/v1/agents/{id}/ports/{port}/proxy` |
| http_route | GET | `/api/v1/agents/{id}/ports/{port}/proxy/{subpath}` |

**Principals:** `user`, `agent`

**Credentials:** `session_jwt`, `scoped_uat`, `agent_jwt`

**Bearer:** `admit` (target `agent_record`; boundaries `project`, `hub`)

**Base Permission:** `agent.port_access`

**Resource Resolver:** agent-from-url

**Effects:** `read-one`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`

---

## agent.port.list

**Domain:** agent

**Description:** List an agent's exposed ports

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/agents/{id}/ports` |

**Principals:** `user`, `agent`

**Credentials:** `session_jwt`, `scoped_uat`, `agent_jwt`

**Bearer:** `admit` (target `agent_record`; boundaries `project`, `hub`)

**Base Permission:** `agent.read`

**Resource Resolver:** agent-from-url

**Effects:** `read-one`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestBearerDispositionMatrix_CatalogEntryPoints`

---

## agent.port.register

**Domain:** agent

**Description:** Register, delete or clear an agent's exposed ports

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/agents/{id}/ports` |
| http_route | DELETE | `/api/v1/agents/{id}/ports/{port}` |
| http_route | DELETE | `/api/v1/agents/{id}/ports` |

**Principals:** `user`, `agent`

**Credentials:** `session_jwt`, `scoped_uat`, `agent_jwt`

**Bearer:** `admit` (target `agent_record`; boundaries `project`, `hub`)

**Base Permission:** `agent.port_access`

**Resource Resolver:** agent-from-url

**Effects:** `update-resource`

**Denial Codes:** `forbidden`, `conflict`

### Tests

- `pkg/hub:TestPortRegistration_UsesAgentRecordTarget`
- `pkg/hub:TestPortManageToken_RequiresSelectorBoundaryAndHubLevelHolder`

---

## agent.port.tunnel

**Domain:** agent

**Description:** Open the calling agent's own port tunnel

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| websocket | GET | `/api/v1/agents/{id}/ports/tunnel` |

**Principals:** `agent`

**Credentials:** `agent_jwt`

**Bearer:** `non_user` (pinned by `TestPortTunnel_OnlyTheAgentItself`)

**Resource Resolver:** agent-self

**Effects:** `update-resource`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestPortTunnel_OnlyTheAgentItself`

### Exemptions

- **internal_only:** Only the agent itself opens its port tunnel, with its own agent JWT and the port-forward scope; every user is refused (scope: agent self access) — waives: `base_permission`

---

## agent.setmessagemode

**Domain:** agent

**Description:** Change an agent's message mode

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/agents/{id}/set_message_mode` |
| http_route | POST | `/api/v1/projects/{projectId}/agents/{id}/set_message_mode` |

**Principals:** `user`, `agent`

**Credentials:** `session_jwt`, `scoped_uat`, `agent_jwt`

**Bearer:** `admit` (target `agent_record`; boundaries `project`, `hub`)

**Base Permission:** `agent.set_message_mode`

**Resource Resolver:** agent-from-url

**Effects:** `update-resource`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`
- `pkg/hub:TestSetMessageModeToken_ProjectAdminDenialStillApplies`

---

## agent.token.refresh

**Domain:** agent

**Description:** Refresh the calling agent's own hub token

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/agents/{id}/token/refresh` |
| http_route | POST | `/api/v1/agents/{id}/refresh-token` |

**Principals:** `agent`

**Credentials:** `agent_jwt`

**Bearer:** `non_user`

**Resource Resolver:** agent-self

**Effects:** `update-resource`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestBearerDisposition_EveryRoutePatternCovered`

### Exemptions

- **internal_only:** The agent authenticates with its own agent JWT for its own record; no user permission applies (scope: agent self access) — waives: `base_permission`

---

## agent.outbound.message

**Domain:** agent

**Description:** Deliver an outbound message from the calling agent

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/agents/{id}/outbound-message` |
| http_route | POST | `/api/v1/projects/{projectId}/agents/{id}/outbound-message` |

**Principals:** `agent`

**Credentials:** `agent_jwt`

**Bearer:** `non_user`

**Resource Resolver:** agent-self

**Effects:** `create-resource`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestBearerDisposition_EveryRoutePatternCovered`

### Exemptions

- **internal_only:** The agent authenticates with its own agent JWT for its own record; no user permission applies (scope: agent self access) — waives: `base_permission`

---

## agent.metrics.report

**Domain:** agent

**Description:** Report runtime metrics for the calling agent

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/agents/{id}/metrics` |

**Principals:** `agent`

**Credentials:** `agent_jwt`

**Bearer:** `non_user`

**Resource Resolver:** agent-self

**Effects:** `update-resource`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestBearerDisposition_EveryRoutePatternCovered`

### Exemptions

- **internal_only:** The agent authenticates with its own agent JWT for its own record; no user permission applies (scope: agent self access) — waives: `base_permission`

---

## agent.secrets.access

**Domain:** agent

**Description:** List, read and write the secrets available to the calling agent: its project's and its creating user's

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/agents/{id}/secrets` |
| http_route | GET | `/api/v1/agents/{id}/secrets/{key}` |
| http_route | PUT | `/api/v1/agents/{id}/secrets/{key}` |

**Principals:** `agent`

**Credentials:** `agent_jwt`

**Bearer:** `non_user`

**Resource Resolver:** agent-self

**Effects:** `read-secret`, `update-resource`

### Audit

- **Event Type:** `agent.secrets.access`
- **Context Fields:** actor_id, project_id, scope
- **Before Fields:** secret_key
- **Atomic:** Yes

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestBearerDisposition_EveryRoutePatternCovered`

### Exemptions

- **internal_only:** The agent authenticates with its own agent JWT, whose subject must match the agent ID in the path; no user permission applies; the project and user scope IDs come from the token (scope: agent self access) — waives: `base_permission`

---

## agent.stopall

**Domain:** agent

**Description:** Stop all running agents on the hub

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/agents/stop-all` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `hub_instance`; boundaries `hub`)

**Base Permission:** `agent.stop_all`

**Resource Resolver:** hub-scoped

**Effects:** `update-resource`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`
- `pkg/hub:TestStopAllHubForm_RequiresStopAllSelectorAndHubBoundary`

---

## agent.stopall.project

**Domain:** agent

**Description:** Stop all running agents in a project

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/projects/{projectId}/agents/stop-all` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `project_path`; boundaries `project`, `hub`)

**Base Permission:** `agent.stop_all`

**Resource Resolver:** project-from-url

**Effects:** `update-resource`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestStopAllHubForm_RequiresStopAllSelectorAndHubBoundary`

---

## project.membership.add

**Domain:** project.membership

**Description:** Add a member to a project with a specified role

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/projects/{id}/members` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `project_path`; boundaries `project`, `hub`; pinned by `TestMembershipToken_CannotGrantRoleAboveCaller`)

**Base Permission:** `project.manage`

**Resource Resolver:** project-from-url

**Effects:** `grant-authority`

### Delegation

- **Kind:** `non_amplification`
- Actor must hold all permissions in the target role (CanDelegate non-amplification)

### Governance

- **Kind:** peer_superior
- RS1 governance: CT1 D5 typed governance matrix — owners manage all roles, admins manage members only. Enforced by ProjectMembershipService.checkGovernance.

### Invariants

| ID | Kind | Description | Fail-Closed |
|----|------|-------------|-------------|
| direct-user-only-owner | security | project-owner role is direct-user-only | Yes |
| single-binding-per-principal | business | CT1 D4: one direct binding per principal per project | No |

### Audit

- **Event Type:** `project_member_add`
- **Context Fields:** actor_id, project_id
- **After Fields:** target_principal_id, target_role
- **Atomic:** Yes

**Denial Codes:** `forbidden`, `role_assignment_forbidden`, `target_role_protected`, `principal_ineligible`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`

---

## project.membership.update

**Domain:** project.membership

**Description:** Change a project member's role

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | PATCH | `/api/v1/projects/{id}/members/{memberId}` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `project_path`; boundaries `project`, `hub`; pinned by `TestMembershipToken_GovernanceMatrixApplies`)

**Base Permission:** `project.manage`

**Resource Resolver:** project-from-url

**Effects:** `change-authority`

### Delegation

- **Kind:** `conditional_on_increase`
- CanDelegate checked when new role has more permissions than old role

**Authority Evaluation:** `before_and_after`

### Governance

- **Kind:** peer_superior
- RS1 governance: CT1 D5 typed governance matrix — owners manage all roles, admins manage members only. Both old and new target roles are governed. Enforced by ProjectMembershipService.checkGovernance.

### Invariants

| ID | Kind | Description | Fail-Closed |
|----|------|-------------|-------------|
| direct-user-only-owner | security | project-owner role is direct-user-only | Yes |
| last-owner-guard | security | Cannot demote the last active direct owner | Yes |

### Audit

- **Event Type:** `project_member_role_change`
- **Context Fields:** actor_id, project_id
- **Before Fields:** target_principal_id, old_role
- **After Fields:** new_role
- **Atomic:** Yes

**Denial Codes:** `forbidden`, `role_assignment_forbidden`, `target_role_protected`, `last_owner`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`

---

## project.membership.remove

**Domain:** project.membership

**Description:** Remove a member from a project

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | DELETE | `/api/v1/projects/{id}/members/{memberId}` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `project_path`; boundaries `project`, `hub`; pinned by `TestMembershipToken_GovernanceMatrixApplies`)

**Base Permission:** `project.manage`

**Resource Resolver:** project-from-url

**Effects:** `revoke-authority`

**Authority Evaluation:** `proposed_post_state`

### Governance

- **Kind:** peer_superior
- RS1 governance: CT1 D5 typed governance matrix — owners manage all roles, admins manage members only. CT1 D1 allows self-removal when another active direct owner remains. Enforced by ProjectMembershipService.checkGovernance.

### Invariants

| ID | Kind | Description | Fail-Closed |
|----|------|-------------|-------------|
| last-owner-guard | security | Cannot remove the last active direct owner | Yes |

### Audit

- **Event Type:** `project_member_remove`
- **Context Fields:** actor_id, project_id
- **Before Fields:** target_principal_id, target_role
- **Atomic:** Yes

**Denial Codes:** `forbidden`, `role_assignment_forbidden`, `target_role_protected`, `last_owner`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`

---

## project.membership.list

**Domain:** project.membership

**Description:** List project members and their roles

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/projects/{id}/members` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `project_path`; boundaries `project`, `hub`)

**Base Permission:** `project.read`

**Resource Resolver:** project-from-url

**Effects:** `list-scoped`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`

---

## project.membership.transfer

**Domain:** project.membership

**Description:** Atomically transfer project ownership from the actor to another user

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/projects/{id}/transfer-ownership` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `project_path`; boundaries `project`, `hub`; pinned by `TestTransferOwnershipToken_RequiresDirectOwner`)

**Base Permission:** `project.manage`

**Resource Resolver:** project-from-url

**Effects:** `change-authority`

### Delegation

- **Kind:** `conditional_on_increase`
- Actor must be a direct project owner; target is promoted to owner, actor is downgraded to member — conditional-on-increase applies to the target's authority change

**Authority Evaluation:** `before_and_after`

### Governance

- **Kind:** peer_superior
- RS1 governance: only active direct project owners may transfer ownership. Actor-must-be-direct-owner is enforced by the ProjectMembershipService.

### Invariants

| ID | Kind | Description | Fail-Closed |
|----|------|-------------|-------------|
| direct-user-only-owner | security | project-owner role is direct-user-only | Yes |
| last-owner-guard | security | Post-state: at least one active direct owner must remain | Yes |
| single-binding-per-principal | business | CT1 D4: one direct binding per principal per project; atomic replacement for both actor and target | No |

### Audit

- **Event Type:** `project_ownership_transfer`
- **Context Fields:** actor_id, project_id
- **Before Fields:** old_owner_id
- **After Fields:** new_owner_id, old_owner_role, new_owner_role
- **Atomic:** Yes

**Denial Codes:** `forbidden`, `role_assignment_forbidden`, `principal_ineligible`, `last_owner`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`

---

## project.membership.principal.set

**Domain:** project.membership

**Description:** Replace a principal's whole set of project roles

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | PUT | `/api/v1/projects/{id}/members/principals/{type}/{principalId}` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `project_path`; boundaries `project`, `hub`; pinned by `TestMembershipToken_CannotGrantRoleAboveCaller`)

**Base Permission:** `project.manage`

**Resource Resolver:** project-from-url

**Effects:** `grant-authority`, `change-authority`, `revoke-authority`

### Delegation

- **Kind:** `conditional_on_increase`
- CanDelegate checked for every created binding; a built-in role change is checked only when the new role is higher

**Authority Evaluation:** `before_and_after`

### Governance

- **Kind:** peer_superior
- Built-in roles follow the governance matrix (owners manage all roles, admins manage members only, direct owners for admin and owner targets); custom roles need custom-role authority (direct owner). Enforced by ProjectMembershipService.SetMemberRoles.

### Invariants

| ID | Kind | Description | Fail-Closed |
|----|------|-------------|-------------|
| direct-user-only-owner | security | project-owner role is direct-user-only | Yes |
| single-binding-per-principal | business | CT1 D4: one built-in binding per principal per project | No |
| last-owner-guard | security | Cannot remove or demote the last active direct owner | Yes |

### Audit

- **Event Type:** `project_member_role_change`
- **Context Fields:** actor_id, project_id
- **Before Fields:** target_principal_id, old_role
- **After Fields:** new_role
- **Atomic:** Yes

**Denial Codes:** `forbidden`, `role_assignment_forbidden`, `target_role_protected`, `principal_ineligible`, `last_owner`, `credential_insufficient`

### Tests

- `pkg/hub:TestMembershipToken_SelectorAndBoundaryPerRoute`

---

## project.membership.principal.remove

**Domain:** project.membership

**Description:** Remove every project role a principal holds

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | DELETE | `/api/v1/projects/{id}/members/principals/{type}/{principalId}` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `project_path`; boundaries `project`, `hub`; pinned by `TestMembershipToken_LastOwnerProtected`)

**Base Permission:** `project.manage`

**Resource Resolver:** project-from-url

**Effects:** `revoke-authority`

**Authority Evaluation:** `proposed_post_state`

### Governance

- **Kind:** peer_superior
- Built-in roles follow the governance matrix; custom roles need custom-role authority (direct owner). Enforced by ProjectMembershipService.SetMemberRoles.

### Invariants

| ID | Kind | Description | Fail-Closed |
|----|------|-------------|-------------|
| last-owner-guard | security | Cannot remove the last active direct owner | Yes |

### Audit

- **Event Type:** `project_member_remove`
- **Context Fields:** actor_id, project_id
- **Before Fields:** target_principal_id, target_role
- **Atomic:** Yes

**Denial Codes:** `forbidden`, `role_assignment_forbidden`, `target_role_protected`, `last_owner`, `credential_insufficient`

### Tests

- `pkg/hub:TestMembershipToken_SelectorAndBoundaryPerRoute`

---

## project.membership.rolebinding.create

**Domain:** project.membership

**Description:** Create a project-scoped binding of a built-in project role through the role-binding API

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/admin/role-bindings` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `project_body`; boundaries `project`, `hub`; pinned by `TestMembershipToken_CannotGrantRoleAboveCaller`)

**Base Permission:** `project.manage`

**Resource Resolver:** project-from-body

**Effects:** `grant-authority`

### Delegation

- **Kind:** `non_amplification`
- Actor must hold all permissions in the target role (CanDelegate non-amplification)

### Governance

- **Kind:** peer_superior
- Routed to ProjectMembershipService.AddMember (create-only): the governance matrix, direct-owner rule and one-binding invariant apply. The project-scope branch runs before any hub-level check.

### Invariants

| ID | Kind | Description | Fail-Closed |
|----|------|-------------|-------------|
| direct-user-only-owner | security | project-owner role is direct-user-only | Yes |
| single-binding-per-principal | business | CT1 D4: one direct binding per principal per project | No |

### Audit

- **Event Type:** `project_member_add`
- **Context Fields:** actor_id, project_id
- **After Fields:** target_principal_id, target_role
- **Atomic:** Yes

**Denial Codes:** `forbidden`, `role_assignment_forbidden`, `target_role_protected`, `principal_ineligible`, `conflict`

### Tests

- `pkg/hub:TestMembershipToken_SelectorAndBoundaryPerRoute`

---

## project.membership.rolebinding.delete

**Domain:** project.membership

**Description:** Delete a project-scoped role binding through the role-binding API

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | DELETE | `/api/v1/admin/role-bindings/{id}` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `role_binding_record`; boundaries `project`, `hub`; pinned by `TestMembershipToken_GovernanceMatrixApplies`)

**Base Permission:** `project.manage`

**Resource Resolver:** project-from-binding

**Effects:** `revoke-authority`

**Authority Evaluation:** `proposed_post_state`

### Governance

- **Kind:** peer_superior
- Project-scoped bindings are removed under project governance: a caller with project.manage on the binding's project goes to ProjectMembershipService.RemoveMember (governance matrix, direct-owner rule, last-owner guard) before any hub-level check; any other caller needs role_binding.delete at hub scope.

### Invariants

| ID | Kind | Description | Fail-Closed |
|----|------|-------------|-------------|
| last-owner-guard | security | Cannot remove the last active direct owner | Yes |

### Audit

- **Event Type:** `project_member_remove`
- **Context Fields:** actor_id, project_id
- **Before Fields:** target_principal_id, target_role
- **Atomic:** Yes

**Denial Codes:** `forbidden`, `role_assignment_forbidden`, `target_role_protected`, `last_owner`

### Tests

- `pkg/hub:TestRoleBindingDelete_ProjectBindingUnderProjectGovernance`

---

## project.visitors.list

**Domain:** project.visitors

**Description:** List a project's visitor grants (federated accounts holding the project-visitor role)

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/projects/{id}/visitors` |

**Principals:** `user`

**Credentials:** `session_jwt`

**Bearer:** `session_only` (reason `GOV_PENDING`)

**Base Permission:** `project.manage_visitors`

**Resource Resolver:** project-from-url

**Effects:** `list-scoped`

**Denial Codes:** `forbidden`, `credential_insufficient`

### Tests

- `pkg/hub:TestProjectVisitors_AddListRemove`

---

## project.visitors.add

**Domain:** project.visitors

**Description:** Grant a user of a trusted issuer visitor access to a project by email; creates the federated account in status pending when none exists

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/projects/{id}/visitors` |

**Principals:** `user`

**Credentials:** `session_jwt`

**Bearer:** `session_only` (reason `GOV_PENDING`)

**Base Permission:** `project.manage_visitors`

**Resource Resolver:** project-from-url

**Effects:** `grant-authority`

### Delegation

- **Kind:** `non_amplification`
- The actor must hold every permission of the project-visitor role on the project (an unscoped local platform admin passes)

### Governance

- **Kind:** peer_superior
- Holders of project.manage_visitors (project owners and admins, and hub admins) grant visitor access; granting also needs every permission of the project-visitor role on the project. A visitor holds no management authority. The issuer must be trusted with allow_visitor_grants and the email must pass its allowed_emails and allowed_domains.

### Invariants

| ID | Kind | Description | Fail-Closed |
|----|------|-------------|-------------|
| visitor-role-only | security | The grant binds only the project-visitor role, at project scope, to a federated account (CreateVisitorRoleBinding) | Yes |

### Audit

- **Event Type:** `project_visitor_grant`
- **Context Fields:** actor_id, project_id
- **After Fields:** visitor_id, issuer, email, not_before, expires_at
- **Atomic:** Yes

**Denial Codes:** `forbidden`, `credential_insufficient`, `principal_ineligible`, `conflict`

### Tests

- `pkg/hub:TestProjectVisitors_AddListRemove`
- `pkg/hub:TestProjectVisitors_GrantNeedsEveryVisitorPermission`
- `pkg/hub:TestProjectVisitors_AuditFailureLeavesNoChange`

---

## project.visitors.update

**Domain:** project.visitors

**Description:** Replace the window (not before, expires at) of a project visitor grant

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | PATCH | `/api/v1/projects/{id}/visitors/{visitorId}` |

**Principals:** `user`

**Credentials:** `session_jwt`

**Bearer:** `session_only` (reason `GOV_PENDING`)

**Base Permission:** `project.manage_visitors`

**Resource Resolver:** project-from-url

**Effects:** `change-authority`

### Delegation

- **Kind:** `conditional_on_increase`
- Only a window change that extends the grant (an earlier or cleared start, or a later or cleared expiry) needs every permission of the project-visitor role on the project, as for a grant; shortening never does

**Authority Evaluation:** `before_and_after`

### Governance

- **Kind:** peer_superior
- Holders of project.manage_visitors (project owners and admins, and hub admins) shorten any visitor grant of the project; extending one also needs every permission of the project-visitor role on the project.

### Audit

- **Event Type:** `project_visitor_update`
- **Context Fields:** actor_id, project_id
- **Before Fields:** visitor_id, not_before, expires_at
- **After Fields:** not_before, expires_at
- **Atomic:** Yes

**Denial Codes:** `forbidden`, `credential_insufficient`, `not_found`

### Tests

- `pkg/hub:TestProjectVisitors_WindowHonoured`
- `pkg/hub:TestProjectVisitors_GrantNeedsEveryVisitorPermission`
- `pkg/hub:TestProjectVisitors_WhoMayGrant`

---

## project.visitors.remove

**Domain:** project.visitors

**Description:** Revoke a project visitor grant; the visitor's next request is refused. Revoking a binding a visitor rule created also excludes the visitor from that rule, so the rule does not bind them again

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | DELETE | `/api/v1/projects/{id}/visitors/{visitorId}` |

**Principals:** `user`

**Credentials:** `session_jwt`

**Bearer:** `session_only` (reason `GOV_PENDING`)

**Base Permission:** `project.manage_visitors`

**Resource Resolver:** project-from-url

**Effects:** `revoke-authority`

### Governance

- **Kind:** peer_superior
- Holders of project.manage_visitors (project owners and admins, and hub admins) revoke any visitor grant of the project.

### Audit

- **Event Type:** `project_visitor_revoke`
- **Context Fields:** actor_id, project_id
- **Before Fields:** visitor_id, issuer, email, subject
- **Atomic:** Yes

**Denial Codes:** `forbidden`, `credential_insufficient`, `not_found`

### Tests

- `pkg/hub:TestProjectVisitors_RevokeRefusesNextRequest`
- `pkg/hub:TestProjectVisitors_WhoMayGrant`
- `pkg/hub:TestProjectVisitorRules_RevokeOneVisitorExcludes`

---

## project.visitors.rules.list

**Domain:** project.visitors

**Description:** List a project's visitor rules (visitor access by issuer and email domain)

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/projects/{id}/visitors/rules` |

**Principals:** `user`

**Credentials:** `session_jwt`

**Bearer:** `session_only` (reason `GOV_PENDING`)

**Base Permission:** `project.manage_visitors`

**Resource Resolver:** project-from-url

**Effects:** `list-scoped`

**Denial Codes:** `forbidden`, `credential_insufficient`

### Tests

- `pkg/hub:TestProjectVisitorRules_AddListRemove`

---

## project.visitors.rules.add

**Domain:** project.visitors

**Description:** Add a visitor rule: visitor access to a project for every user of a trusted issuer whose verified email is in a domain, bound per user on the user's first matching request

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/projects/{id}/visitors/rules` |

**Principals:** `user`

**Credentials:** `session_jwt`

**Bearer:** `session_only` (reason `GOV_PENDING`)

**Base Permission:** `project.manage_visitors`

**Resource Resolver:** project-from-url

**Effects:** `grant-authority`

### Delegation

- **Kind:** `non_amplification`
- The actor must hold every permission of the project-visitor role on the project (an unscoped local platform admin passes), as for a grant by email

### Governance

- **Kind:** peer_superior
- Holders of project.manage_visitors (project owners and admins, and hub admins) add visitor rules; adding one also needs every permission of the project-visitor role on the project. The issuer must be trusted with allow_visitor_grants, and its allowed_emails and allowed_domains must admit the whole domain. The domain matches exactly (case-insensitive), with no wildcard or subdomain match.

### Invariants

| ID | Kind | Description | Fail-Closed |
|----|------|-------------|-------------|
| visitor-rule-bindings-only | security | A rule binds only the project-visitor role, at the rule's project, to a federated account of the rule's issuer, tagged with the rule's ID and carrying its window (CreateVisitorRoleBinding) | Yes |

### Audit

- **Event Type:** `project_visitor_rule_add`
- **Context Fields:** actor_id, project_id
- **After Fields:** rule_id, issuer, domain, not_before, expires_at
- **Atomic:** Yes

**Denial Codes:** `forbidden`, `credential_insufficient`, `principal_ineligible`, `conflict`

### Tests

- `pkg/hub:TestProjectVisitorRules_AddListRemove`
- `pkg/hub:TestProjectVisitorRules_AddRefusals`
- `pkg/hub:TestProjectVisitorRules_AddNeedsEveryVisitorPermission`

---

## project.visitors.rules.update

**Domain:** project.visitors

**Description:** Replace the window (not before, expires at) of a project visitor rule and of every visitor binding it created

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | PATCH | `/api/v1/projects/{id}/visitors/rules/{ruleId}` |

**Principals:** `user`

**Credentials:** `session_jwt`

**Bearer:** `session_only` (reason `GOV_PENDING`)

**Base Permission:** `project.manage_visitors`

**Resource Resolver:** project-from-url

**Effects:** `change-authority`

### Delegation

- **Kind:** `conditional_on_increase`
- Only a window change that extends the rule (an earlier or cleared start, or a later or cleared expiry) needs every permission of the project-visitor role on the project; shortening never does

**Authority Evaluation:** `before_and_after`

### Governance

- **Kind:** peer_superior
- Holders of project.manage_visitors (project owners and admins, and hub admins) shorten any visitor rule of the project; extending one also needs every permission of the project-visitor role on the project. The rule's bindings take the new window in the same transaction.

### Audit

- **Event Type:** `project_visitor_rule_update`
- **Context Fields:** actor_id, project_id
- **Before Fields:** rule_id, not_before, expires_at
- **After Fields:** not_before, expires_at
- **Atomic:** Yes

**Denial Codes:** `forbidden`, `credential_insufficient`, `not_found`

### Tests

- `pkg/hub:TestProjectVisitorRules_WindowHonoured`
- `pkg/hub:TestProjectVisitorRules_AddNeedsEveryVisitorPermission`

---

## project.visitors.rules.remove

**Domain:** project.visitors

**Description:** Remove a project visitor rule; every visitor binding it created and its exclusions are deleted in the same transaction, and grants by email are unchanged

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | DELETE | `/api/v1/projects/{id}/visitors/rules/{ruleId}` |

**Principals:** `user`

**Credentials:** `session_jwt`

**Bearer:** `session_only` (reason `GOV_PENDING`)

**Base Permission:** `project.manage_visitors`

**Resource Resolver:** project-from-url

**Effects:** `revoke-authority`

### Governance

- **Kind:** peer_superior
- Holders of project.manage_visitors (project owners and admins, and hub admins) remove any visitor rule of the project.

### Audit

- **Event Type:** `project_visitor_rule_remove`
- **Context Fields:** actor_id, project_id
- **Before Fields:** rule_id, issuer, domain, removed_visitor_ids
- **Atomic:** Yes

**Denial Codes:** `forbidden`, `credential_insufficient`, `not_found`

### Tests

- `pkg/hub:TestProjectVisitorRules_RemoveDeletesOnlyItsBindings`
- `pkg/hub:TestProjectVisitorRules_AuditFailureLeavesNoChange`

---

## project.lifecycle.create

**Domain:** project

**Description:** Create a new project. project.create is checked as a collection-level create at the hub scope; a token needs project:create on a hub boundary. Naming an existing project by id needs project.update on it

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/projects` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `hub_collection`; boundaries `hub`; pinned by `TestProjectCreateToken_RequiresCreateSelectorAndHubBoundary`)

**Base Permission:** `project.create`

**Resource Resolver:** hub-scoped

**Effects:** `create-resource`, `grant-authority`

### Delegation

- **Kind:** `non_amplification`
- The creator is bound to the project-owner role on the project the call creates. A credential with a permission ceiling must cover every permission of that role before any write (projectOwnerGrantDenial); for other callers project.create gates the grant

### Audit

- **Event Type:** `project_member_add`
- **Context Fields:** actor_id, project_id
- **After Fields:** user_id, role
- **Atomic:** Yes

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`
- `pkg/hub:TestOwnerGrantCoverageCheck`
- `pkg/hub:TestOwnerBindingAuditFailureRollsBack`
- `pkg/hub:TestProjectCreateToken_RequiresCreateSelectorAndHubBoundary`
- `pkg/hub:TestProjectCreateToken_IdempotentPathRequiresUpdate`

---

## project.lifecycle.delete

**Domain:** project

**Description:** Delete a project with cascading security state cleanup and atomic audit

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | DELETE | `/api/v1/projects/{id}` |

**Principals:** `user`

**Credentials:** `session_jwt`

**Bearer:** `session_only` (reason `IRREVERSIBLE_CASCADE`)

**Base Permission:** `project.delete`

**Resource Resolver:** project-from-url

**Effects:** `delete-resource`, `emit-external-effect`

### Governance

- **Kind:** ownership_ancestry
- RS3 governance: direct project owner or super-admin. Hub-admin lacks project.delete and is denied at base permission. Group-derived ownership does not confer deletion authority. Stale Project.OwnerID is not consulted. Enforced by ProjectDeletionService.checkDeletionGovernance.

### Invariants

| ID | Kind | Description | Fail-Closed |
|----|------|-------------|-------------|
| target-exists | business | Project must exist and not be already deleted | Yes |
| agents-runtime-deleted | business | Unless force=true, refused with 409 conflict while the project has an agent that is not soft-deleted (runtime deletion not confirmed); checked under the agent row lock, after authorization | Yes |

### Audit

- **Event Type:** `project.lifecycle.delete`
- **Context Fields:** actor_id
- **Before Fields:** project_id, project_name, project_slug, owner_id
- **After Fields:** cascade_summary
- **Atomic:** Yes

### External Effect Policy

- **Delivery:** `fire_and_forget`
- **Failure Mode:** `log_and_continue`
- **Idempotency:** project ID (single deletion per project)
- **Retry:** no retry — cascading deletes are best-effort; DB cascade is authoritative
- **Auth Before Emit:** Yes

**Denial Codes:** `forbidden`, `user_suspended`, `credential_insufficient`, `not_found`, `conflict`

### Tests

- `pkg/hub:TestRS3_ProjectDeleteOwnerPositiveControl`
- `pkg/hub:TestProjectDelete_LiveAgentRefused`
- `pkg/hub:TestProjectDelete_GuardRunsAfterAuthorization`
- `pkg/hub:TestRS3_ProjectDeleteGovernanceMatrix`
- `pkg/hub:TestRS3_ProjectDeleteAtomicAudit`

---

## project.read

**Domain:** project

**Description:** Read a single project's metadata by ID or slug

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/projects/{id}` |

**Principals:** `user`, `agent`

**Credentials:** `session_jwt`, `scoped_uat`, `agent_jwt`

**Bearer:** `admit` (target `project_path`; boundaries `project`, `hub`)

**Base Permission:** `project.read`

**Resource Resolver:** project-from-url

**Effects:** `read-one`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`

---

## project.list

**Domain:** project

**Description:** List projects within the caller's authorized scope. A user access token needs project:list on either boundary: a project token lists only its boundary project

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/projects` |

**Principals:** `user`, `agent`

**Credentials:** `session_jwt`, `scoped_uat`, `agent_jwt`

**Bearer:** `admit` (target `list_scope`; boundaries `project`, `hub`)

**Base Permission:** `project.list`

**Resource Resolver:** list-scope-resolver

**Effects:** `list-scoped`

### Invariants

| ID | Kind | Description | Fail-Closed |
|----|------|-------------|-------------|
| scope-pushed-query | security | Rows, totalCount, and nextCursor come from the same SQL predicate that includes the authorization scope | Yes |
| cursor-scope-binding | security | Cursor binding includes endpoint, caller filters, authorization scope, and principal/credential context | Yes |
| no-broad-query-on-none | security | ScopeSetNone produces empty list without issuing any resource query | Yes |

**Denial Codes:** `forbidden`, `credential_insufficient`, `user_suspended`

### Tests

- `pkg/hub:TestRS2_ProjectListScopePushed`
- `pkg/hub:TestRS2_ProjectListMineSharedClassification`
- `pkg/hub:TestRS2_ProjectListCursorBinding`
- `pkg/hub:TestRS2_ProjectListMultiPageInterleaved`
- `pkg/hub:TestRS2_ProjectListInterleavedWithCallerFilter`
- `pkg/hub:TestRS2_FailureInjection_PrincipalGroupClosure`
- `pkg/hub:TestRS2_FailureInjection_StoreListCount`
- `pkg/hub:TestRS2_CursorReplayAfterGrantRemoval`
- `pkg/hub:TestRS2_CursorReplayAfterBindingExpiry`
- `pkg/hub:TestRS2_AllPlusConstraint_EndToEnd`
- `pkg/hub:TestRS2_MalformedConstraintExclusionHTTP`
- `pkg/hub:TestRS2_SystemAllSharedSemantics`
- `pkg/hub:TestRS2_GroupChangeCursorReplay`
- `pkg/hub:TestRS2_ConstraintChangeCursorReplay`
- `pkg/hub:TestRS2_SuspensionCursorReplay`
- `pkg/hub:TestRS2_CredentialChangeCursorReplay`
- `pkg/hub:TestRS2_TransferredOwnership`
- `pkg/hub:TestRS2_TransitiveGroupAccess`
- `pkg/hub:TestRS2_FilterCompositionMatrix`
- `pkg/hub:TestProjectListToken_RequiresProjectListSelector`

---

## project.update

**Domain:** project

**Description:** Update project settings and metadata

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | PATCH | `/api/v1/projects/{id}` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `project_path`; boundaries `project`, `hub`)

**Base Permission:** `project.update`

**Resource Resolver:** project-from-url

**Effects:** `update-resource`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`

---

## project.register

**Domain:** project

**Description:** Register a project from an external source. Register checks project.create as a collection-level create at the hub scope; a token needs project:create on a hub boundary. An existing project named by id or matched by slug needs project.update on it. The embedded-broker form (a broker in the request body) is session-only (CREDENTIAL_MANAGEMENT): a token or any other non-session credential is refused before any lookup or write

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/projects/register` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `hub_collection`; boundaries `hub`; pinned by `TestProjectRegisterToken_EnforcesProjectCreate`)

**Base Permission:** `project.create`

**Resource Resolver:** project-from-body

**Effects:** `create-resource`, `grant-authority`

### Delegation

- **Kind:** `non_amplification`
- When the call creates the project, the creator is bound to the project-owner role on it; registering an existing project binds no owner. A credential with a permission ceiling must cover every permission of that role before any write (projectOwnerGrantDenial); for other callers project.create gates the grant

### Audit

- **Event Type:** `project_member_add`
- **Context Fields:** actor_id, project_id
- **After Fields:** user_id, role
- **Atomic:** Yes

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`
- `pkg/hub:TestOwnerGrantCoverageCheck`
- `pkg/hub:TestOwnerBindingAuditFailureRollsBack`
- `pkg/hub:TestProjectRegisterToken_EnforcesProjectCreate`
- `pkg/hub:TestProjectRegisterToken_EmbeddedBrokerFormIsSessionOnly`

---

## project.clone

**Domain:** project

**Description:** Clone a project into a new project owned by the caller. The caller needs project.read on the source and project.create as a collection-level create at the hub scope; a token also needs the project:clone selector

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/projects/{id}/clone` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `hub_collection`; boundaries `hub`; pinned by `TestProjectCloneToken_RequiresSourceReadCloneAndCreate`)

**Base Permission:** `project.create`

**Resource Resolver:** project-from-url

**Effects:** `create-resource`, `grant-authority`

### Delegation

- **Kind:** `non_amplification`
- The caller is bound to the project-owner role on the clone. A credential with a permission ceiling must cover every permission of that role before any write (projectOwnerGrantDenial); for other callers project.create gates the grant

### Audit

- **Event Type:** `project_member_add`
- **Context Fields:** actor_id, project_id
- **After Fields:** user_id, role
- **Atomic:** Yes

**Denial Codes:** `forbidden`, `not_found`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`
- `pkg/hub:TestOwnerBindingRequiresCanDelegate`
- `pkg/hub:TestProjectCloneToken_RequiresSourceReadCloneAndCreate`

---

## schedule.event.list

**Domain:** schedule

**Description:** List scheduled events or recurring schedules in a project

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/projects/{projectId}/scheduled-events` |
| http_route | GET | `/api/v1/projects/{projectId}/schedules` |

**Principals:** `user`, `agent`

**Credentials:** `session_jwt`, `scoped_uat`, `agent_jwt`

**Bearer:** `admit` (target `project_path`; boundaries `project`, `hub`)

**Base Permission:** `scheduled_event.list`

**Resource Resolver:** project-from-url

**Effects:** `list-scoped`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestBearerDispositionMatrix_CatalogEntryPoints`

---

## schedule.event.read

**Domain:** schedule

**Description:** Read a scheduled event, a recurring schedule or a schedule's run history

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/projects/{projectId}/scheduled-events/{id}` |
| http_route | GET | `/api/v1/projects/{projectId}/schedules/{id}` |
| http_route | GET | `/api/v1/projects/{projectId}/schedules/{id}/history` |

**Principals:** `user`, `agent`

**Credentials:** `session_jwt`, `scoped_uat`, `agent_jwt`

**Bearer:** `admit` (target `project_path`; boundaries `project`, `hub`)

**Base Permission:** `scheduled_event.read`

**Resource Resolver:** project-from-url

**Effects:** `read-one`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestBearerDispositionMatrix_CatalogEntryPoints`

---

## schedule.event.create

**Domain:** schedule

**Description:** Create a scheduled event or recurring schedule of any event type. A user access token needs scheduled_event:create and passes the event type's target checks

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/projects/{projectId}/scheduled-events` |
| http_route | POST | `/api/v1/projects/{projectId}/schedules` |

**Principals:** `user`, `agent`

**Credentials:** `session_jwt`, `scoped_uat`, `agent_jwt`

**Bearer:** `admit` (target `project_path`; boundaries `project`, `hub`)

**Base Permission:** `scheduled_event.create`

**Resource Resolver:** project-from-url

**Effects:** `create-resource`, `grant-authority`

### Delegation

- **Kind:** `non_amplification`
- Only the dispatch_agent event type grants authority. Authoring any event type records the author's frozen effect ceiling (revisionAuthorityCeiling); a dispatch_agent event or schedule then creates an agent at fire time, with a delegation edge from the recorded principal, after CanDelegate for that principal. A message event or schedule grants no authority. Effects are listed per operation, not per event type, so grant-authority is listed for the whole operation

### Audit

- **Event Type:** `agent_delegation`
- **Context Fields:** actor_id
- **After Fields:** agent_id, can_delegate_result
- **Atomic:** No
- **Non-Atomic Justification:** The authoring write records no mutation audit record: it stores the initiator attribution and the frozen effect ceiling on the event or schedule row in the same insert. The agent_delegation record is written when a dispatch_agent event fires, in the agent-create transaction with the agent row and its delegation edge. A message event writes none

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestScheduleAuthoringToken_RequiresCreateSelector`

---

## schedule.event.update

**Domain:** schedule

**Description:** Update or resume a recurring schedule of any event type. A user access token needs scheduled_event:update and passes the event type's target checks

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | PATCH | `/api/v1/projects/{projectId}/schedules/{id}` |
| http_route | POST | `/api/v1/projects/{projectId}/schedules/{id}/resume` |

**Principals:** `user`, `agent`

**Credentials:** `session_jwt`, `scoped_uat`, `agent_jwt`

**Bearer:** `admit` (target `project_path`; boundaries `project`, `hub`)

**Base Permission:** `scheduled_event.update`

**Resource Resolver:** project-from-url

**Effects:** `update-resource`, `grant-authority`

### Delegation

- **Kind:** `non_amplification`
- Only the dispatch_agent event type grants authority. A resume, and an update that changes the event type, payload, cron expression or enables the schedule, records the caller's initiator attribution and frozen effect ceiling (revisionAuthorityCeiling) in place of the stored ones; an update that changes none of those fields records neither. An update can switch a schedule to dispatch_agent. A dispatch_agent schedule then creates an agent at fire time, with a delegation edge from the recorded principal, after CanDelegate for that principal. A message schedule grants no authority. Effects are listed per operation, not per event type, so grant-authority is listed for the whole operation

### Audit

- **Event Type:** `agent_delegation`
- **Context Fields:** actor_id
- **After Fields:** agent_id, can_delegate_result
- **Atomic:** No
- **Non-Atomic Justification:** The update or resume write records no mutation audit record: it stores the initiator attribution and the frozen effect ceiling on the schedule row in the same conditional update that bumps its authorization revision. The agent_delegation record is written when a dispatch_agent schedule fires, in the agent-create transaction with the agent row and its delegation edge. A message schedule writes none

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestScheduleResumeToken_RechecksTarget`

---

## schedule.event.pause

**Domain:** schedule

**Description:** Pause a recurring schedule. Pausing only stops future runs, so a token with scheduled_event:update is admitted

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/projects/{projectId}/schedules/{id}/pause` |

**Principals:** `user`, `agent`

**Credentials:** `session_jwt`, `scoped_uat`, `agent_jwt`

**Bearer:** `admit` (target `project_path`; boundaries `project`, `hub`)

**Base Permission:** `scheduled_event.update`

**Resource Resolver:** project-from-url

**Effects:** `update-resource`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestSchedulePause_AdmitsTokenWithUpdateSelector`

---

## schedule.event.delete

**Domain:** schedule

**Description:** Cancel a scheduled event or delete a recurring schedule

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | DELETE | `/api/v1/projects/{projectId}/scheduled-events/{id}` |
| http_route | DELETE | `/api/v1/projects/{projectId}/schedules/{id}` |

**Principals:** `user`, `agent`

**Credentials:** `session_jwt`, `scoped_uat`, `agent_jwt`

**Bearer:** `admit` (target `project_path`; boundaries `project`, `hub`)

**Base Permission:** `scheduled_event.delete`

**Resource Resolver:** project-from-url

**Effects:** `delete-resource`

### Audit

- **Event Type:** `schedule.event.delete`
- **Context Fields:** actor_id, project_id
- **Before Fields:** event_id
- **Atomic:** Yes

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestBearerDispositionMatrix_CatalogEntryPoints`

---

## schedule.event.fire

**Domain:** schedule

**Description:** Fire a scheduled message or dispatch_agent event, one-shot or materialized from a recurring schedule, under the authority its revision recorded

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| scheduler_job | — | `executeSchedule:dispatch_agent` |
| scheduler_job | — | `executeSchedule:message` |
| scheduler_job | — | `fireEvent:dispatch_agent` |
| scheduler_job | — | `fireEvent:message` |

**Principals:** `system`

**Credentials:** `system_internal`

**Bearer:** `non_user` (pinned by `TestScheduledFire_UsesRecordedTokenCeiling`)

**Resource Resolver:** event-project

**Effects:** `create-resource`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestScheduledFire_UsesRecordedTokenCeiling`
- `pkg/hub:TestScheduledFire_UsesRecordedTokenBoundary`

### Exemptions

- **internal_only:** The scheduler fires the event with no request credential. resolveScheduledAuthority resolves the revision's recorded principal and credential, and the fire is bounded by the revision's recorded ceiling and boundary (scope: scheduler fire) — waives: `base_permission`

---

## artifact.read

**Domain:** artifact

**Description:** Read an artifact's metadata or file bytes (owner, home-project readers via the scope grant, or principal grants); unreadable artifacts answer 404

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/artifacts/{id}` |
| http_route | GET | `/api/v1/artifacts/{id}/files/{path}` |
| http_route | GET | `/api/v1/artifacts/{id}/versions/{seq}/files/{path}` |

**Principals:** `user`, `agent`

**Credentials:** `session_jwt`, `scoped_uat`, `agent_jwt`

**Bearer:** `admit` (target `artifact_record`; boundaries `project`, `hub`; pinned by `TestArtifactsUserAccessTokensAreBounded`)

**Base Permission:** `artifact.read`

**Resource Resolver:** artifact-home-project

**Effects:** `read-one`

**Denial Codes:** `not_found`

### Tests

- `pkg/hub:TestArtifactsTwoAgentsSameProject`

---

## artifact.list

**Domain:** artifact

**Description:** List the artifacts the caller owns, holds a grant on, or that are shared to a project it is a member of (?mine=1); each row passes the artifact.read check, so an artifact the caller cannot read is omitted, never denied

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/artifacts` |

**Principals:** `user`, `agent`

**Credentials:** `session_jwt`, `scoped_uat`, `agent_jwt`

**Bearer:** `admit` (target `artifact_record`; boundaries `project`, `hub`; pinned by `TestArtifactsListUserAccessTokensAreBounded`)

**Base Permission:** `artifact.read`

**Resource Resolver:** artifact-home-project

**Effects:** `list-scoped`

**Denial Codes:** `not_found`

### Tests

- `pkg/hub:TestArtifactsListMine`

---

## artifact.create

**Domain:** artifact

**Description:** Create an artifact homed in a project (the caller's own, or ?scope=): a single file published at once (?name=), or a manifest that starts a pending first version (JSON body), whose files are then uploaded and finalized through the artifact.version.upload and artifact.version.finalize routes

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/artifacts` |

**Principals:** `user`, `agent`

**Credentials:** `session_jwt`, `scoped_uat`, `agent_jwt`

**Bearer:** `admit` (target `project_query`; boundaries `project`, `hub`; pinned by `TestArtifactsUserAccessTokensAreBounded`)

**Base Permission:** `artifact.create`

**Resource Resolver:** project-from-query

**Effects:** `create-resource`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestArtifactsTwoAgentsSameProject`

---

## artifact.update

**Domain:** artifact

**Description:** Set or clear an artifact's expiry, or move it to another home project (PATCH); a move also requires artifact.create in the target project and cross-project sharing on; it removes the old home project's grant, so that project's members lose read unless another grant covers them, and gives the target project a read grant unless it already holds a scope grant (an admin one is refused with 409 home_admin_grant); a steward may change the expiry but a move from it is refused with 403 steward_action_not_allowed

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | PATCH | `/api/v1/artifacts/{id}` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `artifact_record`; boundaries `project`, `hub`; pinned by `TestArtifactsManageAndVersionRoutesUserAccessTokensAreBounded`)

**Base Permission:** `artifact.manage`

**Resource Resolver:** artifact-home-project

**Effects:** `update-resource`

### Audit

- **Event Type:** `artifact_event (expiry_set, expiry_cleared, moved)`
- **Context Fields:** artifact_id, actor_kind, actor_ref, authority
- **Before Fields:** oldExpiresAt, fromScope, droppedGrantId
- **After Fields:** newExpiresAt, toScope
- **Atomic:** Yes

**Denial Codes:** `not_found`, `forbidden`, `cross_project_sharing_disabled`, `steward_action_not_allowed`

### Tests

- `pkg/hub:TestArtifactsGrantsOnRoutes`
- `pkg/hub:TestArtifactStewardOnRoutes`
- `pkg/artifacts:TestPatchExpiry`
- `pkg/artifacts:TestPatchRehome`
- `pkg/artifacts:TestStewardBoundedTable`
- `pkg/artifacts:TestStewardRaceAnswersNotFound`

---

## artifact.grant.list

**Domain:** artifact

**Description:** List an artifact's principal and scope grants, home grant first, with whether cross-project sharing is on

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/artifacts/{id}/grants` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `artifact_record`; boundaries `project`, `hub`; pinned by `TestArtifactsManageAndVersionRoutesUserAccessTokensAreBounded`)

**Base Permission:** `artifact.manage`

**Resource Resolver:** artifact-home-project

**Effects:** `list-scoped`

**Denial Codes:** `not_found`, `forbidden`

### Tests

- `pkg/hub:TestArtifactsGrantsOnRoutes`
- `pkg/artifacts:TestGrantsAdminGate`
- `pkg/artifacts:TestStewardBoundedTable`

---

## artifact.grant.put

**Domain:** artifact

**Description:** Give a user, agent or project read, write or admin on an artifact, or a user review, or change an existing grant's permission (POST); a steward (on an artifact owned by an agent: an owner of its home project, or the agent's delegating user who reads in the home project) gives read, review or write, and an admin grant from it is refused with 403 steward_action_not_allowed; a caller that may not manage the artifact may still give a user the review level on an artifact owned by an agent when it is that agent's delegating user or an admin of the artifact's home project (project.manage there; ReviewGrantClause), to a user who can already read it through the home project or a grant to one of the user's projects, never replacing a read, write or admin grant; a caller with that authority asking for a well-formed review grant is refused with 403 review_subject_cannot_read when the user cannot read the artifact and 403 review_replaces_access when the user holds a read, write or admin grant; every other request from such a caller is refused with the same 403, and an accepted one can still answer 409 too_many_grants, or 404 when the artifact went away meanwhile

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/artifacts/{id}/grants` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `artifact_record`; boundaries `project`, `hub`; pinned by `TestArtifactsManageAndVersionRoutesUserAccessTokensAreBounded`)

**Base Permission:** `artifact.manage`

**Resource Resolver:** artifact-home-project

**Effects:** `grant-authority`

### Delegation

- **Kind:** `non_amplification`
- A manager (the owner or an admin grantee) grants at most admin, the level it holds, and admin is never given to the home project's grant; a steward grants at most write (no stored admin grant can outlive its computed authority), decided from roles and the delegation chain (StewardClause), never from grants the caller holds; the review path gives only the review level, which allows review versions and no read of its own, decided by the host's review-grant authority (MayGrantReview) rather than by grants the caller holds

### Audit

- **Event Type:** `artifact_event (grant_created, grant_changed)`
- **Context Fields:** artifact_id, actor_kind, actor_ref, authority
- **Before Fields:** oldPermission
- **After Fields:** grantId, subjectKind, subjectRef, permission, newPermission
- **Atomic:** Yes

**Denial Codes:** `not_found`, `forbidden`, `cross_project_sharing_disabled`, `review_subject_cannot_read`, `review_replaces_access`, `steward_action_not_allowed`

### Tests

- `pkg/hub:TestArtifactsGrantsOnRoutes`
- `pkg/hub:TestArtifactCanGrantReviewOnRoutes`
- `pkg/hub:TestArtifactHostMayGrantReview_ScopedToken`
- `pkg/artifacts:TestGrantsAdminGate`
- `pkg/artifacts:TestReviewGrantAllowed`
- `pkg/artifacts:TestReviewGrantDenied`
- `pkg/hub:TestArtifactStewardOnRoutes`
- `pkg/artifacts:TestStewardBoundedTable`
- `pkg/artifacts:TestStewardEvents`
- `pkg/artifacts:TestStoreCreationEvents`

---

## artifact.grant.delete

**Domain:** artifact

**Description:** Remove one grant from an artifact; the home project's grant cannot be removed

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | DELETE | `/api/v1/artifacts/{id}/grants/{grantId}` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `artifact_record`; boundaries `project`, `hub`; pinned by `TestArtifactsManageAndVersionRoutesUserAccessTokensAreBounded`)

**Base Permission:** `artifact.manage`

**Resource Resolver:** artifact-home-project

**Effects:** `revoke-authority`

### Governance

- **Kind:** domain_specific
- Only the artifact's managers (its owner or an admin grantee, a user whose credential permits artifact.manage in the home project) and, on an artifact owned by an agent, its stewards remove its grants; the home project's grant is refused (409 home_grant)
- **Callback:** `pkg/artifacts.(*Service).adminAuthorityWith`

### Audit

- **Event Type:** `artifact_event (grant_removed)`
- **Context Fields:** artifact_id, actor_kind, actor_ref, authority
- **Before Fields:** grantId, subjectKind, subjectRef, permission
- **Atomic:** Yes

**Denial Codes:** `not_found`, `forbidden`, `home_grant`

### Tests

- `pkg/hub:TestArtifactsManageAndVersionRoutesUserAccessTokensAreBounded`
- `pkg/artifacts:TestGrantsAdminGate`
- `pkg/artifacts:TestOwnerPathsRecordEvents`

---

## artifact.link.list

**Domain:** artifact

**Description:** List an artifact's unexpired share links (ids, creator, expiry; never the tokens)

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/artifacts/{id}/links` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `artifact_record`; boundaries `project`, `hub`; pinned by `TestArtifactsManageAndVersionRoutesUserAccessTokensAreBounded`)

**Base Permission:** `artifact.manage`

**Resource Resolver:** artifact-home-project

**Effects:** `list-scoped`

**Denial Codes:** `not_found`, `forbidden`

### Tests

- `pkg/hub:TestArtifactsShareLinkOnRoutes`
- `pkg/artifacts:TestLinkListAndRevoke`

---

## artifact.link.create

**Domain:** artifact

**Description:** Create a share link: an unguessable token, shown once, that lets anyone holding it read the artifact's current version without signing in until it expires (at most the hub's maximum lifetime, never past the artifact's own expiry) or is revoked

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/artifacts/{id}/links` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `artifact_record`; boundaries `project`, `hub`; pinned by `TestArtifactsManageAndVersionRoutesUserAccessTokensAreBounded`)

**Base Permission:** `artifact.manage`

**Resource Resolver:** artifact-home-project

**Effects:** `grant-authority`

### Delegation

- **Kind:** `non_amplification`
- A link gives read of the current version only, which the creating manager (the owner or an admin grantee) or steward already holds; it gives no write, review or admin

### Audit

- **Event Type:** `artifact_event (link_created)`
- **Context Fields:** artifact_id, actor_kind, actor_ref, authority
- **After Fields:** linkId, expiresAt
- **Atomic:** Yes

**Denial Codes:** `not_found`, `forbidden`

### Tests

- `pkg/hub:TestArtifactsShareLinkOnRoutes`
- `pkg/artifacts:TestLinkManagePermissions`
- `pkg/artifacts:TestStewardEvents`

---

## artifact.link.revoke

**Domain:** artifact

**Description:** Revoke one share link of an artifact

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | DELETE | `/api/v1/artifacts/{id}/links/{linkId}` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `artifact_record`; boundaries `project`, `hub`; pinned by `TestArtifactsManageAndVersionRoutesUserAccessTokensAreBounded`)

**Base Permission:** `artifact.manage`

**Resource Resolver:** artifact-home-project

**Effects:** `revoke-authority`

### Governance

- **Kind:** domain_specific
- Only the artifact's managers (its owner or an admin grantee, a user whose credential permits artifact.manage in the home project) and, on an artifact owned by an agent, its stewards revoke its share links
- **Callback:** `pkg/artifacts.(*Service).adminAuthorityWith`

### Audit

- **Event Type:** `artifact_event (link_revoked)`
- **Context Fields:** artifact_id, actor_kind, actor_ref, authority
- **Before Fields:** linkId
- **Atomic:** Yes

**Denial Codes:** `not_found`, `forbidden`

### Tests

- `pkg/hub:TestArtifactsShareLinkOnRoutes`
- `pkg/artifacts:TestLinkListAndRevoke`

---

## artifact.delete

**Domain:** artifact

**Description:** Delete an artifact with its grants and share links (its files are removed after the hub's grace period); allowed for its managers (the owner or an admin grantee), on an artifact owned by an agent for its stewards (an owner of its home project, or the agent's delegating user who reads there), and, as custodian, for an admin of its home project (project.manage there), each a user whose credential permits artifact.manage in the home project

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | DELETE | `/api/v1/artifacts/{id}` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `artifact_record`; boundaries `project`, `hub`; pinned by `TestArtifactsManageAndVersionRoutesUserAccessTokensAreBounded`)

**Base Permission:** `artifact.manage`

**Resource Resolver:** artifact-home-project

**Effects:** `delete-resource`

### Audit

- **Event Type:** `artifact_event (deleted)`
- **Context Fields:** artifact_id, actor_kind, actor_ref, authority
- **Before Fields:** grantsRemoved, linksRevoked
- **Atomic:** Yes

**Denial Codes:** `not_found`, `forbidden`

### Tests

- `pkg/hub:TestArtifactDeleteOfOrphanOnRoutes`
- `pkg/artifacts:TestDeleteArtifactAllowed`
- `pkg/artifacts:TestDeleteArtifactRefused`
- `pkg/artifacts:TestStoreChangeAndEventCommitTogether`

---

## artifact.version.create

**Domain:** artifact

**Description:** Start a pending version of an artifact from a manifest; a review grantee may start review versions only

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/artifacts/{id}/versions` |

**Principals:** `user`, `agent`

**Credentials:** `session_jwt`, `scoped_uat`, `agent_jwt`

**Bearer:** `admit` (target `artifact_record`; boundaries `project`, `hub`; pinned by `TestArtifactsManageAndVersionRoutesUserAccessTokensAreBounded`)

**Base Permission:** `artifact.create`

**Resource Resolver:** artifact-home-project

**Effects:** `create-resource`

**Denial Codes:** `not_found`, `forbidden`, `review_only`

### Tests

- `pkg/hub:TestArtifactsTwoStepBundleOnRoutes`
- `pkg/artifacts:TestTwoStepWriteAuthorization`

---

## artifact.version.upload

**Domain:** artifact

**Description:** Upload one manifest file of a pending version; only the principal that started the version may upload to it

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | PUT | `/api/v1/artifacts/{id}/versions/{seq}/files/{path}` |

**Principals:** `user`, `agent`

**Credentials:** `session_jwt`, `scoped_uat`, `agent_jwt`

**Bearer:** `admit` (target `artifact_record`; boundaries `project`, `hub`; pinned by `TestArtifactsManageAndVersionRoutesUserAccessTokensAreBounded`)

**Base Permission:** `artifact.create`

**Resource Resolver:** artifact-home-project

**Effects:** `update-resource`

**Denial Codes:** `not_found`, `forbidden`, `review_only`

### Tests

- `pkg/hub:TestArtifactsTwoStepBundleOnRoutes`
- `pkg/artifacts:TestTwoStepWriteAuthorization`

---

## artifact.version.finalize

**Domain:** artifact

**Description:** Finalize a pending version once every file has arrived, making it ready (and current unless a later one is); only the principal that started it may finalize it, and a review is checked against its base

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/artifacts/{id}/versions/{seq}/finalize` |

**Principals:** `user`, `agent`

**Credentials:** `session_jwt`, `scoped_uat`, `agent_jwt`

**Bearer:** `admit` (target `artifact_record`; boundaries `project`, `hub`; pinned by `TestArtifactsManageAndVersionRoutesUserAccessTokensAreBounded`)

**Base Permission:** `artifact.create`

**Resource Resolver:** artifact-home-project

**Effects:** `update-resource`

**Denial Codes:** `not_found`, `forbidden`, `review_only`

### Tests

- `pkg/hub:TestArtifactsTwoStepBundleOnRoutes`
- `pkg/hub:TestArtifactsReviewOnRoutes`
- `pkg/artifacts:TestTwoStepWriteAuthorization`

---

## artifact.version.view

**Domain:** artifact

**Description:** Get a view URL for a ready version whose entry is HTML: a capability, valid for a limited time, that serves only that version's files under /api/v1/artifacts/view/ in the sandboxed viewer (see the view exemption); the caller needs only the read check

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/artifacts/{id}/versions/{seq}/view` |

**Principals:** `user`, `agent`

**Credentials:** `session_jwt`, `scoped_uat`, `agent_jwt`

**Bearer:** `admit` (target `artifact_record`; boundaries `project`, `hub`; pinned by `TestArtifactsManageAndVersionRoutesUserAccessTokensAreBounded`)

**Base Permission:** `artifact.read`

**Resource Resolver:** artifact-home-project

**Effects:** `read-one`

**Denial Codes:** `not_found`

### Tests

- `pkg/hub:TestArtifactsHTMLViewOnRoutes`
- `pkg/artifacts:TestViewMintAuthorization`

---

## artifact.version.read

**Domain:** artifact

**Description:** List an artifact's versions (ready ones, and discarded ones by number and reason) or read one ready version's metadata and file list

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/artifacts/{id}/versions` |
| http_route | GET | `/api/v1/artifacts/{id}/versions/{seq}` |

**Principals:** `user`, `agent`

**Credentials:** `session_jwt`, `scoped_uat`, `agent_jwt`

**Bearer:** `admit` (target `artifact_record`; boundaries `project`, `hub`; pinned by `TestArtifactsManageAndVersionRoutesUserAccessTokensAreBounded`)

**Base Permission:** `artifact.read`

**Resource Resolver:** artifact-home-project

**Effects:** `read-one`, `list-scoped`

**Denial Codes:** `not_found`

### Tests

- `pkg/hub:TestArtifactsTwoStepBundleOnRoutes`
- `pkg/artifacts:TestListVersionsDiscarded`

---

## project.env.read

**Domain:** project

**Description:** Read a project's environment variables (list or one key)

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/projects/{id}/env` |
| http_route | GET | `/api/v1/projects/{id}/env/{key}` |

**Principals:** `user`, `agent`

**Credentials:** `session_jwt`, `scoped_uat`, `agent_jwt`

**Bearer:** `admit` (target `project_path`; boundaries `project`, `hub`)

**Base Permission:** `project.read`

**Resource Resolver:** project-from-url

**Effects:** `read-one`, `list-scoped`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestBearerDispositionMatrix_CatalogEntryPoints`

---

## project.env.write

**Domain:** project

**Description:** Set or delete a project environment variable

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | PUT | `/api/v1/projects/{id}/env/{key}` |
| http_route | DELETE | `/api/v1/projects/{id}/env/{key}` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `project_path`; boundaries `project`, `hub`)

**Base Permission:** `project.update`

**Resource Resolver:** project-from-url

**Effects:** `create-resource`, `update-resource`, `delete-resource`

### Audit

- **Event Type:** `project.env.write`
- **Context Fields:** actor_id, project_id
- **Before Fields:** env_key
- **Atomic:** Yes

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestBearerDispositionMatrix_CatalogEntryPoints`

---

## project.secret.read

**Domain:** project

**Description:** Read a project's secret metadata (list or one key)

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/projects/{id}/secrets` |
| http_route | GET | `/api/v1/projects/{id}/secrets/{key}` |

**Principals:** `user`, `agent`

**Credentials:** `session_jwt`, `scoped_uat`, `agent_jwt`

**Bearer:** `admit` (target `project_path`; boundaries `project`, `hub`)

**Base Permission:** `project.read`

**Resource Resolver:** project-from-url

**Effects:** `read-one`, `list-scoped`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestBearerDispositionMatrix_CatalogEntryPoints`

---

## project.secret.write

**Domain:** project

**Description:** Set, patch or delete a project secret

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | PUT | `/api/v1/projects/{id}/secrets/{key}` |
| http_route | PATCH | `/api/v1/projects/{id}/secrets/{key}` |
| http_route | DELETE | `/api/v1/projects/{id}/secrets/{key}` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `project_path`; boundaries `project`, `hub`)

**Base Permission:** `project.update`

**Resource Resolver:** project-from-url

**Effects:** `create-resource`, `update-resource`, `delete-resource`

### Audit

- **Event Type:** `project.secret.write`
- **Context Fields:** actor_id, project_id
- **Before Fields:** secret_key
- **Atomic:** Yes

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestBearerDispositionMatrix_CatalogEntryPoints`

---

## project.providers.list

**Domain:** project

**Description:** List the runtime brokers that provide for a project

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/projects/{id}/providers` |

**Principals:** `user`, `agent`

**Credentials:** `session_jwt`, `scoped_uat`, `agent_jwt`

**Bearer:** `admit` (target `project_path`; boundaries `project`, `hub`)

**Base Permission:** `project.read`

**Resource Resolver:** project-from-url

**Effects:** `list-scoped`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestBearerDispositionMatrix_CatalogEntryPoints`

---

## project.shareddir.read

**Domain:** project

**Description:** List a project's shared directories and read their files and archives

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/projects/{id}/shared-dirs` |
| http_route | GET | `/api/v1/projects/{id}/shared-dirs/{name}/archive` |
| http_route | GET | `/api/v1/projects/{id}/shared-dirs/{name}/files` |
| http_route | GET | `/api/v1/projects/{id}/shared-dirs/{name}/files/{path}` |

**Principals:** `user`, `agent`

**Credentials:** `session_jwt`, `scoped_uat`, `agent_jwt`

**Bearer:** `admit` (target `project_path`; boundaries `project`, `hub`)

**Base Permission:** `project.read`

**Resource Resolver:** project-from-url

**Effects:** `read-one`, `list-scoped`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestBearerDispositionMatrix_CatalogEntryPoints`

---

## project.shareddir.write

**Domain:** project

**Description:** Create or delete a project shared directory, and upload, write or delete its files

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/projects/{id}/shared-dirs` |
| http_route | POST | `/api/v1/projects/{id}/shared-dirs/{name}/files` |
| http_route | PUT | `/api/v1/projects/{id}/shared-dirs/{name}/files/{path}` |
| http_route | DELETE | `/api/v1/projects/{id}/shared-dirs/{name}/files/{path}` |
| http_route | DELETE | `/api/v1/projects/{id}/shared-dirs/{name}` |

**Principals:** `user`, `agent`

**Credentials:** `session_jwt`, `scoped_uat`, `agent_jwt`

**Bearer:** `admit` (target `project_path`; boundaries `project`, `hub`)

**Base Permission:** `project.update`

**Resource Resolver:** project-from-url

**Effects:** `create-resource`, `update-resource`, `delete-resource`

### Audit

- **Event Type:** `project.shareddir.write`
- **Context Fields:** actor_id, project_id
- **Before Fields:** shared_dir_name
- **Atomic:** Yes

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestBearerDispositionMatrix_CatalogEntryPoints`

---

## project.injectedskills.read

**Domain:** project

**Description:** List the skills injected into a project's agents

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/projects/{id}/injected-skills` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `project_path`; boundaries `project`, `hub`)

**Base Permission:** `project.read`

**Resource Resolver:** project-from-url

**Effects:** `list-scoped`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestBearerDispositionMatrix_CatalogEntryPoints`

---

## project.injectedskills.write

**Domain:** project

**Description:** Add, replace or remove skills injected into a project's agents

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/projects/{id}/injected-skills` |
| http_route | DELETE | `/api/v1/projects/{id}/injected-skills/{entryId}` |
| http_route | PUT | `/api/v1/projects/{id}/injected-skills` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `project_path`; boundaries `project`, `hub`)

**Base Permission:** `project.update`

**Resource Resolver:** project-from-url

**Effects:** `create-resource`, `update-resource`, `delete-resource`

### Audit

- **Event Type:** `project.injectedskills.write`
- **Context Fields:** actor_id, project_id
- **Before Fields:** entry_id
- **Atomic:** Yes

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestBearerDispositionMatrix_CatalogEntryPoints`

---

## project.gcpsa.create

**Domain:** project

**Description:** Register a project-scoped GCP service account. Project-route service account writes use project.manage

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/projects/{id}/gcp-service-accounts` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `project_path`; boundaries `project`, `hub`)

**Base Permission:** `project.manage`

**Resource Resolver:** project-from-url

**Effects:** `create-resource`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestBearerDispositionMatrix_CatalogEntryPoints`

---

## project.messagelogs.read

**Domain:** project

**Description:** Read and stream a project's message log. A stream opened by a token ends once the token stops validating or loses project.read

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/projects/{id}/message-logs` |
| sse | GET | `/api/v1/projects/{id}/message-logs/stream` |

**Principals:** `user`, `agent`

**Credentials:** `session_jwt`, `scoped_uat`, `agent_jwt`

**Bearer:** `admit` (target `project_path`; boundaries `project`, `hub`)

**Base Permission:** `project.read`

**Resource Resolver:** project-from-url

**Effects:** `list-scoped`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestBearerDispositionMatrix_CatalogEntryPoints`

---

## project.broadcast

**Domain:** project

**Description:** Broadcast a message to a project's agents. Each recipient is then filtered by agent.message

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/projects/{id}/broadcast` |

**Principals:** `user`, `agent`

**Credentials:** `session_jwt`, `scoped_uat`, `agent_jwt`

**Bearer:** `admit` (target `project_path`; boundaries `project`, `hub`)

**Base Permission:** `project.read`

**Resource Resolver:** project-from-url

**Effects:** `create-resource`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestBearerDispositionMatrix_CatalogEntryPoints`

---

## project.metrics.read

**Domain:** project

**Description:** Read a project's metrics summary, session metrics summary and metrics dashboard

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/projects/{id}/metrics-summary` |
| http_route | GET | `/api/v1/projects/{id}/metrics/summary` |
| http_route | GET | `/api/v1/projects/{id}/metrics` |

**Principals:** `user`, `agent`

**Credentials:** `session_jwt`, `scoped_uat`, `agent_jwt`

**Bearer:** `admit` (target `project_path`; boundaries `project`, `hub`)

**Base Permission:** `project.read`

**Resource Resolver:** project-from-url

**Effects:** `read-one`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestBearerDispositionMatrix_CatalogEntryPoints`

---

## project.prestarthooks.read

**Domain:** project

**Description:** List or read a project's pre-start hooks

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/projects/{id}/pre-start-hooks` |
| http_route | GET | `/api/v1/projects/{id}/pre-start-hooks/{hookId}` |

**Principals:** `user`, `agent`

**Credentials:** `session_jwt`, `scoped_uat`, `agent_jwt`

**Bearer:** `admit` (target `project_path`; boundaries `project`, `hub`)

**Base Permission:** `project.read`

**Resource Resolver:** project-from-url

**Effects:** `read-one`, `list-scoped`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestBearerDispositionMatrix_CatalogEntryPoints`

---

## project.prestarthooks.write

**Domain:** project

**Description:** Create, update, delete or activate a project pre-start hook

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/projects/{id}/pre-start-hooks` |
| http_route | PUT | `/api/v1/projects/{id}/pre-start-hooks/{hookId}` |
| http_route | POST | `/api/v1/projects/{id}/pre-start-hooks/{hookId}/activate` |
| http_route | DELETE | `/api/v1/projects/{id}/pre-start-hooks/{hookId}` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `project_path`; boundaries `project`, `hub`)

**Base Permission:** `project.update`

**Resource Resolver:** project-from-url

**Effects:** `create-resource`, `update-resource`, `delete-resource`

### Audit

- **Event Type:** `project.prestarthooks.write`
- **Context Fields:** actor_id, project_id
- **Before Fields:** hook_id
- **Atomic:** Yes

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestBearerDispositionMatrix_CatalogEntryPoints`

---

## project.settings.read

**Domain:** project

**Description:** Read a project's settings and its resolved settings

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/projects/{id}/settings` |
| http_route | GET | `/api/v1/projects/{id}/settings/resolved` |

**Principals:** `user`, `agent`

**Credentials:** `session_jwt`, `scoped_uat`, `agent_jwt`

**Bearer:** `admit` (target `project_path`; boundaries `project`, `hub`)

**Base Permission:** `project.read`

**Resource Resolver:** project-from-url

**Effects:** `read-one`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestBearerDispositionMatrix_CatalogEntryPoints`

---

## project.settings.update

**Domain:** project

**Description:** Replace a project's settings

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | PUT | `/api/v1/projects/{id}/settings` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `project_path`; boundaries `project`, `hub`)

**Base Permission:** `project.update`

**Resource Resolver:** project-from-url

**Effects:** `update-resource`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestBearerDispositionMatrix_CatalogEntryPoints`

---

## project.messagingpolicy.read

**Domain:** project

**Description:** Read a project's cross-project inbound messaging policy

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/projects/{id}/messaging-policy` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `project_path`; boundaries `project`, `hub`)

**Base Permission:** `project.read`

**Resource Resolver:** project-from-url

**Effects:** `read-one`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestBearerDispositionMatrix_CatalogEntryPoints`

---

## project.messagingpolicy.update

**Domain:** project

**Description:** Set a project's cross-project inbound messaging policy. The caller also must be an active direct project owner or a local unscoped hub admin

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | PUT | `/api/v1/projects/{id}/messaging-policy` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `project_path`; boundaries `project`, `hub`; pinned by `TestProjectMessagingPolicyPut_RequiresSetMessagingPolicySelector`)

**Base Permission:** `project.set_messaging_policy`

**Resource Resolver:** project-from-url

**Effects:** `update-resource`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestProjectMessagingPolicyPut_RequiresSetMessagingPolicySelector`

---

## project.template.set

**Domain:** project

**Description:** Mark or unmark a project as a template. The caller needs project.update and project.clone on the project

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/projects/{id}/set-template` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `project_path`; boundaries `project`, `hub`; pinned by `TestSetTemplate_RequiresUpdateAndCloneOnTheProject`)

**Base Permission:** `project.update`

**Resource Resolver:** project-from-url

**Effects:** `update-resource`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestSetTemplate_RequiresUpdateAndCloneOnTheProject`

---

## template.project.import

**Domain:** template

**Description:** Discover or import templates into a project

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/projects/{id}/discover-templates` |
| http_route | POST | `/api/v1/projects/{id}/import-templates` |

**Principals:** `user`, `agent`

**Credentials:** `session_jwt`, `scoped_uat`, `agent_jwt`

**Bearer:** `admit` (target `project_collection`; boundaries `project`, `hub`)

**Base Permission:** `template.create`

**Resource Resolver:** project-from-url

**Effects:** `create-resource`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestBearerDispositionMatrix_CatalogEntryPoints`

---

## harnessconfig.project.import

**Domain:** harnessconfig

**Description:** Discover or import harness configs into a project

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/projects/{id}/discover-harness-configs` |
| http_route | POST | `/api/v1/projects/{id}/import-harness-configs` |

**Principals:** `user`, `agent`

**Credentials:** `session_jwt`, `scoped_uat`, `agent_jwt`

**Bearer:** `admit` (target `project_collection`; boundaries `project`, `hub`)

**Base Permission:** `harness_config.create`

**Resource Resolver:** project-from-url

**Effects:** `create-resource`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestBearerDispositionMatrix_CatalogEntryPoints`

---

## project.workspace.read

**Domain:** project

**Description:** Read a project's workspace: WebDAV reads, sync status, cache status, archive and file reads

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/projects/{id}/dav/{path}` |
| http_route | HEAD | `/api/v1/projects/{id}/dav/{path}` |
| http_route | OPTIONS | `/api/v1/projects/{id}/dav/{path}` |
| http_route | GET | `/api/v1/projects/{id}/sync/status` |
| http_route | GET | `/api/v1/projects/{id}/workspace/archive` |
| http_route | GET | `/api/v1/projects/{id}/workspace/cache/status` |
| http_route | GET | `/api/v1/projects/{id}/workspace/files` |
| http_route | GET | `/api/v1/projects/{id}/workspace/files/{path}` |

**Principals:** `user`, `agent`

**Credentials:** `session_jwt`, `scoped_uat`, `agent_jwt`

**Bearer:** `admit` (target `project_path`; boundaries `project`, `hub`)

**Base Permission:** `project.read`

**Resource Resolver:** project-from-url

**Effects:** `read-one`, `list-scoped`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestBearerDispositionMatrix_CatalogEntryPoints`

---

## project.workspace.write

**Domain:** project

**Description:** Change a project's workspace: WebDAV writes, cache refresh and notify, file upload, write and delete, and git pull

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | PUT | `/api/v1/projects/{id}/dav/{path}` |
| http_route | PROPPATCH | `/api/v1/projects/{id}/dav/{path}` |
| http_route | LOCK | `/api/v1/projects/{id}/dav/{path}` |
| http_route | UNLOCK | `/api/v1/projects/{id}/dav/{path}` |
| http_route | COPY | `/api/v1/projects/{id}/dav/{path}` |
| http_route | MOVE | `/api/v1/projects/{id}/dav/{path}` |
| http_route | POST | `/api/v1/projects/{id}/dav/{path}` |
| http_route | PROPFIND | `/api/v1/projects/{id}/dav/{path}` |
| http_route | DELETE | `/api/v1/projects/{id}/dav/{path}` |
| http_route | MKCOL | `/api/v1/projects/{id}/dav/{path}` |
| http_route | POST | `/api/v1/projects/{id}/workspace/cache/notify` |
| http_route | POST | `/api/v1/projects/{id}/workspace/cache/refresh` |
| http_route | POST | `/api/v1/projects/{id}/workspace/files` |
| http_route | PUT | `/api/v1/projects/{id}/workspace/files/{path}` |
| http_route | DELETE | `/api/v1/projects/{id}/workspace/files/{path}` |
| http_route | POST | `/api/v1/projects/{id}/workspace/pull` |

**Principals:** `user`, `agent`

**Credentials:** `session_jwt`, `scoped_uat`, `agent_jwt`

**Bearer:** `admit` (target `project_path`; boundaries `project`, `hub`)

**Base Permission:** `project.update`

**Resource Resolver:** project-from-url

**Effects:** `create-resource`, `update-resource`, `delete-resource`

### Audit

- **Event Type:** `project.workspace.write`
- **Context Fields:** actor_id, project_id
- **Before Fields:** path
- **Atomic:** Yes

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestBearerDispositionMatrix_CatalogEntryPoints`

---

## project.github.read

**Domain:** project

**Description:** Read a project's GitHub status, GitHub permissions and git identity

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/projects/{id}/github-status` |
| http_route | GET | `/api/v1/projects/{id}/github-permissions` |
| http_route | GET | `/api/v1/projects/{id}/git-identity` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `project_path`; boundaries `project`, `hub`)

**Base Permission:** `project.read`

**Resource Resolver:** project-from-url

**Effects:** `read-one`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestBearerDispositionMatrix_CatalogEntryPoints`

---

## project.github.write

**Domain:** project

**Description:** Change a project's GitHub installation, status check, GitHub permissions and git identity

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | PUT | `/api/v1/projects/{id}/github-installation` |
| http_route | DELETE | `/api/v1/projects/{id}/github-installation` |
| http_route | POST | `/api/v1/projects/{id}/github-status` |
| http_route | PUT | `/api/v1/projects/{id}/github-permissions` |
| http_route | DELETE | `/api/v1/projects/{id}/github-permissions` |
| http_route | PUT | `/api/v1/projects/{id}/git-identity` |
| http_route | DELETE | `/api/v1/projects/{id}/git-identity` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `project_path`; boundaries `project`, `hub`)

**Base Permission:** `project.update`

**Resource Resolver:** project-from-url

**Effects:** `create-resource`, `update-resource`, `delete-resource`

### Audit

- **Event Type:** `project.github.write`
- **Context Fields:** actor_id, project_id
- **Before Fields:** setting
- **Atomic:** Yes

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestBearerDispositionMatrix_CatalogEntryPoints`

---

## project.members.assignableroles

**Domain:** project

**Description:** List the roles the caller may assign in a project

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/projects/{id}/members/assignable-roles` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `project_path`; boundaries `project`, `hub`)

**Base Permission:** `project.manage`

**Resource Resolver:** project-from-url

**Effects:** `list-scoped`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestBearerDispositionMatrix_CatalogEntryPoints`

---

## agent.message.send

**Domain:** agent.message

**Description:** Send a message to an agent

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| broker_call | — | `broker.inbound` |

**Principals:** `user`, `agent`, `broker`

**Credentials:** `session_jwt`, `scoped_uat`, `agent_jwt`, `broker_token`

**Base Permission:** `agent.message`

**Resource Resolver:** agent-from-thread

**Effects:** `emit-external-effect`

### Audit

- **Event Type:** `agent.message.send`
- **Context Fields:** actor_id, project_id
- **After Fields:** message_id, target_agent_id
- **Atomic:** No
- **Non-Atomic Justification:** Message dispatch is fire-and-forget; audit recorded before dispatch

### External Effect Policy

- **Delivery:** `fire_and_forget`
- **Failure Mode:** `log_and_continue`
- **Idempotency:** message ID
- **Retry:** no retry for user-sent messages
- **Auth Before Emit:** Yes

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`

---

## inbox.message.read

**Domain:** inbox

**Description:** List and read the caller's own inbox messages. A project token lists only messages of its boundary project

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/messages` |
| http_route | GET | `/api/v1/messages/{id}` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `self_record`; boundaries `project`, `hub`)

**Base Permission:** `inbox.read`

**Resource Resolver:** self-principal

**Effects:** `list-scoped`, `read-one`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestInboxToken_ProjectBoundaryFiltersMessages`
- `pkg/hub:TestInboxToken_ProjectMembershipRecheckedOnEveryRequest`

---

## inbox.message.write

**Domain:** inbox

**Description:** Mark the caller's own inbox messages read. Mark-all by a project token touches only messages of its boundary project

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/messages/{id}/read` |
| http_route | POST | `/api/v1/messages/read-all` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `self_record`; boundaries `project`, `hub`)

**Base Permission:** `inbox.write`

**Resource Resolver:** self-principal

**Effects:** `update-resource`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestInboxToken_MarkAllReadTouchesOnlyVisibleRows`
- `pkg/hub:TestInboxToken_RefusedCredentialKinds`

---

## inbox.channels.list

**Domain:** inbox

**Description:** List the registered message channels: static capability metadata with no records

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/message-channels` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit_self` (self filter `none`; pinned by `TestMessagingStaticMetadata_AnyTokenReads`)

**Resource Resolver:** none

**Effects:** `read-one`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestMessagingStaticMetadata_AnyTokenReads`

### Exemptions

- **authentication_only:** Static channel metadata; carries no user records (scope: static messaging metadata only) — waives: `base_permission`

---

## inbox.capabilities.read

**Domain:** inbox

**Description:** Read the hub messaging capabilities: static capability metadata with no records

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/messaging/capabilities` |

**Principals:** `user`, `agent`

**Credentials:** `session_jwt`, `scoped_uat`, `agent_jwt`

**Bearer:** `admit_self` (self filter `none`; pinned by `TestMessagingStaticMetadata_AnyTokenReads`)

**Resource Resolver:** none

**Effects:** `read-one`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestMessagingStaticMetadata_AnyTokenReads`

### Exemptions

- **authentication_only:** Static capability metadata; carries no user records (scope: static messaging metadata only) — waives: `base_permission`

---

## inbox.conversation.list

**Domain:** inbox

**Description:** List the caller's conversations. A group conversation is listed only while the caller can read it (project:read on its project, or the participant rule for a group with no project; a token is checked as its user); a participant row alone does not list it. A token lists only conversations inside its boundary, and a direct conversation with an agent only with agent:read on that agent; the project group union also needs project:read

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/conversations` |

**Principals:** `user`, `agent`

**Credentials:** `session_jwt`, `scoped_uat`, `agent_jwt`

**Bearer:** `admit` (target `self_record`; boundaries `project`, `hub`)

**Base Permission:** `inbox.read`

**Resource Resolver:** self-principal

**Effects:** `list-scoped`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestConversationListToken_FilteredToBoundary`
- `pkg/hub:TestConversationList_OmitsGroupsOfUnreadableProject`
- `pkg/hub:TestConversationList_GroupReadLookupErrorOmitsRow`

---

## inbox.conversation.create

**Domain:** inbox

**Description:** Create a group conversation in a project. Needs project:read on the project; a token also needs inbox:write for it

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/conversations` |
| http_route | POST | `/api/v1/conversations/` |

**Principals:** `user`, `agent`

**Credentials:** `session_jwt`, `scoped_uat`, `agent_jwt`

**Bearer:** `admit` (target `project_body`; boundaries `project`, `hub`)

**Base Permission:** `inbox.write`

**Resource Resolver:** project-from-body

**Effects:** `create-resource`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestConversationCreateToken_RequiresInboxWriteAndProjectRead`

---

## project.conversation.read

**Domain:** project

**Description:** Read a group conversation, its messages and one message. Needs project:read on the conversation's project; a group with no project needs participation, and a token needs inbox:read on a hub boundary for it

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/conversations/{id}` |
| http_route | GET | `/api/v1/conversations/{id}/messages` |
| http_route | GET | `/api/v1/conversations/{id}/messages/{messageId}` |

**Principals:** `user`, `agent`

**Credentials:** `session_jwt`, `scoped_uat`, `agent_jwt`

**Bearer:** `admit` (target `conversation_record`; boundaries `project`, `hub`)

**Base Permission:** `project.read`

**Resource Resolver:** conversation-project

**Effects:** `read-one`, `list-scoped`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestGroupConversationToken_ProjectlessGroupRequiresHubBoundary`

---

## inbox.conversation.direct.read

**Domain:** inbox

**Description:** Read a direct conversation, its messages and one message. A token needs inbox:read for the peer agent's project and agent:read on the peer agent; a direct conversation between users needs a hub boundary

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/conversations/{id}` |
| http_route | GET | `/api/v1/conversations/{id}/messages` |
| http_route | GET | `/api/v1/conversations/{id}/messages/{messageId}` |

**Principals:** `user`, `agent`

**Credentials:** `session_jwt`, `scoped_uat`, `agent_jwt`

**Bearer:** `admit` (target `self_record`; boundaries `project`, `hub`)

**Base Permission:** `inbox.read`

**Resource Resolver:** self-principal

**Effects:** `read-one`, `list-scoped`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestDirectConversationToken_PeerAgentMustBeInsideBoundary`

---

## inbox.conversation.defaultagent.set

**Domain:** inbox

**Description:** Set the default agent of a group conversation. Needs project:read on the conversation's project; a token also needs inbox:write for it

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | PUT | `/api/v1/conversations/{id}/default-agent` |

**Principals:** `user`, `agent`

**Credentials:** `session_jwt`, `scoped_uat`, `agent_jwt`

**Bearer:** `admit` (target `conversation_record`; boundaries `project`, `hub`)

**Base Permission:** `inbox.write`

**Resource Resolver:** conversation-project

**Effects:** `update-resource`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestConversationCreateToken_RequiresInboxWriteAndProjectRead`

---

## inbox.conversation.participant.add

**Domain:** inbox

**Description:** Add a participant to a group conversation. Every caller needs project:read on the conversation's project; an added agent must be in that project and an added user must be a member of it; a token also needs inbox:write for it. A caller who is not a participant or cannot read the group gets the unknown-conversation answer, and an agent of another project the unknown-agent answer

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/conversations/{id}/participants` |

**Principals:** `user`, `agent`

**Credentials:** `session_jwt`, `scoped_uat`, `agent_jwt`

**Bearer:** `admit` (target `conversation_record`; boundaries `project`, `hub`)

**Base Permission:** `inbox.write`

**Resource Resolver:** conversation-project

**Effects:** `update-resource`

**Denial Codes:** `forbidden`, `not_found`

### Tests

- `pkg/hub:TestConversationAddParticipant_RequiresProjectReadAndMemberPrincipals`
- `pkg/hub:TestConversationAddParticipant_UnreadableGroupMatchesUnknownConversation`
- `pkg/hub:TestConversationAddParticipant_AgentOfOtherProjectMatchesUnknownAgent`

---

## inbox.conversation.leave

**Domain:** inbox

**Description:** Leave a conversation the caller takes part in. A token needs inbox:write for the conversation

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/conversations/{id}/leave` |

**Principals:** `user`, `agent`

**Credentials:** `session_jwt`, `scoped_uat`, `agent_jwt`

**Bearer:** `admit` (target `self_record`; boundaries `project`, `hub`)

**Base Permission:** `inbox.write`

**Resource Resolver:** self-principal

**Effects:** `update-resource`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestConversationLeaveToken_RequiresInboxWrite`

---

## inbox.conversation.resolve

**Domain:** inbox

**Description:** Resolve a conversation reference. A group reference needs project:read on its project and an agent reference needs agent:read on the agent, for every user caller; a token also needs inbox:read for the result

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/conversations/resolve` |

**Principals:** `user`, `agent`

**Credentials:** `session_jwt`, `scoped_uat`, `agent_jwt`

**Bearer:** `admit` (target `self_record`; boundaries `project`, `hub`)

**Base Permission:** `inbox.read`

**Resource Resolver:** self-principal

**Effects:** `read-one`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestConversationResolve_GroupReferenceRequiresProjectRead`
- `pkg/hub:TestConversationResolve_AgentReferenceRequiresAgentRead`

---

## project.threadgroup.list

**Domain:** project

**Description:** List a project's chat thread groups. Needs project:read on the project; an agent must belong to it

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/thread-groups` |

**Principals:** `user`, `agent`

**Credentials:** `session_jwt`, `scoped_uat`, `agent_jwt`

**Bearer:** `admit` (target `project_query`; boundaries `project`, `hub`)

**Base Permission:** `project.read`

**Resource Resolver:** project-from-query

**Effects:** `list-scoped`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestThreadGroups_TokenWithoutInboxWriteReadsOnly`
- `pkg/hub:TestThreadGroups_OtherProjectAgentForbidden`

---

## project.threadgroup.read

**Domain:** project

**Description:** Read a chat thread group. Needs project:read on the group's project; an agent must belong to it

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/thread-groups/{id}` |

**Principals:** `user`, `agent`

**Credentials:** `session_jwt`, `scoped_uat`, `agent_jwt`

**Bearer:** `admit` (target `thread_group_record`; boundaries `project`, `hub`)

**Base Permission:** `project.read`

**Resource Resolver:** threadgroup-project

**Effects:** `read-one`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestThreadGroups_TokenWithoutInboxWriteReadsOnly`
- `pkg/hub:TestThreadGroups_NonMemberUserForbidden`

---

## inbox.threadgroup.create

**Domain:** inbox

**Description:** Create a chat thread group in a project. Needs project:read on the project (an agent must belong to it); a token also needs inbox:write for it

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/thread-groups` |

**Principals:** `user`, `agent`

**Credentials:** `session_jwt`, `scoped_uat`, `agent_jwt`

**Bearer:** `admit` (target `project_body`; boundaries `project`, `hub`)

**Base Permission:** `inbox.write`

**Resource Resolver:** project-from-body

**Effects:** `create-resource`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestThreadGroups_TokenWithoutInboxWriteReadsOnly`
- `pkg/hub:TestThreadGroups_SameProjectAgentAllButDelete`

---

## inbox.threadgroup.update

**Domain:** inbox

**Description:** Rename a chat thread group, or add or remove one of its threads. Needs project:read on the group's project (an agent must belong to it); a token also needs inbox:write for it. An added thread must be a live group conversation of the same project

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | PATCH | `/api/v1/thread-groups/{id}` |
| http_route | POST | `/api/v1/thread-groups/{id}/threads` |
| http_route | DELETE | `/api/v1/thread-groups/{id}/threads/{conversationId}` |

**Principals:** `user`, `agent`

**Credentials:** `session_jwt`, `scoped_uat`, `agent_jwt`

**Bearer:** `admit` (target `thread_group_record`; boundaries `project`, `hub`)

**Base Permission:** `inbox.write`

**Resource Resolver:** threadgroup-project

**Effects:** `update-resource`

**Denial Codes:** `forbidden`, `not_found`

### Tests

- `pkg/hub:TestThreadGroups_TokenWithoutInboxWriteReadsOnly`
- `pkg/hub:TestThreadGroups_AddThreadValidation`

---

## inbox.threadgroup.delete

**Domain:** inbox

**Description:** Delete a chat thread group; its threads become ungrouped. Users only (an agent gets 403). Needs project:read on the group's project; a token also needs inbox:write for it

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | DELETE | `/api/v1/thread-groups/{id}` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `thread_group_record`; boundaries `project`, `hub`)

**Base Permission:** `inbox.write`

**Resource Resolver:** threadgroup-project

**Effects:** `delete-resource`

### Audit

- **Event Type:** `inbox.threadgroup.delete`
- **Context Fields:** actor_id, project_id
- **Before Fields:** thread_group_id
- **Atomic:** Yes

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestThreadGroups_SameProjectAgentAllButDelete`
- `pkg/hub:TestThreadGroups_TokenWithoutInboxWriteReadsOnly`

---

## agent.message.target.resolve

**Domain:** agent.message

**Description:** Resolve a messaging target in another project through the agent message authorization. Only a target the caller may message is answered; any other target gets the unknown-target answer

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/messaging/targets/resolve` |

**Principals:** `user`, `agent`

**Credentials:** `session_jwt`, `scoped_uat`, `agent_jwt`

**Bearer:** `admit` (target `agent_record`; boundaries `project`, `hub`)

**Base Permission:** `agent.message`

**Resource Resolver:** agent-from-query

**Effects:** `read-one`

**Denial Codes:** `forbidden`, `not_found`

### Tests

- `pkg/hub:TestMessagingTargetsResolve_TokenNeedsAgentMessage`
- `pkg/hub:TestMessagingTargetsResolve_ReplyOnlyTargetMatchesMissing`

---

## inbox.notification.read

**Domain:** inbox

**Description:** List the caller's notifications. A token lists only rows inside its boundary; with agentId, rows addressed to the agent subscriber need agent:read on that agent, for every user caller

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/notifications` |

**Principals:** `user`, `agent`

**Credentials:** `session_jwt`, `scoped_uat`, `agent_jwt`

**Bearer:** `admit` (target `self_record`; boundaries `project`, `hub`)

**Base Permission:** `inbox.read`

**Resource Resolver:** self-principal

**Effects:** `list-scoped`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestNotificationToken_RowsFilteredToBoundary`
- `pkg/hub:TestNotificationsByAgent_OtherSubscriberRowsRequireAgentRead`

---

## inbox.notification.ack

**Domain:** inbox

**Description:** Acknowledge the caller's notifications. Ack-all by a project token touches only rows of its boundary project

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/notifications/ack-all` |
| http_route | POST | `/api/v1/notifications/{id}/ack` |

**Principals:** `user`, `agent`

**Credentials:** `session_jwt`, `scoped_uat`, `agent_jwt`

**Bearer:** `admit` (target `self_record`; boundaries `project`, `hub`)

**Base Permission:** `inbox.write`

**Resource Resolver:** self-principal

**Effects:** `update-resource`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestNotificationToken_RowsFilteredToBoundary`

---

## inbox.notification.subscription.create

**Domain:** inbox

**Description:** Create notification subscriptions. A user caller needs project:read on the project and agent:read on a watched agent; a token also needs inbox:write for the project

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/notifications/subscriptions` |
| http_route | POST | `/api/v1/notifications/subscriptions/bulk` |

**Principals:** `user`, `agent`

**Credentials:** `session_jwt`, `scoped_uat`, `agent_jwt`

**Bearer:** `admit` (target `project_body`; boundaries `project`, `hub`)

**Base Permission:** `inbox.write`

**Resource Resolver:** project-from-body

**Effects:** `create-resource`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestNotificationSubscription_RequiresProjectAndAgentRead`

---

## inbox.notification.subscription.read

**Domain:** inbox

**Description:** List the caller's notification subscriptions. A token lists only rows inside its boundary

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/notifications/subscriptions` |

**Principals:** `user`, `agent`

**Credentials:** `session_jwt`, `scoped_uat`, `agent_jwt`

**Bearer:** `admit` (target `self_record`; boundaries `project`, `hub`)

**Base Permission:** `inbox.read`

**Resource Resolver:** self-principal

**Effects:** `list-scoped`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestNotificationToken_RowsFilteredToBoundary`

---

## inbox.notification.subscription.write

**Domain:** inbox

**Description:** Update and delete the caller's notification subscriptions. A token changes only rows inside its boundary

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | PATCH | `/api/v1/notifications/subscriptions/{id}` |
| http_route | DELETE | `/api/v1/notifications/subscriptions/{id}` |
| http_route | POST | `/api/v1/notifications/subscriptions/bulk-delete` |

**Principals:** `user`, `agent`

**Credentials:** `session_jwt`, `scoped_uat`, `agent_jwt`

**Bearer:** `admit` (target `self_record`; boundaries `project`, `hub`)

**Base Permission:** `inbox.write`

**Resource Resolver:** self-principal

**Effects:** `update-resource`, `delete-resource`

### Audit

- **Event Type:** `inbox.notification.subscription.write`
- **Context Fields:** actor_id
- **Before Fields:** subscription_id
- **Atomic:** Yes

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestNotificationToken_RowsFilteredToBoundary`

---

## inbox.notification.template.create

**Domain:** inbox

**Description:** Create a subscription template. A template filed under a project needs project:read on it; a token also needs inbox:write for it

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/notifications/templates` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `project_body`; boundaries `project`, `hub`)

**Base Permission:** `inbox.write`

**Resource Resolver:** project-from-body

**Effects:** `create-resource`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestNotificationTemplates_ListedOnlyForReadableProjects`

---

## inbox.notification.template.read

**Domain:** inbox

**Description:** List subscription templates: only templates of projects the caller may read, and for a token only templates inside its boundary

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/notifications/templates` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `self_record`; boundaries `project`, `hub`)

**Base Permission:** `inbox.read`

**Resource Resolver:** self-principal

**Effects:** `list-scoped`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestNotificationTemplates_ListedOnlyForReadableProjects`

---

## inbox.notification.template.delete

**Domain:** inbox

**Description:** Delete a subscription template the caller created. A token needs inbox:write for the template's project

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | DELETE | `/api/v1/notifications/templates/{id}` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `self_record`; boundaries `project`, `hub`)

**Base Permission:** `inbox.write`

**Resource Resolver:** self-principal

**Effects:** `delete-resource`

### Audit

- **Event Type:** `inbox.notification.template.delete`
- **Context Fields:** actor_id
- **Before Fields:** template_id
- **Atomic:** Yes

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestNotificationTemplates_ListedOnlyForReadableProjects`

---

## project.conversation.gcsobject.read

**Domain:** project

**Description:** Fetch a gs:// object named in a message of a project group conversation, through the sending agent's assigned service account. Needs project:read on the conversation's project; a token needs it inside its boundary

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/gcs/object` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `conversation_record`; boundaries `project`, `hub`)

**Base Permission:** `project.read`

**Resource Resolver:** message-conversation-project

**Effects:** `read-one`

**Denial Codes:** `not_found`

### Tests

- `pkg/hub:TestGCSObjectToken_ProjectGroupNeedsProjectRead`

---

## web.events.stream

**Domain:** web

**Description:** Stream hub events to the web client. The web server admits only a signed-in web session; a token never opens the stream

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| sse | GET | `/events` |

**Principals:** `user`

**Credentials:** `session_jwt`

**Bearer:** `session_only` (reason `INTERACTIVE_STATE`)

**Resource Resolver:** event-subjects

**Effects:** `read-one`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestWebEventStream_RefusesUserAccessToken`

### Exemptions

- **authentication_only:** Each requested subject is authorized for the session user when the stream opens; no single resource permission applies (scope: web event stream) — waives: `base_permission`

---

## chat.prefs.read

**Domain:** chat

**Description:** Read the caller's visibility preference for an agent's chat thread. Needs agent:read on the agent; a token also needs inbox:read for the agent's project

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/chat/prefs` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `agent_record`; boundaries `project`, `hub`)

**Base Permission:** `inbox.read`

**Resource Resolver:** agent-from-query

**Effects:** `read-one`

**Denial Codes:** `forbidden`, `not_found`

### Tests

- `pkg/hub:TestChatPrefsToken_RequiresInboxSelectorAndAgentRead`

---

## chat.prefs.update

**Domain:** chat

**Description:** Write the caller's visibility preference for an agent's chat thread. Needs agent:read on the agent; a token also needs inbox:write for the agent's project

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | PUT | `/api/v1/chat/prefs` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `agent_record`; boundaries `project`, `hub`)

**Base Permission:** `inbox.write`

**Resource Resolver:** agent-from-query

**Effects:** `update-resource`

**Denial Codes:** `forbidden`, `not_found`

### Tests

- `pkg/hub:TestChatPrefsToken_RequiresInboxSelectorAndAgentRead`

---

## chat.space.list

**Domain:** chat

**Description:** List the chat spaces: the projects the caller can read. A project token lists only its boundary project; the caller's rail preferences need inbox:read on a hub boundary

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/chat/spaces` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `project_collection`; boundaries `project`, `hub`)

**Base Permission:** `project.read`

**Resource Resolver:** project-collection

**Effects:** `list-scoped`

**Denial Codes:** `forbidden`, `not_found`

### Tests

- `pkg/hub:TestChatSpacesToken_FilteredToBoundary`

---

## chat.unreadcount.read

**Domain:** chat

**Description:** Count the caller's conversations with unread messages. A token needs inbox:read; threads count only in projects it can read and DMs only where the DM rule passes

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/chat/unread-count` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `self_record`; boundaries `project`, `hub`)

**Base Permission:** `inbox.read`

**Resource Resolver:** self-principal

**Effects:** `read-one`

**Denial Codes:** `forbidden`, `not_found`

### Tests

- `pkg/hub:TestChatUnreadCountToken_FilteredToBoundary`

---

## chat.thread.list

**Domain:** chat

**Description:** List the threads of a chat space. Needs project:read on the space's project

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/chat/spaces/{id}/threads` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `project_path`; boundaries `project`, `hub`)

**Base Permission:** `project.read`

**Resource Resolver:** project-from-path

**Effects:** `list-scoped`

**Denial Codes:** `forbidden`, `not_found`

### Tests

- `pkg/hub:TestChatWriteToken_RequiresInboxWrite`

---

## chat.thread.create

**Domain:** chat

**Description:** Create a thread in a chat space. Needs project:read on the space's project; a token also needs inbox:write for it

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/chat/spaces/{id}/threads` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `project_path`; boundaries `project`, `hub`)

**Base Permission:** `inbox.write`

**Resource Resolver:** project-from-path

**Effects:** `create-resource`

**Denial Codes:** `forbidden`, `not_found`

### Tests

- `pkg/hub:TestChatWriteToken_RequiresInboxWrite`

---

## chat.space.members.read

**Domain:** chat

**Description:** List the members of a chat space. Needs project:read on the space's project; agent rows also need the agent list scope

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/chat/spaces/{id}/members` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `project_path`; boundaries `project`, `hub`)

**Base Permission:** `project.read`

**Resource Resolver:** project-from-path

**Effects:** `list-scoped`

**Denial Codes:** `forbidden`, `not_found`

### Tests

- `pkg/hub:TestChatWriteToken_RequiresInboxWrite`

---

## chat.space.markread

**Domain:** chat

**Description:** Mark every thread of a chat space read for the caller. Needs project:read on the space's project; a token also needs inbox:write for it

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/chat/spaces/{id}/read` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `project_path`; boundaries `project`, `hub`)

**Base Permission:** `inbox.write`

**Resource Resolver:** project-from-path

**Effects:** `update-resource`

**Denial Codes:** `forbidden`, `not_found`

### Tests

- `pkg/hub:TestChatWriteToken_RequiresInboxWrite`

---

## chat.space.emoji.update

**Domain:** chat

**Description:** Set or clear the emoji of a chat space. Needs project:update on the space's project

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | PUT | `/api/v1/chat/spaces/{id}/emoji` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `project_path`; boundaries `project`, `hub`)

**Base Permission:** `project.update`

**Resource Resolver:** project-from-path

**Effects:** `update-resource`

**Denial Codes:** `forbidden`, `not_found`

### Tests

- `pkg/hub:TestChatWriteToken_RequiresInboxWrite`

---

## chat.topic.read

**Domain:** chat

**Description:** Read a chat thread. Needs project:read on the thread's project

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/chat/topics/{id}` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `conversation_record`; boundaries `project`, `hub`)

**Base Permission:** `project.read`

**Resource Resolver:** conversation-project

**Effects:** `read-one`

**Denial Codes:** `forbidden`, `not_found`

### Tests

- `pkg/hub:TestChatWriteToken_RequiresInboxWrite`

---

## chat.topic.update

**Domain:** chat

**Description:** Rename a chat thread or set its default agent. Needs project:read on the thread's project; a token also needs inbox:write for it

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | PATCH | `/api/v1/chat/topics/{id}` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `conversation_record`; boundaries `project`, `hub`)

**Base Permission:** `inbox.write`

**Resource Resolver:** conversation-project

**Effects:** `update-resource`

**Denial Codes:** `forbidden`, `not_found`

### Tests

- `pkg/hub:TestChatWriteToken_RequiresInboxWrite`

---

## chat.topic.delete

**Domain:** chat

**Description:** Delete a chat thread (soft delete; the last thread of a space stays). Needs project:read on the thread's project; a token also needs inbox:write for it

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | DELETE | `/api/v1/chat/topics/{id}` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `conversation_record`; boundaries `project`, `hub`)

**Base Permission:** `inbox.write`

**Resource Resolver:** conversation-project

**Effects:** `update-resource`

**Denial Codes:** `forbidden`, `not_found`

### Tests

- `pkg/hub:TestChatWriteToken_RequiresInboxWrite`

---

## chat.conversation.history.topic

**Domain:** chat

**Description:** Read the messages of a chat thread. Needs project:read on the thread's project

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/chat/conversations/{id}/messages` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `conversation_record`; boundaries `project`, `hub`)

**Base Permission:** `project.read`

**Resource Resolver:** conversation-project

**Effects:** `read-one`, `list-scoped`

**Denial Codes:** `forbidden`, `not_found`

### Tests

- `pkg/hub:TestChatWriteToken_RequiresInboxWrite`

---

## chat.conversation.history.dm

**Domain:** chat

**Description:** Read the messages of one of the caller's DMs. A token needs inbox:read: a DM with an agent needs the agent's project inside its boundary and agent:read on the agent; a DM between users needs a hub boundary

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/chat/conversations/{id}/messages` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `self_record`; boundaries `project`, `hub`)

**Base Permission:** `inbox.read`

**Resource Resolver:** self-principal

**Effects:** `read-one`, `list-scoped`

**Denial Codes:** `forbidden`, `not_found`

### Tests

- `pkg/hub:TestChatDirectMessageToken_PeerAgentInsideBoundary`
- `pkg/hub:TestChatHumanDirectMessage_RequiresHubBoundary`

---

## chat.conversation.send.topic

**Domain:** chat

**Description:** Post a message to a chat thread. Needs project:read on the thread's project; a token also needs inbox:write for it. Agent recipients need agent:message

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/chat/conversations/{id}/messages` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `conversation_record`; boundaries `project`, `hub`)

**Base Permission:** `inbox.write`

**Resource Resolver:** conversation-project

**Effects:** `create-resource`, `emit-external-effect`

### Audit

- **Event Type:** `chat.conversation.send.topic`
- **Context Fields:** actor_id, conversation_key
- **After Fields:** message_id
- **Atomic:** No
- **Non-Atomic Justification:** The message is stored before agent dispatch, which is fire-and-forget

### External Effect Policy

- **Delivery:** `fire_and_forget`
- **Failure Mode:** `log_and_continue`
- **Idempotency:** message ID
- **Retry:** no retry for user-sent messages
- **Auth Before Emit:** Yes

**Denial Codes:** `forbidden`, `not_found`

### Tests

- `pkg/hub:TestChatWriteToken_RequiresInboxWrite`

---

## chat.conversation.send.dmagent

**Domain:** chat

**Description:** Post a message to one of the caller's DMs with an agent. Needs agent:message on the agent; a token also needs inbox:write for the agent's project

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/chat/conversations/{id}/messages` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `self_record`; boundaries `project`, `hub`)

**Base Permission:** `inbox.write`

**Resource Resolver:** self-principal

**Effects:** `create-resource`, `emit-external-effect`

### Audit

- **Event Type:** `chat.conversation.send.dmagent`
- **Context Fields:** actor_id, conversation_key
- **After Fields:** message_id
- **Atomic:** No
- **Non-Atomic Justification:** The message is stored before agent dispatch, which is fire-and-forget

### External Effect Policy

- **Delivery:** `fire_and_forget`
- **Failure Mode:** `log_and_continue`
- **Idempotency:** message ID
- **Retry:** no retry for user-sent messages
- **Auth Before Emit:** Yes

**Denial Codes:** `forbidden`, `not_found`

### Tests

- `pkg/hub:TestChatDirectMessageToken_PeerAgentInsideBoundary`

---

## chat.conversation.send.dmuser

**Domain:** chat

**Description:** Post a message to one of the caller's DMs with another user. Every caller needs read access to the peer user record; a token also needs inbox:write on a hub boundary

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/chat/conversations/{id}/messages` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `self_record`; boundaries `hub`)

**Base Permission:** `inbox.write`

**Resource Resolver:** self-principal

**Effects:** `create-resource`

**Denial Codes:** `forbidden`, `not_found`

### Tests

- `pkg/hub:TestChatHumanDirectMessage_RequiresHubBoundary`

---

## chat.message.edit

**Domain:** chat

**Description:** Edit one of the caller's chat messages. Only the author edits, and only before an agent replies. A token needs inbox:write, and the message's conversation is re-checked: project:read for a thread, the DM rule for a DM

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | PUT | `/api/v1/chat/conversations/{id}/messages/{messageId}` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `conversation_record`; boundaries `project`, `hub`)

**Base Permission:** `inbox.write`

**Resource Resolver:** conversation-project

**Effects:** `update-resource`

**Denial Codes:** `forbidden`, `not_found`

### Tests

- `pkg/hub:TestChatMessageEdit_RechecksTargetConversation`

---

## chat.message.delete

**Domain:** chat

**Description:** Delete one of the caller's chat messages (soft delete). Only the author deletes, and only before an agent replies. A token needs inbox:write, and the message's conversation is re-checked: project:read for a thread, the DM rule for a DM

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | DELETE | `/api/v1/chat/conversations/{id}/messages/{messageId}` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `conversation_record`; boundaries `project`, `hub`)

**Base Permission:** `inbox.write`

**Resource Resolver:** conversation-project

**Effects:** `update-resource`

**Denial Codes:** `forbidden`, `not_found`

### Tests

- `pkg/hub:TestChatMessageEdit_RechecksTargetConversation`

---

## chat.conversation.readstate.topic

**Domain:** chat

**Description:** Read the caller's read state on a chat thread. Needs project:read on the thread's project

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/chat/conversations/{id}/read` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `conversation_record`; boundaries `project`, `hub`)

**Base Permission:** `project.read`

**Resource Resolver:** conversation-project

**Effects:** `read-one`

**Denial Codes:** `forbidden`, `not_found`

### Tests

- `pkg/hub:TestChatWriteToken_RequiresInboxWrite`

---

## chat.conversation.readstate.dm

**Domain:** chat

**Description:** Read the caller's read state on one of its DMs. A token needs inbox:read under the DM rule

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/chat/conversations/{id}/read` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `self_record`; boundaries `project`, `hub`)

**Base Permission:** `inbox.read`

**Resource Resolver:** self-principal

**Effects:** `read-one`

**Denial Codes:** `forbidden`, `not_found`

### Tests

- `pkg/hub:TestChatDirectMessageToken_PeerAgentInsideBoundary`

---

## chat.conversation.markread.topic

**Domain:** chat

**Description:** Move the caller's read marker on a chat thread. Needs project:read on the thread's project; a token also needs inbox:write for it

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/chat/conversations/{id}/read` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `conversation_record`; boundaries `project`, `hub`)

**Base Permission:** `inbox.write`

**Resource Resolver:** conversation-project

**Effects:** `update-resource`

**Denial Codes:** `forbidden`, `not_found`

### Tests

- `pkg/hub:TestChatWriteToken_RequiresInboxWrite`

---

## chat.conversation.markread.dm

**Domain:** chat

**Description:** Move the caller's read marker on one of the caller's DMs. A token needs inbox:write under the DM rule

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/chat/conversations/{id}/read` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `self_record`; boundaries `project`, `hub`)

**Base Permission:** `inbox.write`

**Resource Resolver:** self-principal

**Effects:** `update-resource`

**Denial Codes:** `forbidden`, `not_found`

### Tests

- `pkg/hub:TestChatDirectMessageToken_PeerAgentInsideBoundary`

---

## chat.conversation.markunread.topic

**Domain:** chat

**Description:** Mark unread for the caller a chat thread. Needs project:read on the thread's project; a token also needs inbox:write for it

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/chat/conversations/{id}/unread` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `conversation_record`; boundaries `project`, `hub`)

**Base Permission:** `inbox.write`

**Resource Resolver:** conversation-project

**Effects:** `update-resource`

**Denial Codes:** `forbidden`, `not_found`

### Tests

- `pkg/hub:TestChatWriteToken_RequiresInboxWrite`

---

## chat.conversation.markunread.dm

**Domain:** chat

**Description:** Mark unread for the caller one of the caller's DMs. A token needs inbox:write under the DM rule

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/chat/conversations/{id}/unread` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `self_record`; boundaries `project`, `hub`)

**Base Permission:** `inbox.write`

**Resource Resolver:** self-principal

**Effects:** `update-resource`

**Denial Codes:** `forbidden`, `not_found`

### Tests

- `pkg/hub:TestChatDirectMessageToken_PeerAgentInsideBoundary`

---

## chat.conversation.mute.topic

**Domain:** chat

**Description:** Mute or unmute for the caller a chat thread. Needs project:read on the thread's project; a token also needs inbox:write for it

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | PUT | `/api/v1/chat/conversations/{id}/mute` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `conversation_record`; boundaries `project`, `hub`)

**Base Permission:** `inbox.write`

**Resource Resolver:** conversation-project

**Effects:** `update-resource`

**Denial Codes:** `forbidden`, `not_found`

### Tests

- `pkg/hub:TestChatWriteToken_RequiresInboxWrite`

---

## chat.conversation.mute.dm

**Domain:** chat

**Description:** Mute or unmute for the caller one of the caller's DMs. A token needs inbox:write under the DM rule

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | PUT | `/api/v1/chat/conversations/{id}/mute` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `self_record`; boundaries `project`, `hub`)

**Base Permission:** `inbox.write`

**Resource Resolver:** self-principal

**Effects:** `update-resource`

**Denial Codes:** `forbidden`, `not_found`

### Tests

- `pkg/hub:TestChatDirectMessageToken_PeerAgentInsideBoundary`

---

## chat.conversation.pin.topic

**Domain:** chat

**Description:** Pin or unpin for the caller a chat thread. Needs project:read on the thread's project; a token also needs inbox:write for it

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | PUT | `/api/v1/chat/conversations/{id}/pin` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `conversation_record`; boundaries `project`, `hub`)

**Base Permission:** `inbox.write`

**Resource Resolver:** conversation-project

**Effects:** `update-resource`

**Denial Codes:** `forbidden`, `not_found`

### Tests

- `pkg/hub:TestChatWriteToken_RequiresInboxWrite`

---

## chat.conversation.pin.dm

**Domain:** chat

**Description:** Pin or unpin for the caller one of the caller's DMs. A token needs inbox:write under the DM rule

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | PUT | `/api/v1/chat/conversations/{id}/pin` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `self_record`; boundaries `project`, `hub`)

**Base Permission:** `inbox.write`

**Resource Resolver:** self-principal

**Effects:** `update-resource`

**Denial Codes:** `forbidden`, `not_found`

### Tests

- `pkg/hub:TestChatDirectMessageToken_PeerAgentInsideBoundary`

---

## chat.conversation.typing.topic

**Domain:** chat

**Description:** Publish the caller's typing indicator on a chat thread. Needs project:read on the thread's project; a token also needs inbox:write for it

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/chat/conversations/{id}/typing` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `conversation_record`; boundaries `project`, `hub`)

**Base Permission:** `inbox.write`

**Resource Resolver:** conversation-project

**Effects:** `update-resource`

**Denial Codes:** `forbidden`, `not_found`

### Tests

- `pkg/hub:TestChatWriteToken_RequiresInboxWrite`

---

## chat.conversation.typing.dm

**Domain:** chat

**Description:** Publish the caller's typing indicator on one of the caller's DMs. A token needs inbox:write under the DM rule

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/chat/conversations/{id}/typing` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `self_record`; boundaries `project`, `hub`)

**Base Permission:** `inbox.write`

**Resource Resolver:** self-principal

**Effects:** `update-resource`

**Denial Codes:** `forbidden`, `not_found`

### Tests

- `pkg/hub:TestChatDirectMessageToken_PeerAgentInsideBoundary`

---

## chat.conversation.interagent.read

**Domain:** chat

**Description:** Read the messages an agent in one of the caller's DMs exchanged with other agents. Needs DM participation and agent:attach on the agent

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/chat/conversations/{id}/interagent` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `agent_record`; boundaries `project`, `hub`)

**Base Permission:** `agent.attach`

**Resource Resolver:** agent-from-dm-key

**Effects:** `read-one`, `list-scoped`

**Denial Codes:** `forbidden`, `not_found`

### Tests

- `pkg/hub:TestChatDirectMessageToken_PeerAgentInsideBoundary`

---

## chat.dm.list

**Domain:** chat

**Description:** List the caller's DMs. A token needs inbox:read and lists only the DMs the DM rule lets it read

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/chat/dms` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `self_record`; boundaries `project`, `hub`)

**Base Permission:** `inbox.read`

**Resource Resolver:** self-principal

**Effects:** `list-scoped`

**Denial Codes:** `forbidden`, `not_found`

### Tests

- `pkg/hub:TestChatDMListToken_FilteredToBoundary`

---

## chat.userprefs.read

**Domain:** chat

**Description:** Read the caller's chat rail preferences, a project-less record. A token needs inbox:read on a hub boundary

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/chat/user-prefs` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `self_record`; boundaries `hub`)

**Base Permission:** `inbox.read`

**Resource Resolver:** self-principal

**Effects:** `read-one`

**Denial Codes:** `forbidden`, `not_found`

### Tests

- `pkg/hub:TestChatUserPrefsToken_RequiresHubBoundary`

---

## chat.userprefs.update

**Domain:** chat

**Description:** Write the caller's chat rail preferences, a project-less record. A federated caller is refused. A token needs inbox:write on a hub boundary

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | PUT | `/api/v1/chat/user-prefs` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `self_record`; boundaries `hub`)

**Base Permission:** `inbox.write`

**Resource Resolver:** self-principal

**Effects:** `update-resource`

**Denial Codes:** `forbidden`, `not_found`

### Tests

- `pkg/hub:TestChatUserPrefsToken_RequiresHubBoundary`
- `pkg/hub:TestChatProfileWrites_FederatedUserRefused`

---

## chat.presence.update

**Domain:** chat

**Description:** Publish the caller's presence. It fans out only on the named projects the caller can read (project:read), each once, at most 1000; the others are left out. A federated caller is refused. A token also needs inbox:write

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/chat/presence` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `project_body`; boundaries `project`, `hub`)

**Base Permission:** `inbox.write`

**Resource Resolver:** project-from-body

**Effects:** `update-resource`

**Denial Codes:** `forbidden`, `not_found`

### Tests

- `pkg/hub:TestChatPresence_EveryProjectRequiresProjectRead`
- `pkg/hub:TestChatProfileWrites_FederatedUserRefused`

---

## chat.search.topic

**Domain:** chat

**Description:** Search the messages of one chat thread. Needs project:read on the thread's project

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/chat/search` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `conversation_record`; boundaries `project`, `hub`)

**Base Permission:** `project.read`

**Resource Resolver:** conversation-project

**Effects:** `list-scoped`

**Denial Codes:** `forbidden`, `not_found`

### Tests

- `pkg/hub:TestChatSearchToken_DMRowsNeedInboxRead`

---

## chat.search.project

**Domain:** chat

**Description:** Search the messages of one project's chat. Needs project:read on the project; a token keeps a DM row only when the DM rule lets it read the DM under inbox:read

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/chat/search` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `project_query`; boundaries `project`, `hub`)

**Base Permission:** `project.read`

**Resource Resolver:** project-from-query

**Effects:** `list-scoped`

**Denial Codes:** `forbidden`, `not_found`

### Tests

- `pkg/hub:TestChatSearchToken_DMRowsNeedInboxRead`

---

## chat.search.unscoped

**Domain:** chat

**Description:** Search the messages of every project the caller can read. A project token searches only its boundary project; a token keeps a DM row only when the DM rule lets it read the DM under inbox:read

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/chat/search` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `project_collection`; boundaries `project`, `hub`)

**Base Permission:** `project.read`

**Resource Resolver:** project-collection

**Effects:** `list-scoped`

**Denial Codes:** `forbidden`, `not_found`

### Tests

- `pkg/hub:TestChatSearchToken_DMRowsNeedInboxRead`

---

## chat.search.dm

**Domain:** chat

**Description:** Search the messages of one of the caller's DMs. A token needs inbox:read under the DM rule

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/chat/search` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `self_record`; boundaries `project`, `hub`)

**Base Permission:** `inbox.read`

**Resource Resolver:** self-principal

**Effects:** `list-scoped`

**Denial Codes:** `forbidden`, `not_found`

### Tests

- `pkg/hub:TestChatSearchToken_DMRowsNeedInboxRead`

---

## chat.attachment.upload.project

**Domain:** chat

**Description:** Upload chat attachments to a space. Needs project:read on the space's project; a token also needs inbox:write for it

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/chat/attachments` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `project_body`; boundaries `project`, `hub`)

**Base Permission:** `inbox.write`

**Resource Resolver:** project-from-body

**Effects:** `create-resource`

**Denial Codes:** `forbidden`, `not_found`

### Tests

- `pkg/hub:TestChatProjectUpload_RequiresInboxWriteAndProjectRead`

---

## chat.attachment.upload.noproject

**Domain:** chat

**Description:** Upload project-less chat attachments, reachable only through the messages that reference them. A federated caller is refused. A token needs inbox:write on a hub boundary

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/chat/attachments` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `self_record`; boundaries `hub`)

**Base Permission:** `inbox.write`

**Resource Resolver:** self-principal

**Effects:** `create-resource`

**Denial Codes:** `forbidden`, `not_found`

### Tests

- `pkg/hub:TestChatProjectlessUpload_RequiresHubBoundary`
- `pkg/hub:TestChatProfileWrites_FederatedUserRefused`

---

## chat.attachment.download.project

**Domain:** chat

**Description:** Download a chat attachment of a project. Needs project:read on the project; a session also reads its own uploads and files attached to messages it can read

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/chat/attachments/{id}` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `project_path`; boundaries `project`, `hub`)

**Base Permission:** `project.read`

**Resource Resolver:** attachment-project

**Effects:** `read-one`

**Denial Codes:** `forbidden`, `not_found`

### Tests

- `pkg/hub:TestChatAttachmentDownloadToken_ProjectFileNeedsProjectRead`

---

## chat.conversation.promote

**Domain:** chat

**Description:** Promote one of the caller's DMs with an agent into a thread of the agent's space. Needs DM participation and project:read on the agent's project; a token also needs inbox:write under the DM rule

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/chat/conversations/{id}/promote` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `conversation_record`; boundaries `project`, `hub`)

**Base Permission:** `inbox.write`

**Resource Resolver:** conversation-project

**Effects:** `create-resource`, `update-resource`

**Denial Codes:** `forbidden`, `not_found`

### Tests

- `pkg/hub:TestChatDirectMessageToken_PeerAgentInsideBoundary`

---

## role.definition.create

**Domain:** role

**Description:** Create a custom role definition (create, import, duplicate). A token needs role:create and role:read on a hub boundary and the holder's live role.create authority; the holder must hold every permission of the role within the token's selectors. A project-scoped definition can be bound in every project, so it is judged on the holder's system-scope authority; a system-scoped definition is refused to every token

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/admin/roles` |
| http_route | POST | `/api/v1/admin/roles/import` |
| http_route | POST | `/api/v1/admin/roles/{id}/duplicate` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `hub_instance`; boundaries `hub`)

**Base Permission:** `role.create`

**Resource Resolver:** hub-scoped

**Effects:** `create-resource`

### Audit

- **Event Type:** `role.definition.create`
- **Context Fields:** actor_id
- **After Fields:** role_name, permissions
- **Atomic:** Yes

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`
- `pkg/hub:TestCustomRoleCreateToken_ProjectScopeJudgedOnHubAuthority`
- `pkg/hub:TestRoleDefinitionWriteToken_SelectorAndBoundaryPerRoute`

### Exemptions

- **authentication_only:** audit obligation waived; full operation contract deferred to AH1 (scope: AF1 catalog only) — waives: `audit_obligation`

---

## role.definition.update

**Domain:** role

**Description:** Update a custom role definition. A token needs role:update and role:read on a hub boundary and the holder's live role.update authority; the holder must hold every permission of the updated role within the token's selectors, judged as for create

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | PUT | `/api/v1/admin/roles/{id}` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `hub_instance`; boundaries `hub`)

**Base Permission:** `role.update`

**Resource Resolver:** hub-scoped

**Effects:** `update-resource`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`
- `pkg/hub:TestRoleDefinitionWriteToken_SelectorAndBoundaryPerRoute`

---

## role.definition.delete

**Domain:** role

**Description:** Delete a custom role definition. A token needs role:delete and role:read on a hub boundary and the holder's live role.delete authority; system roles and roles that are still bound are kept

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | DELETE | `/api/v1/admin/roles/{id}` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `hub_instance`; boundaries `hub`)

**Base Permission:** `role.delete`

**Resource Resolver:** hub-scoped

**Effects:** `delete-resource`

### Audit

- **Event Type:** `role.definition.delete`
- **Context Fields:** actor_id
- **Before Fields:** role_name
- **Atomic:** Yes

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`
- `pkg/hub:TestRoleDefinitionWriteToken_SelectorAndBoundaryPerRoute`

---

## role.binding.create

**Domain:** role.binding

**Description:** Create a role binding (grant authority to a principal) for a custom project role or at system scope. A token needs role_binding:create on a hub boundary and the holder's live role_binding.create authority. A system-scoped binding is refused to every token. For a custom project role the holder must also have current project.manage access to the binding's project and hold every permission of the role in that project within the token's selectors

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/admin/role-bindings` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `hub_instance`; boundaries `hub`)

**Base Permission:** `role_binding.create`

**Resource Resolver:** hub-scoped

**Effects:** `grant-authority`

### Delegation

- **Kind:** `non_amplification`
- Actor must hold all permissions in the bound role (CanDelegate)

### Audit

- **Event Type:** `role_binding_create`
- **Context Fields:** actor_id
- **After Fields:** principal_id, role_name, scope
- **Atomic:** Yes

**Denial Codes:** `forbidden`, `role_assignment_forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`
- `pkg/hub:TestRoleBindingCreateToken_SystemScopeAlwaysDenied`
- `pkg/hub:TestCustomRoleBindingToken_NeedsCreateSelectorAndProjectAccess`

---

## role.binding.delete

**Domain:** role.binding

**Description:** Delete a system-scoped role binding (revoke authority from a principal). A token needs role_binding:delete on a hub boundary and the holder's live role_binding.delete authority; the constraint-admin lockout guard applies. Deleting a super-admin binding is refused to every token (session only)

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | DELETE | `/api/v1/admin/role-bindings/{id}` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `hub_instance`; boundaries `hub`)

**Base Permission:** `role_binding.delete`

**Resource Resolver:** hub-scoped

**Effects:** `revoke-authority`

**Authority Evaluation:** `proposed_post_state`

### Governance

- **Kind:** peer_superior
- Revoking authority from a peer or superior principal requires governance review

### Audit

- **Event Type:** `role_binding_delete`
- **Context Fields:** actor_id
- **Before Fields:** principal_id, role_name, scope
- **Atomic:** Yes

**Denial Codes:** `forbidden`, `role_assignment_forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`
- `pkg/hub:TestRoleBindingDeleteToken_SuperAdminBindingDenied`
- `pkg/hub:TestRoleBindingDeleteToken_SystemBindingNeedsDeleteSelector`

---

## group.member.add

**Domain:** group

**Description:** Add a member to a group. A user access token needs group:addMember on a hub boundary, and the group's closure may carry no authority beyond the token's own

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/groups/{id}/members` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `group_record`; boundaries `hub`)

**Base Permission:** `group.addMember`

**Resource Resolver:** group-from-url

**Effects:** `grant-authority`

### Delegation

- **Kind:** `non_amplification`
- Adding a member to a role-bearing group effectively grants authority; actor must hold the group's role permissions. A user access token is refused when the closure carries a system-scoped binding, and must hold, in each project the closure binds, every permission those bindings carry

### Audit

- **Event Type:** `group_member_add`
- **Context Fields:** actor_id, group_id
- **After Fields:** member_principal_id
- **Atomic:** Yes

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`
- `pkg/hub:TestGroupMemberAddToken_ProjectBindingsOnClosureRequireCallerAuthority`
- `pkg/hub:TestGroupMemberAddToken_SystemBindingsOnClosureDenied`
- `pkg/hub:TestGroupChangeToken_SelectorAndBoundaryPerRoute`

---

## group.member.remove

**Domain:** group

**Description:** Remove a member from a group

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | DELETE | `/api/v1/groups/{id}/members/{memberType}/{memberId}` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `group_record`; boundaries `hub`)

**Base Permission:** `group.removeMember`

**Resource Resolver:** group-from-url

**Effects:** `revoke-authority`

### Governance

- **Kind:** peer_superior
- Removing from a constraint-bearing group may change effective authority; governed by group role hierarchy

### Audit

- **Event Type:** `group_member_remove`
- **Context Fields:** actor_id, group_id
- **Before Fields:** member_principal_id
- **Atomic:** Yes

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`
- `pkg/hub:TestGroupChangeToken_SelectorAndBoundaryPerRoute`

---

## group.delete

**Domain:** group

**Description:** Delete a group

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | DELETE | `/api/v1/groups/{id}` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `group_record`; boundaries `hub`)

**Base Permission:** `group.delete`

**Resource Resolver:** group-from-url

**Effects:** `delete-resource`

### Audit

- **Event Type:** `group_delete`
- **Context Fields:** actor_id
- **Before Fields:** group_id, group_name
- **Atomic:** Yes

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`
- `pkg/hub:TestGroupChangeToken_SelectorAndBoundaryPerRoute`

---

## access.constraint.create

**Domain:** access.constraint

**Description:** Create an access constraint (tighten boundary). A token needs access_constraint:admin and access_constraint:read on a hub boundary and the holder's live access_constraint.admin authority; the preview token and governance checks apply, and the commit re-checks judge the holder's authority within the token's selectors

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/admin/access-constraints` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `hub_instance`; boundaries `hub`)

**Base Permission:** `access_constraint.admin`

**Resource Resolver:** hub-scoped

**Effects:** `tighten-boundary`

**Authority Evaluation:** `before_and_after`

### Governance

- **Kind:** constraint_admin
- Constraint creation requires constraint admin authority

### Audit

- **Event Type:** `access.constraint.create`
- **Context Fields:** actor_id
- **Before Fields:** effective_authority_before
- **After Fields:** constraint_id, constraint_type, target_scope
- **Atomic:** Yes

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`
- `pkg/hub:TestConstraintAdminToken_RequiresConstraintAdminSelector`
- `pkg/hub:TestConstraintCommit_AppliesTokenSelectors`

---

## access.constraint.update

**Domain:** access.constraint

**Description:** Update an access constraint (may relax or tighten boundary). A token needs access_constraint:admin and access_constraint:read on a hub boundary and the holder's live access_constraint.admin authority, and may only tighten: an update that relaxes the boundary (adds permissions, changes subject or scope, or narrows or adds its time window) is session only. The commit re-checks judge the holder's authority within the token's selectors

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | PUT | `/api/v1/admin/access-constraints/{id}` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `hub_instance`; boundaries `hub`)

**Base Permission:** `access_constraint.admin`

**Resource Resolver:** hub-scoped

**Effects:** `relax-boundary`, `tighten-boundary`

**Authority Evaluation:** `before_and_after`

### Governance

- **Kind:** constraint_admin
- Constraint modification requires constraint admin authority; relaxation has higher governance bar

### Audit

- **Event Type:** `access.constraint.update`
- **Context Fields:** actor_id
- **Before Fields:** constraint_id, old_scope
- **After Fields:** new_scope
- **Atomic:** Yes

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`
- `pkg/hub:TestConstraintAdminToken_RequiresConstraintAdminSelector`
- `pkg/hub:TestConstraintRelaxToken_SessionOnly`
- `pkg/hub:TestConstraintCommit_AppliesTokenSelectors`

---

## access.constraint.delete

**Domain:** access.constraint

**Description:** Delete an access constraint (relax boundary). Session only: deleting a boundary relaxes it, and a user access token is refused before the preview token is used

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | DELETE | `/api/v1/admin/access-constraints/{id}` |

**Principals:** `user`

**Credentials:** `session_jwt`

**Bearer:** `session_only` (reason `GOV_PENDING`)

**Base Permission:** `access_constraint.admin`

**Resource Resolver:** hub-scoped

**Effects:** `relax-boundary`

**Authority Evaluation:** `before_and_after`

### Governance

- **Kind:** constraint_admin
- Constraint deletion relaxes boundary and requires constraint admin authority

### Audit

- **Event Type:** `access.constraint.delete`
- **Context Fields:** actor_id
- **Before Fields:** constraint_id, constraint_type, target_scope
- **After Fields:** effective_authority_after
- **Atomic:** Yes

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`
- `pkg/hub:TestConstraintRelaxToken_SessionOnly`

---

## access.constraint.preview

**Domain:** access.constraint

**Description:** Generate an access constraint preview and read a preview job and its result. A token needs access_constraint:admin on a hub boundary and the holder's live access_constraint.admin authority

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/admin/access-constraint-previews` |
| http_route | GET | `/api/v1/admin/access-constraint-previews/{jobId}` |
| http_route | GET | `/api/v1/admin/access-constraint-previews/{jobId}/result` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `hub_instance`; boundaries `hub`)

**Base Permission:** `access_constraint.admin`

**Resource Resolver:** hub-scoped

**Effects:** `read-one`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`
- `pkg/hub:TestConstraintAdminToken_RequiresConstraintAdminSelector`

---

## credential.token.read

**Domain:** credential

**Description:** List or read the caller's own user access tokens

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/auth/tokens` |
| http_route | GET | `/api/v1/auth/tokens/{id}` |

**Principals:** `user`

**Credentials:** `session_jwt`

**Bearer:** `session_only` (reason `CREDENTIAL_MANAGEMENT`)

**Base Permission:** `user.read`

**Resource Resolver:** self-principal

**Effects:** `list-scoped`, `read-one`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`
- `pkg/hub:TestSessionOnlyGate_ReasonIsReported`

### Exemptions

- **authentication_only:** Token reads are authenticated-only (user reads own tokens); no per-resource permission required beyond session validity (scope: self-token management only) — waives: `base_permission`

---

## credential.token.create

**Domain:** credential

**Description:** Create a user access token (UAT)

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/auth/tokens` |

**Principals:** `user`

**Credentials:** `session_jwt`

**Bearer:** `session_only` (reason `CREDENTIAL_MANAGEMENT`)

**Base Permission:** `user.read`

**Resource Resolver:** self-principal

**Effects:** `mint-credential`

### Governance

- **Kind:** issuer_credential
- User mints tokens for self; token scopes cannot exceed session authority

### Audit

- **Event Type:** `credential.token.create`
- **Context Fields:** actor_id
- **After Fields:** token_id, scopes
- **Atomic:** Yes

**Denial Codes:** `forbidden`, `scope_violation`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`
- `pkg/hub:TestRS4_IssuerAuthority`
- `pkg/hub:TestRS4_TargetScope`
- `pkg/hub:TestRS4_Audit_Mint`

### Exemptions

- **authentication_only:** Token creation is authenticated-only (user manages own tokens); no per-resource permission required beyond session validity (scope: self-token management only) — waives: `base_permission`

---

## credential.token.revoke

**Domain:** credential

**Description:** Revoke or delete a user access token

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/auth/tokens/{id}/revoke` |
| http_route | DELETE | `/api/v1/auth/tokens/{id}` |

**Principals:** `user`

**Credentials:** `session_jwt`

**Bearer:** `session_only` (reason `CREDENTIAL_MANAGEMENT`)

**Base Permission:** `user.read`

**Resource Resolver:** self-principal

**Effects:** `revoke-authority`

### Governance

- **Kind:** issuer_credential
- User may revoke own tokens; admin may revoke via hub-admin path

### Audit

- **Event Type:** `credential.token.revoke`
- **Context Fields:** actor_id
- **Before Fields:** token_id, action
- **Atomic:** Yes

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`
- `pkg/hub:TestRS4_Audit_Revoke`
- `pkg/hub:TestRS4_Audit_Delete`

### Exemptions

- **authentication_only:** Token revocation is authenticated-only (user manages own tokens) (scope: self-token management only) — waives: `base_permission`

---

## federation.token.mint

**Domain:** federation

**Description:** Obtain a short-lived user access credential for the caller, signed with the hub's OIDC key, for one peer hub audience listed in server.oidc.user_token_audiences. Only a local human user with status active; a user access token needs federation_token:mint on a hub boundary. 404 while federation.project_visitors is off

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/auth/federation-token` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `self_record`; boundaries `hub`)

**Base Permission:** `federation_token.mint`

**Resource Resolver:** self-principal

**Effects:** `mint-credential`

### Governance

- **Kind:** issuer_credential
- A user obtains a credential for themselves only, for an audience the hub admin allows, with a lifetime capped at one hour; it carries no project or agent claims

### Audit

- **Event Type:** `federation_token_issued`
- **Context Fields:** actor_id, credential_kind
- **After Fields:** audience, jti, expires_at
- **Atomic:** Yes

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`
- `pkg/hub:TestFederationToken_RefusedCallers`
- `pkg/hub:TestFederationToken_UserAccessTokenNeedsMintScope`
- `pkg/hub:TestFederationToken_AudienceAllowList`
- `pkg/hub:TestFederationToken_AuditWritten`
- `pkg/hub:TestFederationToken_AuditFailureIssuesNothing`
- `pkg/hub:TestFederationToken_RefusedCredentialKinds`

---

## user.admin.suspend

**Domain:** user.admin

**Description:** Suspend or reactivate a user account (dispatched from PATCH /api/v1/users/{id} when status field is present). A token needs user:suspend on a hub boundary, checked on the hub-level user record. The caller's own status and activating an invited user are refused for every credential

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| internal_dispatch | — | `updateUser:status-field` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `hub_instance`; boundaries `hub`; pinned by `TestUserPatchToken_StatusRequiresSuspendSelector`)

**Base Permission:** `user.suspend`

**Resource Resolver:** user-from-url

**Effects:** `change-principal-status`

### Audit

- **Event Type:** `user.admin.suspend`
- **Context Fields:** actor_id
- **Before Fields:** target_user_id, old_status
- **After Fields:** new_status
- **Atomic:** Yes

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`
- `pkg/hub:TestUserPatchToken_StatusRequiresSuspendSelector`
- `pkg/hub:TestUserPatchToken_SelfStatusGuardStillApplies`

---

## user.admin.invite

**Domain:** user.admin

**Description:** Invite users to the platform (single or bulk) and manage invitation codes (list, create, read stats, read, revoke, delete). The invited record has status invited, the placeholder role member and no role binding or group membership; an invitation code names no role. A token needs user:invite on the hub boundary and the holder's live user.invite authority

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/admin/users/invite` |
| http_route | POST | `/api/v1/admin/users/invite/bulk` |
| http_route | GET | `/api/v1/admin/invites` |
| http_route | POST | `/api/v1/admin/invites` |
| http_route | GET | `/api/v1/admin/invites/stats` |
| http_route | GET | `/api/v1/admin/invites/{id}` |
| http_route | POST | `/api/v1/admin/invites/{id}/revoke` |
| http_route | DELETE | `/api/v1/admin/invites/{id}` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `hub_instance`; boundaries `hub`)

**Base Permission:** `user.invite`

**Resource Resolver:** hub-scoped

**Effects:** `issue-credential`

### Governance

- **Kind:** issuer_credential
- Invitation issues a credential granting platform access

### Audit

- **Event Type:** `user.admin.invite`
- **Context Fields:** actor_id
- **After Fields:** invite_email, invite_id
- **Atomic:** Yes

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`
- `pkg/hub:TestInviteToken_RequiresInviteSelectorAndHubBoundary`
- `pkg/hub:TestInvitationToken_CreatesNoRoleBindingOrGroupMembership`

---

## user.admin.provision

**Domain:** user.admin

**Description:** Pre-register a user (status invited) through POST /api/v1/users; invitation-equivalent, shares the invite creation core; no role, no grants

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/users` |

**Principals:** `user`

**Credentials:** `session_jwt`

**Bearer:** `session_only` (reason `GOV_PENDING`)

**Base Permission:** `user.invite`

**Resource Resolver:** hub-scoped

**Effects:** `create-resource`, `issue-credential`

### Governance

- **Kind:** issuer_credential
- Pre-registration admits sign-in under invite_only, identical to user.admin.invite

### Audit

- **Event Type:** `user.admin.provision`
- **Context Fields:** actor_id, credential_id, credential_kind
- **After Fields:** target_user_id, email, status, display_name
- **Atomic:** Yes

**Denial Codes:** `forbidden`, `user_suspended`, `conflict`, `role_assignment_forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`
- `pkg/hub:TestHandleProvisionUser`

---

## user.admin.promote

**Domain:** user.admin

**Description:** Promote or demote a user's administrative level (dispatched from PATCH /api/v1/users/{id} when role field is present)

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| internal_dispatch | — | `updateUser:role-field` |

**Principals:** `user`

**Credentials:** `session_jwt`

**Bearer:** `session_only` (reason `GOV_PENDING`)

**Base Permission:** `user.promote`

**Resource Resolver:** user-from-url

**Effects:** `change-authority`

### Delegation

- **Kind:** `conditional_on_increase`
- Promotion delegation checked only when effective authority increases

**Authority Evaluation:** `before_and_after`

### Audit

- **Event Type:** `user.admin.promote`
- **Context Fields:** actor_id
- **Before Fields:** target_user_id, old_level
- **After Fields:** new_level
- **Atomic:** Yes

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`

---

## user.admin.delete

**Domain:** user.admin

**Description:** Delete a user account

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | DELETE | `/api/v1/users/{id}` |

**Principals:** `user`

**Credentials:** `session_jwt`

**Bearer:** `session_only` (reason `GOV_PENDING`)

**Base Permission:** `user.delete`

**Resource Resolver:** user-from-url

**Effects:** `delete-resource`

### Audit

- **Event Type:** `user.admin.delete`
- **Context Fields:** actor_id
- **Before Fields:** target_user_id, email, role, status
- **Atomic:** Yes

**Denial Codes:** `forbidden`, `last_owner`, `conflict`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`

---

## group.read

**Domain:** group

**Description:** Read group details

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/groups/{id}` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Base Permission:** `group.read`

**Resource Resolver:** group-from-url

**Effects:** `read-one`, `list-scoped`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`

---

## group.list

**Domain:** group

**Description:** List groups. Each row is shown only when the caller may read it (group.read); a user access token needs group:read on a hub boundary, and a group with no project resolves to the hub

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/groups` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `group_record`; boundaries `hub`)

**Base Permission:** `group.read`

**Resource Resolver:** group-per-row

**Effects:** `list-scoped`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`
- `pkg/hub:TestExplicitGroupToken_ResolvesHubTarget`

---

## group.create

**Domain:** group

**Description:** Create a new group. A user access token needs group:create on a hub boundary; the target is the hub's group collection. Creating under a parent group runs the member-add delegation check on the parent

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/groups` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `hub_collection`; boundaries `hub`)

**Base Permission:** `group.create`

**Resource Resolver:** hub-scoped

**Effects:** `create-resource`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`
- `pkg/hub:TestGroupCreateToken_RequiresCreateSelectorOnHubBoundary`

---

## group.update

**Domain:** group

**Description:** Update group metadata. A user access token that changes the owner needs group:update and live update access on the group, and for the new owner it must pass the delegation check of a member add on the group's role bindings; group:addMember and the group role hierarchy do not apply. A refused owner change returns one fixed 403, including for a group that does not exist

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | PATCH | `/api/v1/groups/{id}` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `group_record`; boundaries `hub`)

**Base Permission:** `group.update`

**Resource Resolver:** group-from-url

**Effects:** `update-resource`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`
- `pkg/hub:TestGroupChangeToken_SelectorAndBoundaryPerRoute`
- `pkg/hub:TestGroupOwnerChangeToken_RequiresDelegationForNewOwner`

---

## user.read

**Domain:** user

**Description:** Read a user profile

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/users/{id}` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Base Permission:** `user.read`

**Resource Resolver:** user-from-url

**Effects:** `read-one`, `list-scoped`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`

---

## user.list

**Domain:** user

**Description:** List users. A session sees the users it may read. A token needs user:list at the hub collection on a hub boundary, and then sees only the users it may read, each checked as a hub-level user record, so it also needs user:read

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/users` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `hub_collection`; boundaries `hub`)

**Base Permission:** `user.list`

**Resource Resolver:** hub-scoped

**Effects:** `list-scoped`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestUserListToken_RequiresUserListAndHubBoundary`

---

## user.update

**Domain:** user

**Description:** Update another user's profile (display name and preferences). The same PATCH dispatches user.admin.suspend for a status field and user.admin.promote for a role field, and every field's check passes before any write. A token needs user:update on a hub boundary, checked on the hub-level user record; a token PATCH that names no status field is checked as a profile update

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | PATCH | `/api/v1/users/{id}` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `hub_instance`; boundaries `hub`)

**Base Permission:** `user.update`

**Resource Resolver:** user-from-url

**Effects:** `update-resource`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`
- `pkg/hub:TestUserPatchToken_CrossUserProfileRequiresUpdateSelector`
- `pkg/hub:TestUserPatchToken_MixedFieldsAllOrNothing`

---

## user.self.update

**Domain:** user

**Description:** Update the caller's own profile (display name and preferences) through PATCH /api/v1/users/{id} naming the caller's own record. A status field on the caller's own record is refused by the self-status guard

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | PATCH | `/api/v1/users/{id}` |

**Principals:** `user`

**Credentials:** `session_jwt`

**Bearer:** `session_only` (reason `INTERACTIVE_STATE`)

**Resource Resolver:** self-principal

**Effects:** `update-resource`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`
- `pkg/hub:TestUserPatchToken_SelfProfileSessionOnly`

### Exemptions

- **authentication_only:** The path names the caller's own record, so no resource permission applies to the caller's own profile (scope: caller's own profile fields) — waives: `base_permission`

---

## role.read

**Domain:** role

**Description:** Read role definitions and the permission registry. A token needs role:read on a hub boundary; the write methods on the same routes also need their own selector

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/admin/roles` |
| http_route | GET | `/api/v1/admin/roles/{id}` |
| http_route | GET | `/api/v1/admin/roles/export` |
| http_route | HEAD | `/api/v1/admin/roles/export` |
| http_route | GET | `/api/v1/admin/roles/{id}/export` |
| http_route | HEAD | `/api/v1/admin/roles/{id}/export` |
| http_route | GET | `/api/v1/admin/permissions` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `hub_instance`; boundaries `hub`)

**Base Permission:** `role.read`

**Resource Resolver:** hub-scoped

**Effects:** `read-one`, `list-scoped`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`
- `pkg/hub:TestRoleReadToken_DoesNotAdmitRoleWrites`
- `pkg/hub:TestRoleRoutesToken_WriteMethodsNeedWriteSelectors`

---

## role.binding.read

**Domain:** role.binding

**Description:** Read role binding assignments

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/admin/role-bindings` |
| http_route | GET | `/api/v1/admin/role-bindings/user/{userId}` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `hub_instance`; boundaries `hub`)

**Base Permission:** `role_binding.read`

**Resource Resolver:** hub-scoped

**Effects:** `read-one`, `list-scoped`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`
- `pkg/hub:TestRoleReadToken_DoesNotAdmitRoleWrites`

---

## access.constraint.read

**Domain:** access.constraint

**Description:** Read access constraint definitions and the principals a constraint affects. A token needs access_constraint:read on a hub boundary; the write methods on the same routes also need access_constraint:admin

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/admin/access-constraints` |
| http_route | GET | `/api/v1/admin/access-constraints/{id}` |
| http_route | GET | `/api/v1/admin/access-constraints/{id}/affected-principals` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `hub_instance`; boundaries `hub`)

**Base Permission:** `access_constraint.read`

**Resource Resolver:** hub-scoped

**Effects:** `read-one`, `list-scoped`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`
- `pkg/hub:TestConstraintReadToken_DoesNotAdmitConstraintAdmin`
- `pkg/hub:TestConstraintWriteRoutesToken_WriteMethodsNeedAdminSelector`

---

## hub.audit.constraint

**Domain:** hub

**Description:** Read one access constraint's audit history. hub.audit.read is a hub-level permission: a token needs hub_audit:read on a hub boundary and is checked against the hub-level constraint record; a denied caller gets the same not-found answer as for a missing constraint

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/admin/access-constraints/{id}/audit` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `hub_instance`; boundaries `hub`)

**Base Permission:** `hub.audit.read`

**Resource Resolver:** hub-scoped

**Effects:** `read-one`

**Denial Codes:** `not_found`

### Tests

- `pkg/hub:TestConstraintAuditToken_RequiresHubAuditReadOnHubBoundary`

---

## user.session.logout

**Domain:** user

**Description:** Sign-in flow logout step; the hub holds no server-side session state for it to change

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/auth/logout` |

**Principals:** `user`

**Credentials:** `session_jwt`

**Bearer:** `non_user`

**Resource Resolver:** none

**Effects:** `update-resource`

### Tests

- `pkg/hub:TestBearerDisposition_EveryRoutePatternCovered`

### Exemptions

- **authentication_only:** Sign-in flow step that reads and changes no hub state; no resource permission applies (scope: session logout) — waives: `base_permission`, `denial_codes`

---

## user.session.revoke

**Domain:** user

**Description:** Revoke every cookie session of a user (platform admin only)

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/users/{id}/revoke-sessions` |

**Principals:** `user`

**Credentials:** `session_jwt`

**Bearer:** `session_only` (reason `SESSION_RECOVERY`)

**Resource Resolver:** user-from-url

**Effects:** `revoke-authority`

### Governance

- **Kind:** peer_superior
- Only an unscoped local platform admin may revoke another user's sessions

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`
- `pkg/hub:TestSessionOnlyGate_ReasonIsReported`

### Exemptions

- **hub_admin:** Platform-admin role check (requireAdminFor); the session generation increment is logged, not audited (scope: user session revocation) — waives: `base_permission`, `audit_obligation`

---

## user.terminalworkspace

**Domain:** user

**Description:** Read or replace the caller's own terminal workspace

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/users/me/terminal-workspace` |
| http_route | PUT | `/api/v1/users/me/terminal-workspace` |

**Principals:** `user`

**Credentials:** `session_jwt`

**Bearer:** `session_only` (reason `INTERACTIVE_STATE`)

**Resource Resolver:** self-principal

**Effects:** `read-one`, `update-resource`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`
- `pkg/hub:TestSessionOnlyGate_ReasonIsReported`

### Exemptions

- **authentication_only:** The path names no user; the subject is always the caller, so no resource permission applies (scope: caller's own terminal workspace) — waives: `base_permission`

---

## user.skillinjection.update

**Domain:** user.skillinjection

**Description:** Add, replace or remove the skills injected into the caller's own agents. A user access token needs user_skill_injection:update on a hub boundary

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | DELETE | `/api/v1/users/me/injected-skills/{id}` |
| http_route | POST | `/api/v1/users/me/injected-skills` |
| http_route | PUT | `/api/v1/users/me/injected-skills` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `self_record`; boundaries `hub`)

**Base Permission:** `user_skill_injection.update`

**Resource Resolver:** self-principal

**Effects:** `create-resource`, `update-resource`, `delete-resource`

### Audit

- **Event Type:** `user.skillinjection.update`
- **Context Fields:** actor_id
- **Before Fields:** skill_injection_id
- **Atomic:** Yes

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestUserInjectedSkillsWrite_TokenNeedsUpdateScope`
- `pkg/hub:TestUserInjectedSkillsWrite_ProjectBoundaryTokenDenied`
- `pkg/hub:TestUserInjectedSkillsWrite_SessionAndDevUnchanged`
- `pkg/hub:TestUserInjectedSkillsWrite_FederatedUserDenied`

---

## user.skillinjection.read

**Domain:** user.skillinjection

**Description:** List the skills injected into the caller's own agents. Any token of the holder reads its own entries; the entries belong to no project

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/users/me/injected-skills` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit_self` (self filter `none`; pinned by `TestUserInjectedSkillsList_AnyTokenReadsOwnEntries`)

**Resource Resolver:** self-principal

**Effects:** `list-scoped`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestUserInjectedSkillsList_AnyTokenReadsOwnEntries`

### Exemptions

- **authentication_only:** The path names no user; the subject is always the caller, and the list carries only the caller's own entries (scope: caller's own user-scope injected skills) — waives: `base_permission`

---

## template.user.read

**Domain:** template

**Description:** List, read or download the caller's own user-scope templates. A user access token needs template:read on a hub boundary

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/users/me/templates` |
| http_route | GET | `/api/v1/users/me/templates/{id}` |
| http_route | GET | `/api/v1/users/me/templates/{id}/download` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `catalog_record`; boundaries `hub`)

**Base Permission:** `template.read`

**Resource Resolver:** self-principal

**Effects:** `read-one`, `list-scoped`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestUserTemplateWrite_RequiresTemplateSelector`

---

## template.user.create

**Domain:** template

**Description:** Create a template in the caller's own user scope; the scope and owner are the caller. A user access token needs template:create on a hub boundary

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/users/me/templates` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `catalog_record`; boundaries `hub`)

**Base Permission:** `template.create`

**Resource Resolver:** self-principal

**Effects:** `create-resource`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestUserTemplateWrite_RequiresTemplateSelector`
- `pkg/hub:TestUserTemplateWrites_FederatedUserRefused`

---

## template.user.update

**Domain:** template

**Description:** Update, upload to or finalize one of the caller's own user-scope templates. A user access token needs template:update on a hub boundary

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | PUT | `/api/v1/users/me/templates/{id}` |
| http_route | POST | `/api/v1/users/me/templates/{id}/upload` |
| http_route | POST | `/api/v1/users/me/templates/{id}/finalize` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `catalog_record`; boundaries `hub`)

**Base Permission:** `template.update`

**Resource Resolver:** self-principal

**Effects:** `update-resource`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestUserTemplateWrite_RequiresTemplateSelector`
- `pkg/hub:TestUserTemplateWrites_FederatedUserRefused`

---

## template.user.delete

**Domain:** template

**Description:** Delete one of the caller's own user-scope templates, through /api/v1/users/me/templates/{id} or /api/v1/templates/{id}. A user access token needs template:delete on a hub boundary

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | DELETE | `/api/v1/users/me/templates/{id}` |
| http_route | DELETE | `/api/v1/templates/{id}` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `catalog_record`; boundaries `hub`)

**Base Permission:** `template.delete`

**Resource Resolver:** self-principal

**Effects:** `delete-resource`

### Audit

- **Event Type:** `template.delete`
- **Context Fields:** actor_id
- **Before Fields:** template_id, template_name
- **Atomic:** Yes

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestUserTemplateWrite_RequiresTemplateSelector`
- `pkg/hub:TestUserTemplateWrites_FederatedUserRefused`

---

## hub.audit.effectiveaccess

**Domain:** hub

**Description:** Read any principal's effective access composition at the system scope. A token needs hub_audit:read on a hub boundary; HEAD answers whether the caller may read it

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/admin/effective-access` |
| http_route | HEAD | `/api/v1/admin/effective-access` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `hub_instance`; boundaries `hub`)

**Base Permission:** `hub.audit.read`

**Resource Resolver:** hub-scoped

**Effects:** `read-one`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestHubAuditToken_EffectiveAccessAndCrossPrincipalExplain`

---

## hub.audit.explain

**Domain:** hub

**Description:** Explain another principal's authorization, or compare two principals' effective permissions. A token needs hub_audit:read on a hub boundary; grant details of the other principal are redacted

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/authz/explain` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `hub_instance`; boundaries `hub`)

**Base Permission:** `hub.audit.read`

**Resource Resolver:** hub-scoped

**Effects:** `read-one`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestHubAuditToken_EffectiveAccessAndCrossPrincipalExplain`

---

## user.self.explain

**Domain:** user

**Description:** Explain the caller's own authorization. A token's decisions apply its ceiling and boundary, and its effective permission list holds only the permissions it may use; a project-boundary token explains only targets in its boundary project, while its holder is a current member of that project

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/authz/explain` |

**Principals:** `user`, `agent`

**Credentials:** `session_jwt`, `scoped_uat`, `agent_jwt`

**Bearer:** `admit_self` (self filter `boundary_project`; pinned by `TestExplainSelfToken_ProjectBoundaryLimitedToItsProject`)

**Resource Resolver:** self-principal

**Effects:** `read-one`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestExplainSelfToken_ProjectBoundaryLimitedToItsProject`

### Exemptions

- **authentication_only:** Explains the caller's own authorization; every explained decision runs with the caller's own credential (scope: caller's own authorization only) — waives: `base_permission`

---

## user.self.profile

**Domain:** user

**Description:** Read the caller's own profile and preferences. Any token reads its holder's own profile. A visitor reads its own federated account; an unlinked federated user is refused

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/auth/me` |

**Principals:** `user`, `agent`

**Credentials:** `session_jwt`, `scoped_uat`, `agent_jwt`, `federated_user`

**Bearer:** `admit_self` (self filter `none`; pinned by `TestSelfIdentityReadsToken_AnyTokenReads`)

**Resource Resolver:** self-principal

**Effects:** `read-one`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestSelfIdentityReadsToken_AnyTokenReads`
- `pkg/hub:TestSelfReadCredential_VisitorIdentityOnly`
- `pkg/hub:TestVisitorRoute_AuthMeReturnsVisitorOwnProfile`
- `pkg/hub:TestVisitorRoute_UnlinkedFederatedUserUnchanged`

### Exemptions

- **authentication_only:** Reads the caller's own profile; carries no authority (scope: caller's own profile only) — waives: `base_permission`

---

## user.self.scopes

**Domain:** user

**Description:** List the token selectors and, for a requested boundary, the caller's own mint eligibility for each. Any token reads it

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/auth/scopes` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit_self` (self filter `none`; pinned by `TestSelfIdentityReadsToken_AnyTokenReads`)

**Resource Resolver:** self-principal

**Effects:** `read-one`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestSelfIdentityReadsToken_AnyTokenReads`

### Exemptions

- **authentication_only:** Lists the selector catalog and the caller's own mint eligibility; carries no authority (scope: selector catalog and caller's own eligibility only) — waives: `base_permission`

---

## user.self.adminstatus

**Domain:** user

**Description:** Read the caller's own admin status and system-scope permissions. For a token the permission list holds only the permissions its ceiling allows that are eligible for its boundary kind

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/auth/admin-status` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit_self` (self filter `none`; pinned by `TestAdminStatusToken_ReportsCeilingIntersection`)

**Resource Resolver:** self-principal

**Effects:** `read-one`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestAdminStatusToken_ReportsCeilingIntersection`

### Exemptions

- **authentication_only:** Reads the caller's own admin status; a token's permission list is limited to its ceiling and boundary (scope: caller's own admin status only) — waives: `base_permission`

---

## user.self.groups

**Domain:** user

**Description:** List the caller's own effective groups. A project-boundary token lists groups that belong to no project and the groups of its boundary project, the latter only while its holder is a current member of that project

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/users/me/groups` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit_self` (self filter `boundary_project`; pinned by `TestMyGroupsToken_ProjectBoundaryFiltersProjectGroups`)

**Resource Resolver:** self-principal

**Effects:** `list-scoped`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestMyGroupsToken_ProjectBoundaryFiltersProjectGroups`

### Exemptions

- **authentication_only:** Lists the caller's own group memberships; a project token sees only its boundary project's groups and groups with no project (scope: caller's own groups only) — waives: `base_permission`

---

## user.invite.redeem

**Domain:** user

**Description:** Redeem an invitation code for the caller's identity. Session-only: a token does not bind an invitation to its holder

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/auth/invite/redeem` |

**Principals:** `user`

**Credentials:** `session_jwt`

**Bearer:** `session_only` (reason `IDENTITY_BINDING`)

**Resource Resolver:** self-principal

**Effects:** `update-resource`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestInviteRedeem_SessionOnly`

### Exemptions

- **authentication_only:** Redeems an invitation for the caller's own identity; the handler requires an interactive session and refuses a federated caller (scope: caller's own identity only) — waives: `base_permission`

---

## user.chatlink.verify

**Domain:** user

**Description:** Link a pending chat account to the caller's identity with a verification code. Session-only: a token does not bind an external account to its holder

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/telegram/link/verify` |
| http_route | POST | `/api/v1/discord/link/verify` |
| http_route | POST | `/api/v1/teams/link/verify` |

**Principals:** `user`

**Credentials:** `session_jwt`

**Bearer:** `session_only` (reason `IDENTITY_BINDING`)

**Resource Resolver:** self-principal

**Effects:** `update-resource`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestChatLinkVerify_SessionOnly`

### Exemptions

- **authentication_only:** Links an external chat account to the caller's own identity; the handler requires an interactive session and refuses a federated caller (scope: caller's own identity only) — waives: `base_permission`

---

## hub.authreset

**Domain:** hub

**Description:** Reset all agent authentication credentials (emergency action)

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/admin/agents/reset-auth-all` |

**Principals:** `user`

**Credentials:** `session_jwt`

**Bearer:** `session_only` (reason `SESSION_RECOVERY`)

**Base Permission:** `hub.auth_reset.execute`

**Resource Resolver:** hub-scoped

**Effects:** `revoke-authority`

### Governance

- **Kind:** peer_superior
- Mass auth reset is a drastic authority revocation requiring hub admin governance

### Audit

- **Event Type:** `hub.authreset`
- **Context Fields:** actor_id
- **Before Fields:** agent_count
- **Atomic:** Yes

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`
- `pkg/hub:TestAdminResetAuthAll_TokenRefused`
- `pkg/hub:TestAdminResetAuthAll_SessionPassesGuard`
- `pkg/hub:TestAdminResetAuthAll_DevCredentialPassesGuard`

---

## hub.authreset.reissue

**Domain:** hub

**Description:** Re-issue an agent's role scopes from its delegator's current authority (dispatched from POST .../agents/{id}/reset-auth when reissue_scopes is set; hub super-admin only)

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| internal_dispatch | — | `handleAgentResetAuth:reissue-scopes` |

**Principals:** `user`

**Credentials:** `session_jwt`

**Bearer:** `session_only` (reason `GOV_PENDING`)

**Base Permission:** `hub.auth_reset.execute`

**Resource Resolver:** hub-scoped

**Effects:** `change-authority`, `revoke-authority`, `mint-credential`

### Delegation

- **Kind:** `conditional_on_increase`
- The re-issued role and scopes are checked with CanDelegate against the delegator's live grant (never the operator's), and the role is never raised

**Authority Evaluation:** `before_and_after`

### Governance

- **Kind:** peer_superior
- Re-recording an agent's delegated authority and revoking its credentials is a hub super-admin action

### Audit

- **Event Type:** `agent_scopes_reissued`
- **Context Fields:** actor_id
- **Before Fields:** role_before, edge_replaced
- **After Fields:** role_after, edge_new, scopes_added, scopes_removed, credentials_revoked
- **Atomic:** Yes

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`
- `pkg/hub:TestScopeReissue_OperatorRefusals`
- `pkg/hub:TestScopeReissue_UserDelegatorFailClosed`
- `pkg/hub:TestScopeReissue_UserDelegatorLookupFault`
- `pkg/hub:TestScopeReissue_SessionRootedEqualsCreateToday`
- `pkg/hub:TestScopeReissue_SessionRootedEqualsCreateTodayAfterChange`

---

## hub.authreset.reissueall

**Domain:** hub

**Description:** Re-issue every agent's role scopes from its delegator's current authority (dispatched from POST /api/v1/admin/agents/reset-auth-all when reissue_scopes is set; dry run by default; hub super-admin only)

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| internal_dispatch | — | `handleAdminResetAuthAll:reissue-scopes` |

**Principals:** `user`

**Credentials:** `session_jwt`

**Bearer:** `session_only` (reason `GOV_PENDING`)

**Base Permission:** `hub.auth_reset.execute`

**Resource Resolver:** hub-scoped

**Effects:** `change-authority`, `revoke-authority`, `mint-credential`

### Delegation

- **Kind:** `conditional_on_increase`
- Each agent's re-issued role and scopes are checked with CanDelegate against its own delegator's live grant (never the operator's), and no role is raised

**Authority Evaluation:** `before_and_after`

### Governance

- **Kind:** peer_superior
- A hub-wide re-issue of delegated authority is a hub super-admin action; each agent is re-issued and audited on its own

### Audit

- **Event Type:** `agent_scopes_reissue_batch`
- **Context Fields:** actor_id
- **Before Fields:** total
- **After Fields:** dry_run, succeeded, noop, refused, push_failed
- **Atomic:** No
- **Non-Atomic Justification:** Each agent's re-issue commits with its own agent_scopes_reissued row in one transaction; the batch row is a summary written after the run with a fresh context, and the response reports batch_audit_recorded=false if it could not be written

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`
- `pkg/hub:TestScopeReissueBulk_OperatorRefusals`

---

## hub.config.read

**Domain:** hub

**Description:** Read server configuration and schema

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/admin/server-config` |
| http_route | GET | `/api/v1/admin/server-config/schema` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `hub_instance`; boundaries `hub`)

**Base Permission:** `hub.config.read`

**Resource Resolver:** hub-scoped

**Effects:** `read-one`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestHubConfigToken_ProjectBoundaryDenied`

---

## hub.config.update

**Domain:** hub

**Description:** Update server configuration sections. The route guard checks hub.config.read, so a token needs hub_config:read and hub_config:update, and writes configuration keys only

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | PUT | `/api/v1/admin/server-config` |
| http_route | PATCH | `/api/v1/admin/server-config` |
| http_route | POST | `/api/v1/admin/server-config` |
| http_route | DELETE | `/api/v1/admin/server-config/sections/{id}` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `hub_instance`; boundaries `hub`)

**Base Permission:** `hub.config.update`

**Resource Resolver:** hub-scoped

**Effects:** `update-resource`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestServerConfigUpdate_AuthorityKeysRefuseTokens`

---

## hub.messaging.update

**Domain:** hub

**Description:** Read and update messaging configuration switches

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/admin/messaging` |
| http_route | PUT | `/api/v1/admin/messaging` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `hub_instance`; boundaries `hub`)

**Base Permission:** `hub.messaging.update`

**Resource Resolver:** hub-scoped

**Effects:** `update-resource`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestBearerDispositionMatrix_CatalogEntryPoints`

---

## hub.profiling.update

**Domain:** hub

**Description:** Read and update the profiling switches (session only)

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/admin/profiling` |
| http_route | PUT | `/api/v1/admin/profiling` |

**Principals:** `user`

**Credentials:** `session_jwt`

**Bearer:** `session_only` (reason `HOST_OPERATIONS`)

**Base Permission:** `hub.config.update`

**Resource Resolver:** hub-scoped

**Effects:** `update-resource`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestAdminProfiling_TokenRefused`

---

## hub.experiments.update

**Domain:** hub

**Description:** Read and update hub-wide experiment overrides

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/admin/experiments` |
| http_route | PUT | `/api/v1/admin/experiments` |
| http_route | DELETE | `/api/v1/admin/experiments` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `hub_instance`; boundaries `hub`)

**Base Permission:** `hub.experiments.update`

**Resource Resolver:** hub-scoped

**Effects:** `update-resource`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestBearerDispositionMatrix_CatalogEntryPoints`

---

## hub.conduitgrantkeys.rotate

**Domain:** hub

**Description:** Rotate the conduit grant signing key (kids and timestamps only in the response)

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/admin/conduit/grant-keys/rotate` |

**Principals:** `user`

**Credentials:** `session_jwt`

**Bearer:** `session_only` (reason `CREDENTIAL_MANAGEMENT`)

**Base Permission:** `hub.conduit_grant_keys.execute`

**Resource Resolver:** hub-scoped

**Effects:** `update-resource`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestAdminConduitGrantKeyRotate`

---

## hub.conduitgrantkeys.revoke

**Domain:** hub

**Description:** Revoke a conduit grant key at once (the kid only in the response)

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/admin/conduit/grant-keys/revoke` |

**Principals:** `user`

**Credentials:** `session_jwt`

**Bearer:** `session_only` (reason `CREDENTIAL_MANAGEMENT`)

**Base Permission:** `hub.conduit_grant_keys.execute`

**Resource Resolver:** hub-scoped

**Effects:** `update-resource`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestAdminConduitGrantKeyRevoke`

---

## hub.maintenance.execute

**Domain:** hub

**Description:** Execute maintenance operations including migrations and restarts

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/admin/maintenance/operations` |
| http_route | GET | `/api/v1/admin/maintenance/operations/{id}` |
| http_route | POST | `/api/v1/admin/maintenance/operations/{id}/run` |
| http_route | POST | `/api/v1/admin/maintenance/restart` |
| http_route | POST | `/api/v1/admin/maintenance/check-updates` |
| http_route | POST | `/api/v1/admin/maintenance/migrations/{id}/run` |
| http_route | GET | `/api/v1/admin/maintenance/update-available` |
| http_route | DELETE | `/api/v1/admin/maintenance/update-available` |

**Principals:** `user`

**Credentials:** `session_jwt`

**Base Permission:** `hub.maintenance.execute`

**Resource Resolver:** hub-scoped

**Effects:** `update-resource`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`

---

## hub.adminmode.read

**Domain:** hub

**Description:** Read admin/maintenance mode state

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/admin/maintenance` |

**Principals:** `user`

**Credentials:** `session_jwt`

**Base Permission:** `hub.admin_mode.read`

**Resource Resolver:** hub-scoped

**Effects:** `read-one`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestHubAdminReadUpdateRoutePermissions`

---

## hub.adminmode.update

**Domain:** hub

**Description:** Toggle admin/maintenance mode

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | PUT | `/api/v1/admin/maintenance` |

**Principals:** `user`

**Credentials:** `session_jwt`

**Base Permission:** `hub.admin_mode.update`

**Resource Resolver:** hub-scoped

**Effects:** `update-resource`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`

---

## hub.allowlist.read

**Domain:** hub

**Description:** Read the platform email allow list and its domains. A token needs hub_allow_list:read on the hub boundary; it does not admit the allow-list writes

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/admin/allow-list` |
| http_route | GET | `/api/v1/admin/allow-list/domains` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `hub_instance`; boundaries `hub`)

**Base Permission:** `hub.allow_list.read`

**Resource Resolver:** hub-scoped

**Effects:** `list-scoped`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestHubAdminReadUpdateRoutePermissions`
- `pkg/hub:TestAllowListReadToken_DoesNotAdmitWrites`

---

## hub.allowlist.update

**Domain:** hub

**Description:** Manage the platform email allow list: add and import create invited records (status invited, placeholder role member, no role binding or group membership); delete removes an invited record under the last-project-owner guard and binding cascade of user.admin.delete. The route guard checks hub.allow_list.read, so a token needs hub_allow_list:read and hub_allow_list:update on the hub boundary

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/admin/allow-list` |
| http_route | POST | `/api/v1/admin/allow-list/import` |
| http_route | DELETE | `/api/v1/admin/allow-list/{email}` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `hub_instance`; boundaries `hub`)

**Base Permission:** `hub.allow_list.update`

**Resource Resolver:** hub-scoped

**Effects:** `update-resource`

**Denial Codes:** `forbidden`, `last_owner`, `conflict`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`
- `pkg/hub:TestAllowListReadToken_DoesNotAdmitWrites`
- `pkg/hub:TestAllowListDeleteToken_LastProjectOwnerProtected`
- `pkg/hub:TestInvitationToken_CreatesNoRoleBindingOrGroupMembership`

---

## hub.health.read

**Domain:** hub

**Description:** Read platform health summary and GCP quota status

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/admin/health/summary` |
| http_route | GET | `/api/v1/admin/gcp-quota` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `hub_instance`; boundaries `hub`)

**Base Permission:** `hub.health.read`

**Resource Resolver:** hub-scoped

**Effects:** `read-one`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestBearerDispositionMatrix_CatalogEntryPoints`

---

## hub.diagnostics.read

**Domain:** hub

**Description:** Read diagnostic logs, the diagnostic log stream and messaging divergence data. The log stream re-checks a token credential on every heartbeat and ends once the token stops validating or loses hub.diagnostics.read

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/admin/diagnostics/logs` |
| sse | GET | `/api/v1/admin/diagnostics/logs/stream` |
| http_route | GET | `/api/v1/admin/messaging/divergence` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `hub_instance`; boundaries `hub`)

**Base Permission:** `hub.diagnostics.read`

**Resource Resolver:** hub-scoped

**Effects:** `read-one`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestDiagnosticsLogStream_EndsWhenTokenStopsValidating`

---

## hub.scheduler.read

**Domain:** hub

**Description:** Read scheduler status and configuration

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/admin/scheduler` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `hub_instance`; boundaries `hub`)

**Base Permission:** `hub.scheduler.read`

**Resource Resolver:** hub-scoped

**Effects:** `read-one`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestBearerDispositionMatrix_CatalogEntryPoints`

---

## hub.projectdefaults.read

**Domain:** hub

**Description:** Read project default settings

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/admin/project-defaults` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `hub_instance`; boundaries `hub`)

**Base Permission:** `hub.project_defaults.read`

**Resource Resolver:** hub-scoped

**Effects:** `read-one`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestBearerDispositionMatrix_CatalogEntryPoints`

---

## hub.lifecyclehooks.read

**Domain:** hub

**Description:** Read lifecycle hook definitions

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/admin/lifecycle-hooks` |
| http_route | GET | `/api/v1/admin/lifecycle-hooks/{id}` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `hub_instance`; boundaries `hub`)

**Base Permission:** `hub.lifecycle_hooks.read`

**Resource Resolver:** hub-scoped

**Effects:** `read-one`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestBearerDispositionMatrix_CatalogEntryPoints`

---

## hub.projectdefaults.update

**Domain:** hub

**Description:** Update project default settings. The route guard checks hub.project_defaults.read, so a token needs hub_project_defaults:read and hub_project_defaults:update, and writes configuration keys only

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | PUT | `/api/v1/admin/project-defaults` |
| http_route | PATCH | `/api/v1/admin/project-defaults` |
| http_route | POST | `/api/v1/admin/project-defaults` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `hub_instance`; boundaries `hub`)

**Base Permission:** `hub.project_defaults.update`

**Resource Resolver:** hub-scoped

**Effects:** `update-resource`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestProjectDefaultsUpdate_EveryKeyClassifiedForTokens`

---

## hub.lifecyclehooks.update

**Domain:** hub

**Description:** Create, update, delete and activate hub lifecycle hooks and hub pre-start hooks. The admin lifecycle-hook route guard checks hub.lifecycle_hooks.read, so a token writing there needs hub_lifecycle_hooks:read and hub_lifecycle_hooks:update

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/admin/lifecycle-hooks` |
| http_route | PUT | `/api/v1/admin/lifecycle-hooks/{id}` |
| http_route | DELETE | `/api/v1/admin/lifecycle-hooks/{id}` |
| http_route | POST | `/api/v1/pre-start-hooks` |
| http_route | PUT | `/api/v1/pre-start-hooks/{id}` |
| http_route | POST | `/api/v1/pre-start-hooks/{id}/activate` |
| http_route | DELETE | `/api/v1/pre-start-hooks/{id}` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `hub_instance`; boundaries `hub`)

**Base Permission:** `hub.lifecycle_hooks.update`

**Resource Resolver:** hub-scoped

**Effects:** `update-resource`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestBearerDispositionMatrix_CatalogEntryPoints`

---

## hub.settings.update

**Domain:** hub

**Description:** Set the user-defined hub injected skills; system entries are preserved

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | PUT | `/api/v1/hub/settings/injected-skills` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `hub_instance`; boundaries `hub`)

**Base Permission:** `hub.settings.update`

**Resource Resolver:** hub-scoped

**Effects:** `update-resource`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestBearerDispositionMatrix_CatalogEntryPoints`

---

## hub.validate.execute

**Domain:** hub

**Description:** Validate resource definitions against schema

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/admin/validate-resources` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `hub_instance`; boundaries `hub`)

**Base Permission:** `hub.validate.execute`

**Resource Resolver:** hub-scoped

**Effects:** `read-one`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestBearerDispositionMatrix_CatalogEntryPoints`

---

## hub.integrations.read

**Domain:** hub

**Description:** Read integration configurations, the available-integrations list, integration health and integration update status

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/admin/integrations` |
| http_route | GET | `/api/v1/admin/integrations/{name}` |
| http_route | GET | `/api/v1/admin/integrations/available` |
| http_route | GET | `/api/v1/admin/integrations/{name}/health` |
| http_route | GET | `/api/v1/admin/integrations/{name}/update/{id}` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `hub_instance`; boundaries `hub`)

**Base Permission:** `hub.integrations.read`

**Resource Resolver:** hub-scoped

**Effects:** `read-one`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestBearerDispositionMatrix_CatalogEntryPoints`

---

## hub.integrations.update

**Domain:** hub

**Description:** Update an integration's settings and restart an integration. The route guard checks hub.integrations.read, so a token needs hub_integrations:read and hub_integrations:update. A config update that sets secrets or any settings key outside the configuration set requires an interactive session

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | PUT | `/api/v1/admin/integrations/{name}/config` |
| http_route | POST | `/api/v1/admin/integrations/{name}/restart` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `hub_instance`; boundaries `hub`)

**Base Permission:** `hub.integrations.update`

**Resource Resolver:** hub-scoped

**Effects:** `update-resource`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestIntegrationConfigUpdate_SecretsSessionOnlyForTokens`

---

## hub.integrations.install

**Domain:** hub

**Description:** Install an integration and start an integration update; both build and install code on the hub host, so an interactive session only

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/admin/integrations/{name}/install` |
| http_route | POST | `/api/v1/admin/integrations/{name}/update` |

**Principals:** `user`

**Credentials:** `session_jwt`

**Bearer:** `session_only` (reason `HOST_OPERATIONS`)

**Base Permission:** `hub.integrations.update`

**Resource Resolver:** hub-scoped

**Effects:** `update-resource`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestIntegrationInstall_SessionOnlyForTokens`

---

## hub.teamsmanifest.read

**Domain:** hub

**Description:** Read Teams integration manifest

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/admin/integrations/teams/manifest` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `hub_instance`; boundaries `hub`)

**Base Permission:** `hub.teams_manifest.read`

**Resource Resolver:** hub-scoped

**Effects:** `read-one`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestBearerDispositionMatrix_CatalogEntryPoints`

---

## hub.metrics.read

**Domain:** hub

**Description:** Read metrics dashboard data

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/metrics/{name}` |
| http_route | GET | `/api/v1/admin/metrics-dashboard` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `hub_instance`; boundaries `hub`)

**Base Permission:** `hub.metrics.read`

**Resource Resolver:** hub-scoped

**Effects:** `read-one`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestMetricsDashboard_RequiresMetricsSelector`

---

## hub.githubapp.read

**Domain:** hub

**Description:** Read GitHub App configuration and installations

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/github-app` |
| http_route | GET | `/api/v1/github-app/installations` |
| http_route | GET | `/api/v1/github-app/installations/{id}` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `hub_instance`; boundaries `hub`)

**Base Permission:** `hub.github_app.read`

**Resource Resolver:** hub-scoped

**Effects:** `read-one`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestBearerDispositionMatrix_CatalogEntryPoints`

---

## hub.githubapp.update

**Domain:** hub

**Description:** Create, update and delete GitHub App installations, discover installations and sync permissions

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/github-app/installations` |
| http_route | PUT | `/api/v1/github-app/installations/{id}` |
| http_route | DELETE | `/api/v1/github-app/installations/{id}` |
| http_route | POST | `/api/v1/github-app/installations/discover` |
| http_route | POST | `/api/v1/github-app/sync-permissions` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `hub_instance`; boundaries `hub`)

**Base Permission:** `hub.github_app.update`

**Resource Resolver:** hub-scoped

**Effects:** `update-resource`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestBearerDispositionMatrix_CatalogEntryPoints`

---

## hub.githubapp.config.update

**Domain:** hub

**Description:** Update the GitHub App configuration, which sets the hub's app credentials; an interactive session only

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | PUT | `/api/v1/github-app` |

**Principals:** `user`

**Credentials:** `session_jwt`

**Bearer:** `session_only` (reason `CREDENTIAL_MANAGEMENT`)

**Base Permission:** `hub.github_app.update`

**Resource Resolver:** hub-scoped

**Effects:** `update-resource`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestGitHubAppConfigUpdate_SessionOnlyForTokens`

---

## quota.read

**Domain:** quota

**Description:** Read limit definitions, entitlements, and usage. The route guard checks quota.read on the hub-scoped quota target

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/admin/limits` |
| http_route | GET | `/api/v1/admin/limits/{id}` |
| http_route | GET | `/api/v1/admin/limits/{id}/entitlements` |
| http_route | GET | `/api/v1/admin/entitlements/{id}` |
| http_route | GET | `/api/v1/admin/usage` |
| http_route | GET | `/api/v1/admin/usage/{limit}` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `hub_instance`; boundaries `hub`)

**Base Permission:** `quota.read`

**Resource Resolver:** hub-scoped

**Effects:** `read-one`, `list-scoped`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestQuotaAdminToken_ResolvesHubTarget`

---

## quota.create

**Domain:** quota

**Description:** Create limit definitions and entitlement bindings. The route guard checks quota.read, so a token needs quota:read and quota:create

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/admin/limits` |
| http_route | POST | `/api/v1/admin/limits/{id}/entitlements` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `hub_instance`; boundaries `hub`)

**Base Permission:** `quota.create`

**Resource Resolver:** hub-scoped

**Effects:** `create-resource`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestQuotaAdminToken_ResolvesHubTarget`

---

## quota.update

**Domain:** quota

**Description:** Update limit definitions and entitlement bindings. The route guard checks quota.read, so a token needs quota:read and quota:update

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | PUT | `/api/v1/admin/limits/{id}` |
| http_route | PUT | `/api/v1/admin/entitlements/{id}` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `hub_instance`; boundaries `hub`)

**Base Permission:** `quota.update`

**Resource Resolver:** hub-scoped

**Effects:** `update-resource`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestQuotaAdminToken_ResolvesHubTarget`

---

## quota.brokersettings.update

**Domain:** quota

**Description:** Replace a runtime broker's settings document. Requires broker.read on the broker and quota.update on the hub-scoped quota target for every write, including one that changes no key, and each changed key's declared permission (maxAgents declares quota.update)

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | PUT | `/api/v1/runtime-brokers/{id}/settings` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `hub_instance`; boundaries `hub`)

**Base Permission:** `quota.update`

**Resource Resolver:** hub-scoped

**Effects:** `update-resource`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestBrokerSettingsPut_RequiresQuotaUpdateEvenWithoutChanges`
- `pkg/hub:TestBrokerSettingsPut_EachChangedKeyIsAuthorized`

---

## quota.delete

**Domain:** quota

**Description:** Delete limit definitions and entitlement bindings. The route guard checks quota.read, so a token needs quota:read and quota:delete

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | DELETE | `/api/v1/admin/limits/{id}` |
| http_route | DELETE | `/api/v1/admin/entitlements/{id}` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `hub_instance`; boundaries `hub`)

**Base Permission:** `quota.delete`

**Resource Resolver:** hub-scoped

**Effects:** `delete-resource`

### Audit

- **Event Type:** `quota.delete`
- **Context Fields:** actor_id
- **Before Fields:** limit_id, limit_name
- **Atomic:** Yes

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestQuotaAdminToken_ResolvesHubTarget`

---

## quota.usage.self.read

**Domain:** quota

**Description:** Read the caller's own quota usage: each limit's effective value and active count for the caller only

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/usage/me` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit_self` (self filter `none`; pinned by `TestUsageMe_ReturnsOnlyTheHolder`)

**Resource Resolver:** none

**Effects:** `read-one`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestUsageMe_ReturnsOnlyTheHolder`

### Exemptions

- **authentication_only:** Returns only the caller's own usage; carries no other user's records (scope: caller's own quota usage only) — waives: `base_permission`

---

## hub.policies.removed

**Domain:** hub

**Description:** Removed policy API; every method and sub-path answers 410 Gone and points callers to role bindings

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/policies` |
| http_route | GET | `/api/v1/policies/{id}` |

**Principals:** `user`

**Credentials:** `session_jwt`

**Bearer:** `non_user`

**Resource Resolver:** none

**Effects:** `read-one`

### Tests

- `pkg/hub:TestBearerDisposition_EveryRoutePatternCovered`

### Exemptions

- **authentication_only:** The handler answers 410 Gone for every caller and reads or changes nothing; no resource permission applies (scope: removed policy API) — waives: `base_permission`, `denial_codes`

---

## skill.list

**Domain:** skill

**Description:** List skills; each row is filtered by skill read. A user access token needs skill:list on either boundary; its page, total and cursor count only the rows it may read

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/skills` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `list_scope`; boundaries `project`, `hub`)

**Base Permission:** `skill.list`

**Resource Resolver:** project-from-url

**Effects:** `list-scoped`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`
- `pkg/hub:TestSkillListToken_CountsOnlyAuthorizedRows`

---

## skill.read

**Domain:** skill

**Description:** Read a skill, its versions, its download URLs, its resolved version and its files. A record the caller may not read answers as not found

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/skills/{id}` |
| http_route | GET | `/api/v1/skills/{id}/versions` |
| http_route | GET | `/api/v1/skills/{id}/versions/{vid}` |
| http_route | GET | `/api/v1/skills/{id}/download` |
| http_route | GET | `/api/v1/skills/{id}/resolve` |
| http_route | GET | `/api/v1/skills/{id}/files/{path}` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `catalog_record`; boundaries `project`, `hub`)

**Base Permission:** `skill.read`

**Resource Resolver:** skill-from-url

**Effects:** `read-one`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestBearerDispositionMatrix_CatalogEntryPoints`

---

## skill.create

**Domain:** skill

**Description:** Create a skill in a project named by the request body. A user access token needs skill:create on a boundary that contains the project

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/skills` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `project_body`; boundaries `project`, `hub`)

**Base Permission:** `skill.create`

**Resource Resolver:** project-from-body

**Effects:** `create-resource`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`
- `pkg/hub:TestBearerDispositionMatrix_CatalogEntryPoints`

---

## skill.update

**Domain:** skill

**Description:** Update a skill: its metadata, version publishing and deprecation, file uploads and finalize. A user-scope skill refuses a federated caller (requireProfileWriter)

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | PATCH | `/api/v1/skills/{id}` |
| http_route | POST | `/api/v1/skills/{id}/versions` |
| http_route | POST | `/api/v1/skills/{id}/versions/{vid}/deprecate` |
| http_route | POST | `/api/v1/skills/{id}/upload` |
| http_route | POST | `/api/v1/skills/{id}/finalize` |
| http_route | PUT | `/api/v1/skills/{id}/files/{path}` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `catalog_record`; boundaries `project`, `hub`)

**Base Permission:** `skill.update`

**Resource Resolver:** skill-from-url

**Effects:** `update-resource`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`
- `pkg/hub:TestUserSkillWrites_FederatedUserRefused`

---

## skill.delete

**Domain:** skill

**Description:** Delete a skill definition. A user-scope skill refuses a federated caller (requireProfileWriter)

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | DELETE | `/api/v1/skills/{id}` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `catalog_record`; boundaries `project`, `hub`)

**Base Permission:** `skill.delete`

**Resource Resolver:** skill-from-url

**Effects:** `delete-resource`

### Audit

- **Event Type:** `skill.delete`
- **Context Fields:** actor_id, project_id
- **Before Fields:** skill_id, skill_name
- **Atomic:** Yes

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`
- `pkg/hub:TestUserSkillWrites_FederatedUserRefused`

---

## skill.register

**Domain:** skill

**Description:** Manage skill registries and their pinned hashes. The route guard checks skill.register on the hub-scoped registry target, so a user access token needs skill:register on a hub boundary

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/skill-registries` |
| http_route | GET | `/api/v1/skill-registries` |
| http_route | GET | `/api/v1/skill-registries/{id}` |
| http_route | PUT | `/api/v1/skill-registries/{id}` |
| http_route | PATCH | `/api/v1/skill-registries/{id}` |
| http_route | POST | `/api/v1/skill-registries/{id}/pin` |
| http_route | GET | `/api/v1/skill-registries/{id}/pins` |
| http_route | POST | `/api/v1/skill-registries/{id}/unpin` |
| http_route | DELETE | `/api/v1/skill-registries/{id}` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `hub_instance`; boundaries `hub`)

**Base Permission:** `skill.register`

**Resource Resolver:** hub-scoped

**Effects:** `create-resource`, `update-resource`, `delete-resource`

### Audit

- **Event Type:** `skill.register`
- **Context Fields:** actor_id
- **Before Fields:** registry_id
- **Atomic:** Yes

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`
- `pkg/hub:TestSkillRegistryToken_RequiresRegisterSelector`

---

## template.list

**Domain:** template

**Description:** List templates; each row is filtered by template read. The unfiltered list is used only with hub-level evidence for template.list: a user access token needs template:list on a hub boundary and hub-wide list authority for it. Every other token, project tokens included, gets the per-row template read scan

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/templates` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `hub_collection`; boundaries `hub`)

**Base Permission:** `template.list`

**Resource Resolver:** project-from-url

**Effects:** `list-scoped`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`
- `pkg/hub:TestCatalogWideList_UsesListSelectorThroughEvidence`

---

## template.read

**Domain:** template

**Description:** Read a template, its download URLs, its storage validation and its files. A record the caller may not read answers as not found

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/templates/{id}` |
| http_route | GET | `/api/v1/templates/{id}/download` |
| http_route | GET | `/api/v1/templates/{id}/validate` |
| http_route | GET | `/api/v1/templates/{id}/files` |
| http_route | GET | `/api/v1/templates/{id}/files/{path}` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `catalog_record`; boundaries `project`, `hub`)

**Base Permission:** `template.read`

**Resource Resolver:** template-from-url

**Effects:** `read-one`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestBearerDispositionMatrix_CatalogEntryPoints`

---

## template.create

**Domain:** template

**Description:** Create a template in the global, project or caller's own user scope the request body names. A user-scope create refuses a federated caller (requireProfileWriter)

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/templates` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `catalog_record`; boundaries `project`, `hub`)

**Base Permission:** `template.create`

**Resource Resolver:** project-from-body

**Effects:** `create-resource`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`
- `pkg/hub:TestGenericUserTemplateWrites_FederatedUserRefused`

---

## template.update

**Domain:** template

**Description:** Update a template: its definition and metadata, file uploads, finalize and file writes. A user-scope template refuses a federated caller (requireProfileWriter)

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | PUT | `/api/v1/templates/{id}` |
| http_route | PATCH | `/api/v1/templates/{id}` |
| http_route | POST | `/api/v1/templates/{id}/upload` |
| http_route | POST | `/api/v1/templates/{id}/finalize` |
| http_route | POST | `/api/v1/templates/{id}/files` |
| http_route | PUT | `/api/v1/templates/{id}/files/{path}` |
| http_route | DELETE | `/api/v1/templates/{id}/files/{path}` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `catalog_record`; boundaries `project`, `hub`)

**Base Permission:** `template.update`

**Resource Resolver:** template-from-url

**Effects:** `update-resource`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`
- `pkg/hub:TestGenericUserTemplateWrites_FederatedUserRefused`

---

## template.delete

**Domain:** template

**Description:** Delete a global or project template. A user-scope template is template.user.delete

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | DELETE | `/api/v1/templates/{id}` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `catalog_record`; boundaries `project`, `hub`)

**Base Permission:** `template.delete`

**Resource Resolver:** template-from-url

**Effects:** `delete-resource`

### Audit

- **Event Type:** `template.delete`
- **Context Fields:** actor_id, project_id
- **Before Fields:** template_id, template_name
- **Atomic:** Yes

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`
- `pkg/hub:TestGenericUserTemplateWrites_FederatedUserRefused`

---

## harnessconfig.list

**Domain:** harnessconfig

**Description:** List harness configurations; each row is filtered by harness config read. The unfiltered list is used only with hub-level evidence for harness_config.list: a user access token needs harness_config:list on a hub boundary and hub-wide list authority for it. Every other token, project tokens included, gets the per-row harness config read scan

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/harness-configs` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `hub_collection`; boundaries `hub`)

**Base Permission:** `harness_config.list`

**Resource Resolver:** project-from-url

**Effects:** `list-scoped`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`
- `pkg/hub:TestCatalogWideList_UsesListSelectorThroughEvidence`

---

## harnessconfig.read

**Domain:** harnessconfig

**Description:** Read a harness configuration, its download URLs, its storage validation, its image status and its files. A record the caller may not read answers as not found

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/harness-configs/{id}` |
| http_route | GET | `/api/v1/harness-configs/{id}/download` |
| http_route | GET | `/api/v1/harness-configs/{id}/validate` |
| http_route | GET | `/api/v1/harness-configs/{id}/image-status` |
| http_route | GET | `/api/v1/harness-configs/{id}/files` |
| http_route | GET | `/api/v1/harness-configs/{id}/files/{path}` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `catalog_record`; boundaries `project`, `hub`)

**Base Permission:** `harness_config.read`

**Resource Resolver:** project-from-url

**Effects:** `read-one`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`

---

## harnessconfig.create

**Domain:** harnessconfig

**Description:** Create a harness configuration in the global, project or caller's own user scope the request body names; a user-scope config's scope and owner are the caller. A user-scope harness config refuses a federated caller (requireProfileWriter)

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/harness-configs` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `catalog_record`; boundaries `project`, `hub`)

**Base Permission:** `harness_config.create`

**Resource Resolver:** project-from-body

**Effects:** `create-resource`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`
- `pkg/hub:TestUserHarnessConfigWrites_FederatedUserRefused`

---

## harnessconfig.update

**Domain:** harnessconfig

**Description:** Update a harness configuration: its definition and metadata, file uploads, finalize, the image check and file writes. On a user-scope harness config the existing route gate (authorizeHarnessConfigRoute) already refuses a federated caller, with requireProfileWriter behind it

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | PUT | `/api/v1/harness-configs/{id}` |
| http_route | PATCH | `/api/v1/harness-configs/{id}` |
| http_route | POST | `/api/v1/harness-configs/{id}/upload` |
| http_route | POST | `/api/v1/harness-configs/{id}/finalize` |
| http_route | POST | `/api/v1/harness-configs/{id}/check-image` |
| http_route | POST | `/api/v1/harness-configs/{id}/files` |
| http_route | PUT | `/api/v1/harness-configs/{id}/files/{path}` |
| http_route | DELETE | `/api/v1/harness-configs/{id}/files/{path}` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `catalog_record`; boundaries `project`, `hub`)

**Base Permission:** `harness_config.update`

**Resource Resolver:** harnessconfig-from-url

**Effects:** `update-resource`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`
- `pkg/hub:TestUserHarnessConfigWrites_FederatedUserRefused`

---

## harnessconfig.delete

**Domain:** harnessconfig

**Description:** Delete a global or project harness configuration. The route gate needs harness_config read and the delete needs harness_config delete on the owning scope. A user-scope config is harnessconfig.user.delete

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | DELETE | `/api/v1/harness-configs/{id}` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `catalog_record`; boundaries `project`, `hub`)

**Base Permission:** `harness_config.delete`

**Resource Resolver:** harnessconfig-from-url

**Effects:** `delete-resource`

### Audit

- **Event Type:** `harnessconfig.delete`
- **Context Fields:** actor_id, project_id
- **Before Fields:** config_id, config_name
- **Atomic:** Yes

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`
- `pkg/hub:TestUserHarnessConfigWrites_FederatedUserRefused`
- `pkg/hub:TestHarnessDeleteToken_RequiresReadAndDelete`

---

## skill.global.create

**Domain:** skill

**Description:** Create a skill in the global catalog (global or core scope). The check is skill.create_global on a collection-level request at the hub scope, so a user access token needs skill:create_global on a hub boundary

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/skills` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `hub_collection`; boundaries `hub`)

**Base Permission:** `skill.create_global`

**Resource Resolver:** hub-scoped

**Effects:** `create-resource`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestGlobalSkillCreate_RequiresCreateGlobalSelectorAndHubBoundary`

---

## skill.discover.project

**Domain:** skill

**Description:** Discover the skills in a remote GitHub directory for a project named by the request body, spending that project's GitHub credentials. A user access token needs skill:create on a boundary that contains the project

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/skills/discover-directory` |

**Principals:** `user`, `agent`

**Credentials:** `session_jwt`, `scoped_uat`, `agent_jwt`

**Bearer:** `admit` (target `project_collection`; boundaries `project`, `hub`)

**Base Permission:** `skill.create`

**Resource Resolver:** project-from-body

**Effects:** `read-one`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestDiscoverDirectory_RequiresSkillCreateOnProject`

---

## skill.discover.global

**Domain:** skill

**Description:** Discover the skills in a public remote GitHub directory with no project named. A user access token needs skill:create_global on a hub boundary

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/skills/discover-directory` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `hub_collection`; boundaries `hub`)

**Base Permission:** `skill.create_global`

**Resource Resolver:** hub-scoped

**Effects:** `read-one`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestDiscoverDirectory_GlobalRequiresCreateGlobalSelector`

---

## skill.resolve.local

**Domain:** skill

**Description:** Resolve skill:// references against the hub catalog. Each candidate is checked for skill read; a candidate the caller may not read answers as not found

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/skills/resolve` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `catalog_record`; boundaries `project`, `hub`)

**Base Permission:** `skill.read`

**Resource Resolver:** skill-from-body

**Effects:** `read-one`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestLocalSkillResolve_TokenNeedsSkillRead`

---

## skill.resolve.github.project

**Domain:** skill

**Description:** Resolve gh:// references with a project named by the request body, using that project's GitHub credentials. The caller needs skill read on the project, so a user access token needs skill:read on a boundary that contains the project; a refused reference answers forbidden

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/skills/resolve` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `project_body`; boundaries `project`, `hub`)

**Base Permission:** `skill.read`

**Resource Resolver:** project-from-body

**Effects:** `read-one`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestProjectGitHubSkillResolve_TokenNeedsSkillReadOnProject`

---

## skill.resolve.github.public

**Domain:** skill

**Description:** Resolve gh:// references from public repositories with no project named; the hub fetches them and writes its resolution cache. A user access token needs skill:read on the global catalog, on a hub boundary; a refused reference answers forbidden

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/skills/resolve` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `catalog_record`; boundaries `hub`)

**Base Permission:** `skill.read`

**Resource Resolver:** hub-scoped

**Effects:** `read-one`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestPublicGitHubSkillResolve_TokenNeedsSkillReadOnHub`

---

## skill.file.signedread

**Domain:** skill

**Description:** Read one skill version file through a signed URL issued by the skill download. The signature alone authorizes exactly the skill, version and file it was issued for; no principal is evaluated

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/skills/{id}/files/{path}` |

**Bearer:** `non_user`

**Resource Resolver:** skill-from-url

**Effects:** `read-one`

### Tests

- `pkg/hub:TestSignedSkillFileURL_EndToEnd`

### Exemptions

- **public_endpoint:** Capability URL: the signature issued by the skill download authorizes one skill version file, with no principal (scope: signed skill file read) — waives: `principals`, `credentials`, `base_permission`, `denial_codes`

---

## template.clone

**Domain:** template

**Description:** Clone a readable template into the global scope or a project the request body names. The source needs template read; the destination needs template create on its scope, so a user access token needs template:read and template:create on a boundary that contains both

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/templates/{id}/clone` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `catalog_record`; boundaries `project`, `hub`)

**Base Permission:** `template.create`

**Resource Resolver:** project-from-body

**Effects:** `create-resource`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestCatalogCloneToken_RequiresReadAndCreate`

---

## template.reimport

**Domain:** template

**Description:** Reimport a global or project template from its source. The template needs template read and the reimport needs template create on the owning scope, as import does

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/templates/{id}/reimport` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `catalog_record`; boundaries `project`, `hub`)

**Base Permission:** `template.create`

**Resource Resolver:** template-from-url

**Effects:** `update-resource`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestCatalogReimportToken_RequiresReadAndCreate`

---

## template.import.global

**Domain:** template

**Description:** Import or discover templates for the global catalog. The check is template create on a collection-level request at the hub scope, so a user access token needs template:create on a hub boundary

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/resources/import` |
| http_route | POST | `/api/v1/resources/discover` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `hub_collection`; boundaries `hub`)

**Base Permission:** `template.create`

**Resource Resolver:** hub-scoped

**Effects:** `create-resource`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestGlobalResourceImport_ResolvesHubThroughEvidence`

---

## template.import.project

**Domain:** template

**Description:** Import or discover templates for a project named by the request body. The check is template create on a collection-level request inside that project

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/resources/import` |
| http_route | POST | `/api/v1/resources/discover` |

**Principals:** `user`, `agent`

**Credentials:** `session_jwt`, `scoped_uat`, `agent_jwt`

**Bearer:** `admit` (target `project_collection`; boundaries `project`, `hub`)

**Base Permission:** `template.create`

**Resource Resolver:** project-from-body

**Effects:** `create-resource`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestProjectResourceImport_TokenNeedsCreateOnTheProject`

---

## harnessconfig.clone

**Domain:** harnessconfig

**Description:** Clone a readable harness configuration into the global scope or a project the request body names. The route gate needs harness_config read on the source; the destination needs harness_config create on its scope

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/harness-configs/{id}/clone` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `catalog_record`; boundaries `project`, `hub`)

**Base Permission:** `harness_config.create`

**Resource Resolver:** project-from-body

**Effects:** `create-resource`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestCatalogCloneToken_RequiresReadAndCreate`

---

## harnessconfig.reimport

**Domain:** harnessconfig

**Description:** Reimport a global or project harness configuration from its source. The route gate needs harness_config read and the reimport needs harness_config create on the owning scope, as import does

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/harness-configs/{id}/reimport` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `catalog_record`; boundaries `project`, `hub`)

**Base Permission:** `harness_config.create`

**Resource Resolver:** harnessconfig-from-url

**Effects:** `update-resource`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestCatalogReimportToken_RequiresReadAndCreate`

---

## harnessconfig.import.global

**Domain:** harnessconfig

**Description:** Import or discover harness configurations for the global catalog. The check is harness_config create on a collection-level request at the hub scope, so a user access token needs harness_config:create on a hub boundary

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/resources/import` |
| http_route | POST | `/api/v1/resources/discover` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `hub_collection`; boundaries `hub`)

**Base Permission:** `harness_config.create`

**Resource Resolver:** hub-scoped

**Effects:** `create-resource`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestGlobalResourceImport_ResolvesHubThroughEvidence`

---

## harnessconfig.import.project

**Domain:** harnessconfig

**Description:** Import or discover harness configurations for a project named by the request body. The check is harness_config create on a collection-level request inside that project

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/resources/import` |
| http_route | POST | `/api/v1/resources/discover` |

**Principals:** `user`, `agent`

**Credentials:** `session_jwt`, `scoped_uat`, `agent_jwt`

**Bearer:** `admit` (target `project_collection`; boundaries `project`, `hub`)

**Base Permission:** `harness_config.create`

**Resource Resolver:** project-from-body

**Effects:** `create-resource`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestProjectResourceImport_TokenNeedsCreateOnTheProject`

---

## template.user.clone

**Domain:** template

**Description:** Clone a readable template into the caller's own user scope. The source needs template:read; a user access token needs template:create on a hub boundary for the destination

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/templates/{id}/clone` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `catalog_record`; boundaries `hub`)

**Base Permission:** `template.create`

**Resource Resolver:** self-principal

**Effects:** `create-resource`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestCloneToUserScope_RequiresCreateOnDestination`

---

## template.user.reimport

**Domain:** template

**Description:** Reimport one of the caller's own user-scope templates from its source. The read gate needs template:read and the reimport needs template:update on the record, on a hub boundary for a user access token

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/templates/{id}/reimport` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `catalog_record`; boundaries `hub`)

**Base Permission:** `template.update`

**Resource Resolver:** self-principal

**Effects:** `update-resource`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestTemplateUserReimport_RequiresUpdateAction`

---

## skill.user.create

**Domain:** skill

**Description:** Create a skill in the caller's own user scope; the scope and owner are the caller. A user access token needs skill:create on a hub boundary

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/skills` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `catalog_record`; boundaries `hub`)

**Base Permission:** `skill.create`

**Resource Resolver:** self-principal

**Effects:** `create-resource`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestUserScopeSkillCreate_RequiresSkillCreate`
- `pkg/hub:TestUserSkillWrites_FederatedUserRefused`

---

## harnessconfig.user.clone

**Domain:** harnessconfig

**Description:** Clone a readable harness configuration into the caller's own user scope. The source needs harness_config:read; a user access token needs harness_config:create on a hub boundary for the destination

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/harness-configs/{id}/clone` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `catalog_record`; boundaries `hub`)

**Base Permission:** `harness_config.create`

**Resource Resolver:** self-principal

**Effects:** `create-resource`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestCloneToUserScope_RequiresCreateOnDestination`

---

## harnessconfig.user.delete

**Domain:** harnessconfig

**Description:** Delete one of the caller's own user-scope harness configurations. The route gate needs harness_config:read and the delete needs harness_config:delete, on a hub boundary for a user access token

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | DELETE | `/api/v1/harness-configs/{id}` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `catalog_record`; boundaries `hub`)

**Base Permission:** `harness_config.delete`

**Resource Resolver:** self-principal

**Effects:** `delete-resource`

### Audit

- **Event Type:** `harnessconfig.delete`
- **Context Fields:** actor_id
- **Before Fields:** config_id, config_name
- **Atomic:** Yes

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestHarnessUserDelete_RequiresDeleteAction`

---

## harnessconfig.user.reimport

**Domain:** harnessconfig

**Description:** Reimport one of the caller's own user-scope harness configurations from its source. The route gate needs harness_config:read and the reimport needs harness_config:update, on a hub boundary for a user access token

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/harness-configs/{id}/reimport` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `catalog_record`; boundaries `hub`)

**Base Permission:** `harness_config.update`

**Resource Resolver:** self-principal

**Effects:** `update-resource`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestHarnessUserReimport_RequiresUpdateAction`

---

## template.user.import

**Domain:** template

**Description:** Import or discover templates for the caller's own user scope. A user access token needs template:create on a hub boundary

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/resources/import` |
| http_route | POST | `/api/v1/resources/discover` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `catalog_record`; boundaries `hub`)

**Base Permission:** `template.create`

**Resource Resolver:** self-principal

**Effects:** `create-resource`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestUserScopeImport_RequiresCreateSelector`
- `pkg/hub:TestUserResourceImport_FederatedUserRefused`

---

## harnessconfig.user.import

**Domain:** harnessconfig

**Description:** Import or discover harness configurations for the caller's own user scope. A user access token needs harness_config:create on a hub boundary

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/resources/import` |
| http_route | POST | `/api/v1/resources/discover` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `catalog_record`; boundaries `hub`)

**Base Permission:** `harness_config.create`

**Resource Resolver:** self-principal

**Effects:** `create-resource`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestUserScopeImport_RequiresCreateSelector`
- `pkg/hub:TestUserResourceImport_FederatedUserRefused`

---

## broker.read

**Domain:** broker

**Description:** Read runtime broker status or list brokers

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/runtime-brokers` |
| http_route | GET | `/api/v1/runtime-brokers/{id}` |
| http_route | GET | `/api/v1/runtime-brokers/{id}/settings` |
| http_route | GET | `/api/v1/runtime-brokers/{id}/projects` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `hub_instance`; boundaries `hub`)

**Base Permission:** `broker.read`

**Resource Resolver:** hub-scoped

**Effects:** `read-one`, `list-scoped`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`
- `pkg/hub:TestBearerDispositionMatrix_CatalogEntryPoints`
- `pkg/hub:TestHubTokenBrokerOwner_ReadsOwnBrokerThroughOwnerGrant`

---

## broker.material.read

**Domain:** broker

**Description:** Read a runtime broker's environment variables (sensitive values masked) and secret metadata. A user needs broker.read on the broker and, beyond it, the owner relationship or broker.update; the broker reads its own material

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/runtime-brokers/{id}/env` |
| http_route | GET | `/api/v1/runtime-brokers/{id}/env/{key}` |
| http_route | GET | `/api/v1/runtime-brokers/{id}/secrets` |
| http_route | GET | `/api/v1/runtime-brokers/{id}/secrets/{key}` |

**Principals:** `user`, `broker`

**Credentials:** `session_jwt`, `scoped_uat`, `broker_token`

**Bearer:** `admit` (target `hub_instance`; boundaries `hub`)

**Base Permission:** `broker.read`

**Resource Resolver:** hub-scoped

**Effects:** `read-one`, `list-scoped`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestBearerDispositionMatrix_CatalogEntryPoints`
- `pkg/hub:TestBrokerMaterial_ReadsRequireBrokerRead`

---

## broker.material.write

**Domain:** broker

**Description:** Set, patch or delete a runtime broker's environment variables and secrets. A user needs an interactive session and broker.update on the broker; the broker writes its own material

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | PUT | `/api/v1/runtime-brokers/{id}/env/{key}` |
| http_route | DELETE | `/api/v1/runtime-brokers/{id}/env/{key}` |
| http_route | PUT | `/api/v1/runtime-brokers/{id}/secrets/{key}` |
| http_route | PATCH | `/api/v1/runtime-brokers/{id}/secrets/{key}` |
| http_route | DELETE | `/api/v1/runtime-brokers/{id}/secrets/{key}` |

**Principals:** `user`, `broker`

**Credentials:** `session_jwt`, `broker_token`

**Bearer:** `session_only` (reason `GOV_PENDING`)

**Base Permission:** `broker.update`

**Resource Resolver:** hub-scoped

**Effects:** `create-resource`, `update-resource`, `delete-resource`

### Audit

- **Event Type:** `broker.material.write`
- **Context Fields:** actor_id, broker_id
- **Before Fields:** key
- **Atomic:** Yes

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestBearerDispositionMatrix_CatalogEntryPoints`
- `pkg/hub:TestBrokerMaterial_WritesSessionOnly`

---

## broker.update

**Domain:** broker

**Description:** Update a runtime broker's name, labels or host service account. A user needs an interactive session and broker.update on the broker

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | PATCH | `/api/v1/runtime-brokers/{id}` |

**Principals:** `user`

**Credentials:** `session_jwt`

**Bearer:** `session_only` (reason `GOV_PENDING`)

**Base Permission:** `broker.update`

**Resource Resolver:** hub-scoped

**Effects:** `update-resource`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestBearerDispositionMatrix_CatalogEntryPoints`

---

## broker.delete

**Domain:** broker

**Description:** Deregister a runtime broker: unlink it from every project, clear project defaults that name it, and remove its credentials. A user needs an interactive session and broker.delete on the broker

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | DELETE | `/api/v1/runtime-brokers/{id}` |

**Principals:** `user`

**Credentials:** `session_jwt`

**Bearer:** `session_only` (reason `GOV_PENDING`)

**Base Permission:** `broker.delete`

**Resource Resolver:** hub-scoped

**Effects:** `delete-resource`

### Audit

- **Event Type:** `broker.deregister`
- **Context Fields:** actor_id, broker_id
- **Before Fields:** broker_name
- **Atomic:** Yes

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestBearerDispositionMatrix_CatalogEntryPoints`

---

## broker.heartbeat

**Domain:** broker

**Description:** Record a broker heartbeat: broker liveness and the state of the agents it runs. The broker sends its own; a user needs an interactive session and broker.update on the broker from a role binding (the owner relationship does not apply)

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/runtime-brokers/{id}/heartbeat` |

**Principals:** `broker`, `user`

**Credentials:** `broker_token`, `session_jwt`

**Bearer:** `session_only` (reason `HOST_OPERATIONS`)

**Base Permission:** `broker.update`

**Resource Resolver:** broker-self

**Effects:** `update-resource`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestBearerDispositionMatrix_CatalogEntryPoints`
- `pkg/hub:TestBrokerHeartbeat_UserPathSessionOnly`

---

## broker.agent.launchreport

**Domain:** broker

**Description:** Record a broker's launch report for an agent it runs

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/runtime-brokers/{id}/agents/{agentId}/launch` |

**Principals:** `broker`

**Credentials:** `broker_token`

**Bearer:** `non_user`

**Resource Resolver:** broker-self

**Effects:** `update-resource`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestBearerDisposition_EveryRoutePatternCovered`

### Exemptions

- **internal_only:** The broker authenticates with its own HMAC credential for its own record; no user permission applies (scope: broker self access) — waives: `base_permission`

---

## broker.messagefailures.report

**Domain:** broker

**Description:** Record buffered message delivery failures reported by a broker

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/runtime-brokers/{id}/message-failures` |

**Principals:** `broker`

**Credentials:** `broker_token`

**Bearer:** `non_user`

**Resource Resolver:** broker-self

**Effects:** `update-resource`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestBearerDisposition_EveryRoutePatternCovered`

### Exemptions

- **internal_only:** The broker authenticates with its own HMAC credential for its own record; no user permission applies (scope: broker self access) — waives: `base_permission`

---

## broker.controlchannel.call

**Domain:** broker

**Description:** Carry a call between the hub and a connected broker over the control channel; each call runs under the operation that initiated it

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| broker_call | — | `TunnelRequest:controlchannel` |
| broker_call | — | `OpenStream:controlchannel` |
| broker_call | — | `SendStreamData:controlchannel` |
| broker_call | — | `ResizeStream:controlchannel` |
| broker_call | — | `CloseStream:controlchannel` |
| broker_call | — | `handleResponse:controlchannel` |
| broker_call | — | `handleStreamData:controlchannel` |
| broker_call | — | `handleStreamClose:controlchannel` |
| broker_call | — | `handleEvent:controlchannel` |

**Principals:** `broker`, `system`

**Credentials:** `broker_token`, `system_internal`

**Bearer:** `non_user`

**Resource Resolver:** initiating-operation

**Effects:** `read-one`, `update-resource`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestBearerDisposition_EveryRoutePatternCovered`

### Exemptions

- **internal_only:** Transport between the hub and an authenticated broker; authorization belongs to the initiating operation (scope: control channel transport) — waives: `base_permission`

---

## gcp.identity.create

**Domain:** gcp.identity

**Description:** Create a GCP service account binding

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/gcp-service-accounts` |

**Principals:** `user`

**Credentials:** `session_jwt`

**Base Permission:** `gcp_service_account.create`

**Resource Resolver:** project-from-body

**Effects:** `assign-credential`

### Governance

- **Kind:** issuer_credential
- Service account creation assigns a credential to project scope

### Audit

- **Event Type:** `gcp.identity.create`
- **Context Fields:** actor_id, project_id
- **After Fields:** service_account_email
- **Atomic:** Yes

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`

---

## gcp.identity.delete

**Domain:** gcp.identity

**Description:** Delete a GCP service account binding

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | DELETE | `/api/v1/gcp-service-accounts/{id}` |

**Principals:** `user`

**Credentials:** `session_jwt`

**Base Permission:** `gcp_service_account.delete`

**Resource Resolver:** gcp-service-account-from-url

**Effects:** `delete-resource`

### Audit

- **Event Type:** `gcp.identity.delete`
- **Context Fields:** actor_id
- **Before Fields:** service_account_id
- **Atomic:** Yes

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`

---

## gcp.identity.assign

**Domain:** gcp.identity

**Description:** Assign a GCP service account to an agent

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| internal_dispatch | — | `createAgentInProject:gcp-identity-assign` |
| internal_dispatch | — | `applyAgentUpdate:gcp-identity-assign` |

**Principals:** `user`

**Credentials:** `session_jwt`

**Base Permission:** `gcp_service_account.assign`

**Resource Resolver:** gcp-service-account-from-url

**Effects:** `assign-credential`

### Governance

- **Kind:** issuer_credential
- Assigning a service account to an agent grants the agent access to the service account's identity

### Audit

- **Event Type:** `gcp.identity.assign`
- **Context Fields:** actor_id
- **After Fields:** service_account_id, agent_id
- **Atomic:** Yes

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`

---

## gcp.identity.mint

**Domain:** gcp.identity

**Description:** Mint a GCP access token for a service account

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/agent/gcp-token` |

**Principals:** `agent`

**Credentials:** `agent_jwt`

**Base Permission:** `gcp_service_account.mint`

**Resource Resolver:** agent-gcp-service-account

**Effects:** `mint-credential`

### Governance

- **Kind:** issuer_credential
- Agent mints GCP tokens scoped to its assigned service account

### Audit

- **Event Type:** `gcp.identity.mint`
- **Context Fields:** agent_id
- **After Fields:** service_account_email, token_scopes
- **Atomic:** No
- **Non-Atomic Justification:** Token minting calls external GCP API; audit recorded before external call

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`

---

## secret.read

**Domain:** secret

**Description:** Read project secrets or environment variables containing secrets

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/secrets` |
| http_route | GET | `/api/v1/secrets/{key}` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Base Permission:** `project.read`

**Resource Resolver:** project-from-url

**Effects:** `read-secret`

### Audit

- **Event Type:** `secret.read`
- **Context Fields:** actor_id, project_id
- **Atomic:** Yes

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`

---

## secret.write

**Domain:** secret

**Description:** Create, update or delete secrets. At user scope, the default, the secrets are the caller's own and a federated caller is refused (requireProfileWriter)

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | PUT | `/api/v1/secrets/{key}` |
| http_route | DELETE | `/api/v1/secrets/{key}` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Base Permission:** `project.update`

**Resource Resolver:** project-from-url

**Effects:** `create-resource`, `update-resource`, `delete-resource`

### Audit

- **Event Type:** `secret.write`
- **Context Fields:** actor_id, project_id
- **Before Fields:** secret_key
- **Atomic:** Yes

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`
- `pkg/hub:TestUserEnvSecretWrites_FederatedUserRefused`
- `pkg/hub:TestUserEnvSecretWrites_SessionAndTokenUnchanged`

---

## gcp.identity.read

**Domain:** gcp.identity

**Description:** Read GCP service account details or list accounts. A project's list requires project.read on it (members, and agents of that project only); the hub-scoped list requires gcp_service_account.list at hub scope (hub members)

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/gcp-service-accounts` |
| http_route | GET | `/api/v1/gcp-service-accounts/{id}` |
| http_route | GET | `/api/v1/projects/{id}/gcp-service-accounts` |

**Principals:** `user`, `agent`

**Credentials:** `session_jwt`, `scoped_uat`, `agent_jwt`

**Base Permission:** `gcp_service_account.read`

**Resource Resolver:** project-from-url

**Effects:** `read-one`, `list-scoped`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`

---

## gcp.identity.verify

**Domain:** gcp.identity

**Description:** Verify a GCP service account's IAM configuration

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/gcp-service-accounts/{id}/verify` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Base Permission:** `gcp_service_account.verify`

**Resource Resolver:** gcp-identity-from-url

**Effects:** `assign-credential`

### Governance

- **Kind:** issuer_credential
- Verification may re-bind IAM credentials

### Audit

- **Event Type:** `gcp.identity.verify`
- **Context Fields:** actor_id, project_id
- **After Fields:** service_account_id, verification_status
- **Atomic:** Yes

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`

---

## env.read

**Domain:** env

**Description:** Read project environment variables

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/env` |
| http_route | GET | `/api/v1/env/{key}` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Base Permission:** `project.read`

**Resource Resolver:** project-from-url

**Effects:** `read-one`, `list-scoped`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`

---

## env.hub.list

**Domain:** env

**Description:** List hub-level environment variables (scope=hub), without secret entries

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/env` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Base Permission:** `hub.env_vars.read`

**Resource Resolver:** hub-scoped

**Effects:** `list-scoped`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`

---

## testidentity.create

**Domain:** testidentity

**Description:** Issue a short-lived synthetic member or viewer test identity and one access token for it (no refresh token, no cookie)

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/test-identities` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`, `workload_delegation`

**Bearer:** `admit` (target `hub_collection`; boundaries `hub`)

**Base Permission:** `test_identity.issue`

**Resource Resolver:** hub-scoped

**Effects:** `create-resource`, `mint-credential`

### Governance

- **Kind:** issuer_credential
- The role is member or viewer only; live identities are capped per issuer and per hub, and issuance is rate limited per issuer

### Audit

- **Event Type:** `test_identity_issue`
- **Context Fields:** actor_id, credential_id, credential_kind
- **After Fields:** user_id, role, issued_by, purpose, expires_at, token_ttl_seconds, workload_delegation
- **Atomic:** Yes

**Denial Codes:** `forbidden`, `not_found`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`
- `pkg/hub:TestTestIdentity_IssuerAuthorization`

---

## testidentity.list

**Domain:** testidentity

**Description:** List test identities: the caller's own, or every identity for an unscoped platform admin session

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/test-identities` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`, `workload_delegation`

**Bearer:** `admit` (target `hub_collection`; boundaries `hub`)

**Base Permission:** `test_identity.issue`

**Resource Resolver:** hub-scoped

**Effects:** `list-scoped`

**Denial Codes:** `forbidden`, `not_found`

### Tests

- `pkg/hub:TestTestIdentity_ListIsolation`

---

## testidentity.token.issue

**Domain:** testidentity

**Description:** Re-issue one access token for a live test identity, for its issuer or an unscoped platform admin session; an admin-kind identity's token is re-issued only through the admin-scope workload delegation that issued it (phase X4b)

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/test-identities/{id}/token` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`, `workload_delegation`

**Bearer:** `admit` (target `hub_collection`; boundaries `hub`)

**Base Permission:** `test_identity.issue`

**Resource Resolver:** hub-scoped

**Effects:** `mint-credential`

### Governance

- **Kind:** issuer_credential
- Only the identity's issuer (or an unscoped platform admin session) re-issues a member-tier token, with test_identity.issue; the token never outlives the identity. An admin-kind identity's token (no re-issue for any other caller: 409) is re-issued only through the admin-scope workload delegation that issued it, of the identity's kind, with that kind's issuer checks (its capability, a live super-admin grantor through current bindings), at most the kind's token lifetime and never past the identity's expiry or its creation plus the kind's maximum lifetime; the delegation, the grantor and the identity are re-checked inside the mint transaction, and a test admin audit sink line carries the delegation id. Admin token re-issue is bounded only by the per-delegation test identity rate limiter, not by an admin-specific limit; each re-issue adds a live token (earlier ones stay valid until they expire), and every token ends no later than the identity's kind lifetime

### Audit

- **Event Type:** `test_identity_token_issue`
- **Context Fields:** actor_id, credential_id, credential_kind
- **After Fields:** user_id, role, issued_by, token_ttl_seconds, workload_delegation
- **Atomic:** Yes

**Denial Codes:** `forbidden`, `not_found`, `conflict`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`
- `pkg/hub:TestTestIdentity_TokenReissue`
- `pkg/hub:TestWorkloadAdminCredentials_TokenReissueOwnIdentity`
- `pkg/hub:TestWorkloadAdminCredentials_TokenReissueRefusals`

---

## testidentity.webcode.issue

**Domain:** testidentity

**Description:** Mint a single-use, five-minute web-login code for a live test identity, for its issuer, a workload issuer delegation on the identities it issued (member scope: member-tier identities; admin scope, phase X4b: admin-kind identities of its kind), or an unscoped platform admin session; only on hubs where test identity web sessions are available. Kind test_fixture needs test_identity.issue; an admin kind (test_hub_admin, test_super_admin) only while server.hub.test_identities.admin_web_sessions is on at an admin --test-mode level, and only with the issuer checks of that kind's issuance (its capability, a live super-admin through current bindings, an access token with exactly the kind's scope)

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/test-identities/{id}/web-login-code` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`, `workload_delegation`

**Bearer:** `admit` (target `hub_collection`; boundaries `hub`)

**Base Permission:** `test_identity.issue`

**Resource Resolver:** hub-scoped

**Effects:** `mint-credential`

### Governance

- **Kind:** issuer_credential
- Only the identity's issuer (or a workload delegation, for the identities of its tier issued through it, or an unscoped platform admin session) mints a code; for an admin-kind identity the caller also passes that kind's issuer checks, and through an admin-scope delegation the delegation, the grantor and the identity are re-checked inside the mint transaction and a test admin audit sink line carries the delegation id; at most one unredeemed code per identity, redeemable once, never past the identity's expiry (for an admin kind, nor its creation plus the kind's maximum lifetime)

### Audit

- **Event Type:** `test_identity_web_code_issue`
- **Context Fields:** actor_id, credential_id, credential_kind
- **After Fields:** user_id, code_id, code_expires_at, issued_by, workload_delegation
- **Atomic:** Yes

**Denial Codes:** `forbidden`, `not_found`, `conflict`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`
- `pkg/hub:TestTestIdentityWeb_Mint`
- `pkg/hub:TestAdminWebSessions_RefusedWithoutSetting`
- `pkg/hub:TestAdminWebSessions_MintNeedsTheKindsIssuerChecks`
- `pkg/hub:TestWorkloadDelegation_WebLoginCodeForOwnIdentity`
- `pkg/hub:TestWorkloadDelegation_WebLoginCodeRefusedOutsideOwnMemberIdentities`
- `pkg/hub:TestWorkloadDelegation_RevokedAfterAuthMintsNothing`
- `pkg/hub:TestWorkloadAdminCredentials_WebCodeOwnIdentity`
- `pkg/hub:TestWorkloadAdminCredentials_WebCodeRefusals`

---

## testidentity.delete

**Domain:** testidentity

**Description:** Delete a test identity (its role bindings, group memberships and user-scope data go with it), for its issuer, an unscoped platform admin session, or a holder of test_identity.issue and user.delete; refused with 409 while it owns agents or is a project's last owner. With ?purge=true it also removes the agents, schedules, groups and projects stamped as created under the identity (the identity ID is the only input), keeps stamped projects that have another owner or other resources, and reports unstamped agents as unproven; for an admin-kind identity (purged by its issuer or a super-admin) it first removes every grant stamped with its issuance and reports its residue

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | DELETE | `/api/v1/test-identities/{id}` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`, `workload_delegation`

**Bearer:** `admit` (target `hub_collection`; boundaries `hub`)

**Base Permission:** `test_identity.issue`

**Resource Resolver:** hub-scoped

**Effects:** `delete-resource`

### Governance

- **Kind:** issuer_credential
- Only a kind=test_fixture user is deleted; any other user ID answers 404, even for an admin (with ?purge=true: an admin-kind identity too, and the same 200 empty report for any ID the caller may not purge); test identities of every kind are refused as callers

### Audit

- **Event Type:** `test_identity_delete`
- **Context Fields:** actor_id, credential_id, credential_kind
- **Before Fields:** user_id, role, issued_by, purpose, expires_at
- **Atomic:** Yes

**Denial Codes:** `forbidden`, `not_found`, `conflict`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`
- `pkg/hub:TestTestIdentity_Delete`

---

## testidentity.cleanup

**Domain:** testidentity

**Description:** Remove the agents, schedules, groups and projects stamped as created under the caller's expired or deleted test identities (and the expired identities themselves), with the purge engine; the hub selects the targets from the provenance stamps, the request names none (no body; ?dryRun=true lists the targets and writes nothing). An issuer reaches its own identities, a workload delegation only the identities issued through it, an unscoped platform admin session every issuer's; a live identity is never touched

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/test-identities/cleanup` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`, `workload_delegation`

**Bearer:** `admit` (target `hub_collection`; boundaries `hub`)

**Base Permission:** `test_identity.issue`

**Resource Resolver:** hub-scoped

**Effects:** `delete-resource`

### Governance

- **Kind:** issuer_credential
- Bounded by the stamp's fixture_issuer_id: the caller's own identities, its own delegation's, or every issuer's for an unscoped platform admin session; each identity is admitted by the purge route's rules; test identities of every kind are refused as callers

### Audit

- **Event Type:** `test_identity_cleanup`
- **Context Fields:** actor_id, credential_id, credential_kind, correlation_id
- **Before Fields:** provenance_basis
- **After Fields:** identities, deleted, not_done, unproven
- **Atomic:** Yes

**Denial Codes:** `forbidden`, `not_found`, `conflict`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`
- `pkg/hub:TestFixtureCleanup_IssuerIsolation`
- `pkg/hub:TestFixtureCleanup_DelegationIsolation`
- `pkg/hub:TestFixtureCleanup_LimitCountsOnlyAdmittedNonLive`
- `pkg/hub:TestFixtureCleanup_AdminKindTargets`
- `pkg/hub:TestFixtureCleanup_DelegationCannotReachAdminKind`
- `pkg/hub:TestFixtureCleanup_AdminScopeDelegationRefused`

---

## testidentity.provenance

**Domain:** testidentity

**Description:** Explain whether the hub recorded one agent, project, schedule or group as created under a test identity of the caller's issuer scope (its own, its own delegation's, or every issuer's for an unscoped platform admin session): proven with the stamp and the identity's state, proven deleted by an in-scope deletion record for a resource that is gone, or unproven with only the reason (a missing resource and another issuer's stamp get the same answer, and no owner or creator data is returned)

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/test-identities/provenance` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`, `workload_delegation`

**Bearer:** `admit` (target `hub_collection`; boundaries `hub`)

**Base Permission:** `test_identity.issue`

**Resource Resolver:** hub-scoped

**Effects:** `read-one`

### Governance

- **Kind:** issuer_credential
- Only stamps and deletion records in the caller's issuer scope are proven; unproven, missing and out-of-scope resources are indistinguishable and carry no owner or creator fields; test identities of every kind are refused as callers

**Denial Codes:** `forbidden`, `not_found`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`
- `pkg/hub:TestFixtureProvenance_Route`
- `pkg/hub:TestFixtureProvenance_ScopeAndDeletionRecords`

---

## testidentity.admin.create

**Domain:** testidentity

**Description:** Issue a short-lived test identity holding an expiring hub-admin role (on a hub started with --test-mode=admin or above) or super-admin role (--test-mode=super-admin), and one access token for it (no refresh token, no cookie)

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/test-identities/admin` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`, `workload_delegation`

**Bearer:** `admit` (target `hub_collection`; boundaries `hub`)

**Base Permission:** `test_identity.issue_admin`

**Resource Resolver:** hub-scoped

**Effects:** `create-resource`, `grant-authority`, `mint-credential`

### Delegation

- **Kind:** `non_amplification`
- The issued hub-admin or super-admin authority never exceeds the issuer's: the issuer must be a live super-admin through its own current bindings (canIssueTestAdmin), and an access token issuer's frozen ceiling must carry the requested kind's capability. A workload issuer delegation (phase X4) acts for its human grantor within its one admin scope (test_identity:issue_admin or test_identity:issue_super_admin), which must be the requested kind's; inside the issuance transaction the delegation is re-read and the grantor is re-checked as a live human super-admin holding the capability (recheckTestAdminDelegationGrantor). The binding expires with the identity

### Governance

- **Kind:** issuer_credential
- The issuer is a human, live super-admin through its own current bindings; an access token issuer presents a hub-boundary token with exactly the requested kind's scope (test_identity:issue_admin or test_identity:issue_super_admin), at most 8 h old, loaded and checked on every issuance. A workload delegation issuer is admitted only with its delegation's admin scope available at the hub's --test-mode level; besides issuance it re-issues tokens for and mints web-login codes for the identities it issued (phase X4b), and nothing else. One live admin-kind identity per issuer, or per workload delegation its admin cap (at most 2 test hub-admins or 1 test super-admin); four per hub of which at most one test super-admin, counted across every issuer, access token and delegation under the issuance lock; issuance is rate limited per issuer or delegation (6/h hub-admin, 2/h super-admin)

### Audit

- **Event Type:** `test_admin_issue`
- **Context Fields:** actor_id, credential_id, credential_kind
- **After Fields:** user_id, kind, role, issued_by, issuer_credential_id, issuance_id, purpose, expires_at, token_ttl_seconds, workload_delegation
- **Atomic:** Yes

**Denial Codes:** `forbidden`, `not_found`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`
- `pkg/hub:TestTestAdmin_IssuanceMatrix`
- `pkg/hub:TestWorkloadAdminDelegation_IssuanceSinkLineAndAudit`

---

## testidentity.admin.revokeall

**Domain:** testidentity

**Description:** Revoke every admin-kind test identity: remove every grant stamped with its issuance, then delete it (or, when the user delete refuses it, suspend it and remove its role bindings), for a human super-admin session

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/test-identities/admin/revoke-all` |

**Principals:** `user`

**Credentials:** `session_jwt`

**Bearer:** `session_only` (reason `CREDENTIAL_MANAGEMENT`)

**Base Permission:** `test_identity.issue_admin`

**Resource Resolver:** hub-scoped

**Effects:** `delete-resource`, `revoke-authority`

### Governance

- **Kind:** issuer_credential
- Only a human caller holding an active super-admin binding, on an interactive session; only admin-kind rows are touched

### Audit

- **Event Type:** `test_admin_revoke`
- **Context Fields:** actor_id, credential_id, credential_kind
- **Before Fields:** user_id, role, issued_by, purpose, expires_at
- **After Fields:** outcome
- **Atomic:** Yes

**Denial Codes:** `forbidden`, `not_found`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`
- `pkg/hub:TestTestAdmin_RevokeAll`
- `pkg/hub:TestAdminWebSessions_RevokeAllEndsLiveSession`

---

## testidentity.admin.residue

**Domain:** testidentity

**Description:** Read the residue report of an admin-kind test identity, live or purged: the grants and resources stamped with it, and its changes to rows without its stamp, for a human super-admin session

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/test-identities/admin/{id}/residue` |

**Principals:** `user`

**Credentials:** `session_jwt`

**Bearer:** `session_only` (reason `CREDENTIAL_MANAGEMENT`)

**Base Permission:** `test_identity.issue_admin`

**Resource Resolver:** hub-scoped

**Effects:** `read-one`

### Governance

- **Kind:** issuer_credential
- Only a human caller holding an active super-admin binding, on an interactive session; only admin-kind identities are reported

**Denial Codes:** `forbidden`, `not_found`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`
- `pkg/hub:TestTestAdminA3_ResidueRoute`

---

## testidentity.delegation.create

**Domain:** testidentity

**Description:** Delegate test identity issuance to one pinned workload identity (a subject in the server.hub.test_identities.workload_issuers allowlist) with one scope: test_identity:issue (member and viewer; member cap, lifetime at most 7 days), or on a hub running the admin tier test_identity:issue_admin (admin cap 1-2, at most 8 h) or test_identity:issue_super_admin (admin cap 1, at most 2 h)

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/test-identity-delegations` |

**Principals:** `user`

**Credentials:** `session_jwt`

**Bearer:** `session_only` (reason `CREDENTIAL_MANAGEMENT`)

**Base Permission:** `test_identity.issue`

**Resource Resolver:** hub-scoped

**Effects:** `issue-credential`

### Governance

- **Kind:** issuer_credential
- Only a live human user's interactive session may delegate; the subject must be allow-listed for the scope's tier (max_tier admin for an admin scope). An admin scope is accepted only at its --test-mode level with A2 in place, and only from a live human super-admin's interactive session holding the scope's capability (canIssueTestAdmin). Every mint re-checks the grantor's live bindings, the delegation (including its scope's maximum age from creation) and the allowlist; an admin mint re-checks the grantor inside the issuance transaction

### Audit

- **Event Type:** `test_identity_delegation_create`
- **Context Fields:** actor_id, credential_id, credential_kind
- **After Fields:** delegation_id, grantor, workload_sub, scope, member_cap, admin_cap, label, expires_at
- **Atomic:** Yes

**Denial Codes:** `forbidden`, `not_found`, `validation_error`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`
- `pkg/hub:TestTestIdentityDelegation_CreateAuthorization`
- `pkg/hub:TestWorkloadAdminDelegation_SuperAdminCreateOnlyFromLiveHumanSuperAdminSession`

---

## testidentity.delegation.list

**Domain:** testidentity

**Description:** List test identity issuer delegations: the caller's own, or every delegation for an unscoped platform admin session

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/test-identity-delegations` |

**Principals:** `user`

**Credentials:** `session_jwt`

**Bearer:** `session_only` (reason `CREDENTIAL_MANAGEMENT`)

**Base Permission:** `test_identity.issue`

**Resource Resolver:** hub-scoped

**Effects:** `list-scoped`

**Denial Codes:** `forbidden`, `not_found`

### Tests

- `pkg/hub:TestTestIdentityDelegation_ListAndRevoke`

---

## testidentity.delegation.revoke

**Domain:** testidentity

**Description:** Revoke a test identity issuer delegation, for its grantor or an unscoped platform admin session; the next mint through it is refused

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | DELETE | `/api/v1/test-identity-delegations/{id}` |

**Principals:** `user`

**Credentials:** `session_jwt`

**Bearer:** `session_only` (reason `CREDENTIAL_MANAGEMENT`)

**Base Permission:** `test_identity.issue`

**Resource Resolver:** hub-scoped

**Effects:** `revoke-authority`

### Governance

- **Kind:** issuer_credential
- Only the grantor or an unscoped platform admin session revokes; any other caller gets 404

### Audit

- **Event Type:** `test_identity_delegation_revoke`
- **Context Fields:** actor_id, credential_id, credential_kind
- **Before Fields:** delegation_id, grantor, workload_sub, scope
- **Atomic:** Yes

**Denial Codes:** `forbidden`, `not_found`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`
- `pkg/hub:TestTestIdentityDelegation_ListAndRevoke`

---

## agent.delegation.create

**Domain:** agent.delegation

**Description:** Issue an agent delegation grant binding one agent the issuer controls to a boundary, a frozen permission ceiling and an expiry

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/agents/{id}/delegations` |

**Principals:** `user`

**Credentials:** `session_jwt`

**Bearer:** `session_only` (reason `CREDENTIAL_MANAGEMENT`)

**Base Permission:** `agent.delegation.create`

**Resource Resolver:** agent-from-url

**Effects:** `issue-credential`

### Governance

- **Kind:** issuer_credential
- Only the agent's owner or recorded ancestor, admitted to the agent's project, may issue; the ceiling is limited to selectors the issuer may mint and to the hub agent-delegation policy

### Audit

- **Event Type:** `agent_delegation_grant_create`
- **Context Fields:** actor_id, agent_id
- **After Fields:** grant_id, boundary_kind, permissions, expires_at
- **Atomic:** Yes

**Denial Codes:** `forbidden`, `scope_violation`, `credential_not_admitted`, `reserved_identity`, `issuer_not_controller`, `issuer_project_access`, `permission_not_delegable`, `agent_not_found`, `agent_not_eligible`, `agent_reincarnating`, `subdelegation_not_supported`, `grant_limit_reached`, `audit_failed`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`
- `pkg/hub:TestAgentDelegationIssuance_IssuesGrantWithAudit`
- `pkg/hub:TestAgentDelegationIssuance_RefusesNonSessionCredentials`

---

## agent.delegation.exchange

**Domain:** agent.delegation

**Description:** Exchange an agent delegation grant, with the bound agent's own agent token, for a short-lived opaque delegated credential

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/agents/{id}/delegations/{grantId}/exchange` |

**Principals:** `agent`

**Credentials:** `agent_jwt`

**Bearer:** `non_user` (pinned by `TestAgentDelegationExchange_RefusesUATAndSession`)

**Base Permission:** `agent.delegation.exchange`

**Resource Resolver:** agent-from-url

**Effects:** `mint-credential`

### Governance

- **Kind:** issuer_credential
- Only the bound agent, whose agent credential row is live, may exchange; the issuer, grant, agent and policy are re-checked, and the credential never exceeds the grant ceiling, the grant expiry or the agent credential's expiry

### Audit

- **Event Type:** `agent_delegation_credential_issue`
- **Context Fields:** actor_id, grant_id
- **After Fields:** credential_id, permissions, expires_at
- **Atomic:** Yes

**Denial Codes:** `credential_not_admitted`, `agent_credential_invalid`, `grant_not_found`, `grant_agent_changed`, `issuer_not_controller`, `issuer_project_access`, `grant_inactive`, `issuer_invalid`, `permission_not_delegable`, `invalid_audience`, `outside_ceiling`, `audit_failed`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`
- `pkg/hub:TestAgentDelegationExchange_IssuesCredentialWithAudit`
- `pkg/hub:TestAgentDelegationExchange_RefusesUATAndSession`

---

## agent.delegation.revoke

**Domain:** agent.delegation

**Description:** Revoke an agent delegation grant and every credential exchanged from it

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | DELETE | `/api/v1/agents/{id}/delegations/{grantId}` |

**Principals:** `user`, `agent`

**Credentials:** `session_jwt`, `agent_jwt`

**Bearer:** `session_only` (reason `CREDENTIAL_MANAGEMENT`)

**Base Permission:** `agent.delegation.revoke`

**Resource Resolver:** agent-from-url

**Effects:** `revoke-authority`

### Governance

- **Kind:** issuer_credential
- Per grant: the issuer; the bound agent with its own agent token (its credential row live and unexpired, the path agent its subject); the agent's current owner; holders of the operation's permission on the hub (hub admins), for any grant; holders of it on project P (project owners and admins), for project(P)-bounded grants only. A dev session counts only as hub authority. Delegated credentials and user access tokens are refused

### Audit

- **Event Type:** `agent_delegation_grant_revoke`
- **Context Fields:** actor_id, grant_id
- **Before Fields:** grant_id, revoke_reason, revoked_credential_ids
- **Atomic:** Yes

**Denial Codes:** `forbidden`, `credential_not_admitted`, `agent_credential_invalid`, `grant_not_found`, `audit_failed`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`
- `pkg/hub:TestAgentDelegationRevoke_CascadesWithOneAuditRow`
- `pkg/hub:TestAgentDelegationManagement_ControlAuthority`

---

## agent.delegation.credential.revoke

**Domain:** agent.delegation

**Description:** Revoke one delegated credential exchanged from an agent delegation grant

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | DELETE | `/api/v1/agents/{id}/delegations/{grantId}/credentials/{credentialId}` |

**Principals:** `user`, `agent`

**Credentials:** `session_jwt`, `agent_jwt`

**Bearer:** `session_only` (reason `CREDENTIAL_MANAGEMENT`)

**Base Permission:** `agent.delegation.credential.revoke`

**Resource Resolver:** agent-from-url

**Effects:** `revoke-authority`

### Governance

- **Kind:** issuer_credential
- Per grant: the issuer; the bound agent with its own agent token (its credential row live and unexpired, the path agent its subject); the agent's current owner; holders of the operation's permission on the hub (hub admins), for any grant; holders of it on project P (project owners and admins), for project(P)-bounded grants only. A dev session counts only as hub authority. Delegated credentials and user access tokens are refused

### Audit

- **Event Type:** `agent_delegation_credential_revoke`
- **Context Fields:** actor_id, grant_id
- **Before Fields:** credential_id, grant_id, revoke_reason
- **Atomic:** Yes

**Denial Codes:** `forbidden`, `credential_not_admitted`, `agent_credential_invalid`, `grant_not_found`, `not_found`, `audit_failed`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`
- `pkg/hub:TestAgentDelegationCredentialRevoke_RevokesOneWithAudit`
- `pkg/hub:TestAgentDelegationManagement_ControlAuthority`

---

## agent.delegation.list

**Domain:** agent.delegation

**Description:** List agent delegation grants: those bound to one agent, or those the caller issued; grants outside the caller's authority are left out

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/agents/{id}/delegations` |
| http_route | GET | `/api/v1/users/me/delegations` |

**Principals:** `user`, `agent`

**Credentials:** `session_jwt`, `agent_jwt`

**Bearer:** `session_only` (reason `CREDENTIAL_MANAGEMENT`)

**Base Permission:** `agent.delegation.list`

**Resource Resolver:** agent-from-url

**Effects:** `list-scoped`

### Governance

- **Kind:** issuer_credential
- Per grant: the issuer; the bound agent with its own agent token (its credential row live and unexpired, the path agent its subject); the agent's current owner; holders of the operation's permission on the hub (hub admins), for any grant; holders of it on project P (project owners and admins), for project(P)-bounded grants only. A dev session counts only as hub authority. Delegated credentials and user access tokens are refused. The caller's own issued grants on /users/me/delegations, session only

**Denial Codes:** `forbidden`, `credential_not_admitted`, `agent_credential_invalid`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`
- `pkg/hub:TestAgentDelegationList_LeavesOutGrantsOutsideAuthority`
- `pkg/hub:TestAgentDelegationManagement_NoSecretFieldsInResponses`

---

## agent.delegation.read

**Domain:** agent.delegation

**Description:** Read one agent delegation grant; a grant outside the caller's authority answers as missing

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/agents/{id}/delegations/{grantId}` |

**Principals:** `user`, `agent`

**Credentials:** `session_jwt`, `agent_jwt`

**Bearer:** `session_only` (reason `CREDENTIAL_MANAGEMENT`)

**Base Permission:** `agent.delegation.read`

**Resource Resolver:** agent-from-url

**Effects:** `read-one`

### Governance

- **Kind:** issuer_credential
- Per grant: the issuer; the bound agent with its own agent token (its credential row live and unexpired, the path agent its subject); the agent's current owner; holders of the operation's permission on the hub (hub admins), for any grant; holders of it on project P (project owners and admins), for project(P)-bounded grants only. A dev session counts only as hub authority. Delegated credentials and user access tokens are refused

**Denial Codes:** `forbidden`, `credential_not_admitted`, `agent_credential_invalid`, `grant_not_found`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`
- `pkg/hub:TestAgentDelegationManagement_ControlAuthority`
- `pkg/hub:TestAgentDelegationManagement_NoSecretFieldsInResponses`

---

