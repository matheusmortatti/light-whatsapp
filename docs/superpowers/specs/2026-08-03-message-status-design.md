# Message delivery/read status (1:1 chats)

## Context

The chat thread (`app/`) currently shows a message you sent with no feedback
on whether it actually reached the other person. `core/main.go` already
handles `*events.Receipt` in `handleReadReceipt`, but only for the
"another linked device opened this chat" case
(`evt.MessageSource.IsFromMe == true`). Receipts about a message you sent
being delivered to or read by the actual recipient
(`evt.MessageSource.IsFromMe == false`) are received by whatsmeow but
silently dropped — that branch returns early today.

This was chosen (2026-08-03 brainstorming session) as the next small
thread-UX improvement over background notifications (parked — LightOS's
notification chrome is essentially invisible, see "Notifications
investigation" below) and over extending media types beyond images/audio.

## Notifications investigation (context, not in scope)

Before landing on this feature, we spiked background/backgrounded-app
notifications and tested directly on real LP3 hardware
(`LP3LHMA551300893`):

- LightOS's own SDK push path (`sdk/client/LightPushService.kt`,
  UnifiedPush-based) doesn't apply: it requires a remote server to originate
  the push (none exists here — `core/` runs whatsmeow locally), and it's
  only wired up for `light-sdk` Tool modules, not `app/`'s plain
  `ComponentActivity` (see `PROJECT.md`'s decision log for why `app/` isn't
  a Tool). The SDK README's own "Push notifications" section is `// TODO`.
- The only remaining path is standard Android `NotificationManager`. Tested
  on the real device: a crude `adb shell cmd notification post` produced a
  small icon/indicator with no drill-down. A proper in-app notification
  (real channel, `IMPORTANCE_HIGH`, `BigTextStyle`, tap `PendingIntent`)
  produced **no visible UI at all** — only a vibrate + chime.
- Conclusion: LightOS appears to suppress notification banners/icons
  entirely, exposing only the sound/haptic side effects. This changes what
  a "notifications" feature would even mean here (no preview content is
  ever shown), and needs more LightOS-side documentation/investigation
  before it's worth designing in full. Parked, not implemented.

## Goal

Show sent → delivered → read status on your own messages in 1:1 chats, so
you know whether a message you sent actually got through.

## Non-goals (this pass)

- Group chats: WhatsApp only shows "read" once *every* member has read a
  message, which needs per-recipient receipt aggregation. Group messages
  keep today's behavior (no status shown) — a possible follow-up, not part
  of this spec.
- A "sending…" transient state: `handleSendMessage` already blocks on
  `client.SendMessage` before the message is added to the cache/UI at all,
  so every message that appears has already reached "sent" (server-acked).
  No optimistic/pending state exists to represent.
- Failed-send handling: unchanged — already surfaced via the existing
  `"error"` event.

## Design

### `core/main.go`

- `chatMessage` gains a `Status string` field (`json:"status,omitempty"`):
  one of `"sent"`, `"delivered"`, `"read"`. Only meaningful for `FromMe`
  messages in non-group chats; omitted/ignored otherwise.
- `handleSendMessage` sets `Status: "sent"` on the `chatMessage` it
  constructs (right after `client.SendMessage` succeeds).
- `handleReadReceipt` gets a new branch for
  `evt.MessageSource.IsFromMe == false` (a receipt from the actual
  recipient, not our own read-sync) in a non-group chat:
  - Look up the chat's cached message list, match `evt.MessageIDs` by ID.
  - For each match, set `Status` to `"delivered"` on
    `types.ReceiptTypeDelivered`, or `"read"` on `types.ReceiptTypeRead` /
    `types.ReceiptTypeReadSelf` — never downgrade an existing `"read"` back
    to `"delivered"` (a delivered receipt can in principle arrive after a
    read one out of order).
  - Persist the updated message cache and re-emit a `"messages"` event for
    that chat so the UI updates live, same pattern `handleMessage` already
    follows.

### `app/` (`MainActivity.kt`)

- `Message` data class (wherever it's deserialized from the `"messages"`
  event) gains a nullable/default `status: String?`.
- In `ChatDetailScreen`, determine the most recent `fromMe` message in a
  1:1 chat (`!chat.isGroup`). Only that message's header shows a status
  suffix, appended after the timestamp: `You  14:32 · Read` (title-cased
  from `Status`; `"sent"` → `Sent`, etc).
- That one message's header is forced visible even if `showHeader` would
  otherwise hide it due to clustering — status only matters on the latest
  message, and it needs a header line to attach to. All earlier own
  messages, and all messages in group chats, render exactly as they do
  today (no status suffix, existing clustering logic untouched).

## Testing

- Go: unit-test the new `handleReadReceipt` branch (delivered → read
  transition, non-downgrade, group chats ignored, message-not-found is a
  no-op) with existing test patterns in `core/`.
- Manual end-to-end on the real LP3: send a 1:1 message, confirm it shows
  `Sent`; have the recipient's phone come online (delivered) and open the
  chat (read); confirm the status label updates live through both
  transitions without reopening the chat.
