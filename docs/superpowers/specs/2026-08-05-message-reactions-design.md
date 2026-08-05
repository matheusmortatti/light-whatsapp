# Message reactions (receive-only)

## Context

WhatsApp reactions arrive over the wire as an ordinary `*events.Message`
whose `Message.ReactionMessage` field is set instead of the usual
content fields. Today `extractMessage` doesn't recognize
`ReactionMessage`, so it falls into the `unsupported` default branch and
gets inserted into the chat as its own fake message
("Unsupported message: reaction"). This is the bug this feature fixes,
plus the actual feature: real reaction rendering under the message being
reacted to.

Chosen (2026-08-05, `/goal`) as the next small thread-UX feature,
receive-only per explicit scope — no reaction *sending* UI or protocol
call.

## Non-goals

- Sending/removing your own reactions from this app.
- `EncReactionMessage` (community-announcement-group encrypted
  reactions) — niche WhatsApp feature, needs a separate decrypt call
  (`Client.DecryptReaction`); out of scope.
- Reacting to a message this app doesn't currently have cached (older
  than `maxMessagesPerChat`, or a chat never opened) — silently
  dropped, same as an unmatched receipt today.
- Per-reactor identity in the UI (WhatsApp itself only reveals this on
  long-press) — just show which emoji, aggregated with a count.

## Design

### `core/main.go`

- `chatMessage` gains `Reactions []chatReaction` (`json:"reactions,omitempty"`).
- New type:
  ```go
  // chatReaction is one person's current reaction to a message, keyed by
  // Sender so a later reaction from the same person replaces (or, with
  // Emoji == "", removes) their earlier one — WhatsApp allows exactly one
  // active reaction per person per message.
  type chatReaction struct {
      Sender     string `json:"sender"`
      SenderName string `json:"sender_name,omitempty"` // group chats only
      FromMe     bool   `json:"from_me,omitempty"`
      Emoji      string `json:"emoji"`
  }
  ```
- `handleMessage` gets an early check: if `evt.Message.GetReactionMessage() != nil`,
  delegate to `handleReaction` and return — before the chat-bump/unread
  logic, since a reaction shouldn't bump the chat or count as unread.
- New `handleReaction(ctx, client, evt, messages)`:
  - Resolve the target message ID from `ReactionMessage.Key.ID`; a blank
    ID is a no-op.
  - Canonicalize `evt.Info.Chat`, same junk/broadcast filtering
    `handleMessage` already does.
  - Look up the chat's cached list, find the message by ID (no-op if
    absent).
  - Upsert into that message's `Reactions` by `evt.Info.Sender.String()`:
    a blank `ReactionMessage.Text` removes that sender's entry, anything
    else replaces it. `SenderName` set from `evt.Info.PushName` when
    `evt.Info.IsGroup && !evt.Info.IsFromMe`, matching how
    `handleMessage` already sets `chatMessage.Sender`/`SenderName`.
  - Persist and emit `message_update` for the changed message, same
    pattern as `downloadMedia`/`applyMessageStatus`.

### `app/` (`CoreProcess.kt`, `MainActivity.kt`)

- New `Reaction(sender, senderName, fromMe, emoji)` data class; `Message`
  gains `val reactions: List<Reaction>` (empty list default), parsed in
  `parseMessages`.
- `MessageRow` renders a reaction line under the bubble when
  `message.reactions.isNotEmpty()`, aligned the same side as the bubble:
  group by emoji, show each once with a `×N` suffix when count > 1
  (e.g. `👍 ❤️×2`), using the existing `ChatMetaText` style.

## Testing

- Go: unit-test `handleReaction` — add, replace (same sender reacts
  again with a different emoji), remove (blank text), target message not
  found, blank reaction target ID.
- Manual end-to-end on the real LP3: react to a message from another
  device/phone, confirm the emoji shows up live under the message;
  change the reaction; remove it; confirm all three update without
  reopening the chat.
