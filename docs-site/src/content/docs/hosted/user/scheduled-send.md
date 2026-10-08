---
title: Scheduled Chat Messages
description: Write a chat message now and have the Hub send it as you later.
---

Scheduled send lets you write a message in the web chat and have the Hub send it at a time you choose. When the time comes, the Hub sends it exactly as if you had pressed **Send** then: it comes from you, agents receive it through the usual routing, and it appears in the thread like any other message.

:::caution[Experimental]
Scheduled send is behind the `web.chat_scheduled_send` [experiment](/scion/reference/experiments/), which is off by default. While it is off, the menu item and the scheduled messages are hidden, the scheduled-message API answers `404`, and pending messages are held: they are neither sent nor failed.
:::

## Scheduling a message

1. Type your message in a topic.
2. Right-click **Send** (on a touch screen, press and hold it) and choose **Schedule send…**.
3. Pick a preset (**In 1 hour**, **Tomorrow 09:00**, **Monday 09:00**) or enter a date and time. Times are in your display time zone, shown under the field.
4. Confirm. The composer clears and the message appears at the bottom of the thread, dimmed, with the time it will be sent.

The time must be at least one minute and at most 90 days ahead. You can have up to 50 messages waiting to be sent at once, across all conversations. Attachments and artifact references cannot be scheduled, and a message you are editing cannot be scheduled.

## Who sees a scheduled message

Only you. Until it is sent, a scheduled message is not in the thread history, search results or unread counts, and agents and other people cannot see it. Your other browser tabs and devices show it too.

## Changing or cancelling

Click **Cancel** on the message's banner. If the composer is empty, the text goes back into it, so to change a scheduled message you cancel it, edit the text, and schedule it again. Once the Hub has started sending a message it can no longer be cancelled.

## When the message is sent

The Hub checks for due messages every 10 seconds, so a message is sent within about 10 seconds of its time. Nothing is decided from what was true when you scheduled it. When it sends, the Hub checks again that:

- your account still exists and is active;
- the topic still exists, and you can still read its project;
- each agent the message is routed to will accept a message from you.

Routing is worked out at that moment too: the topic's current default agent, the agents you @-mention, and the message you replied to (if it was deleted, the message is sent without the reply link). A scheduled message never interrupts an agent and never wakes a suspended one.

Each scheduled message is sent at most once.

## When a message is not sent

If a check fails, the message is not sent and its banner turns red with the reason:

| Reason | Meaning |
| :--- | :--- |
| You no longer have access | You can no longer post in this conversation, or an agent it is routed to no longer accepts messages from you. |
| This conversation no longer exists | The topic was deleted. |
| Your account is not active | Your account was suspended. |
| The scheduled time passed while the Hub was unavailable | The Hub found the message more than 60 minutes after its time, for example after downtime or while the experiment was off. It is not sent that late without asking you. |
| Delivery was interrupted | The Hub stopped while it was sending the message. It may or may not have reached the thread; check the thread before sending it again. The Hub never sends it again on its own. |
| Delivery failed | Any other error. |

A failed message offers:

- **Send now**: only for a missed or interrupted message. The Hub runs the same checks as when you schedule a message, then sends it within about 10 seconds, after checking again as described above.
- **Copy to composer**: puts the text into the composer, after any draft, so you can send or schedule it again.
- **Dismiss**: removes the message from the thread.

## Retention

The Hub keeps sent and cancelled scheduled messages for 7 days and failed ones for 30 days, then deletes them. Scheduled messages are also deleted when their topic is deleted or when your user account is deleted.

## Audit

The Hub records every scheduling, cancellation, Send now, dismissal and delivery outcome through its audit log, with you as the principal and `scheduled-send` as the executor. The records never contain the message text.
