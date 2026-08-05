# Send reactions

## Context

Reactions today (see `2026-08-05-message-reactions-design.md`) are
receive-only by explicit scope — the app can show a reaction someone else
sent, but has no way to send one itself. Both `core/main.go`'s
`chatMessage.Reactions` doc comment and `app/`'s `Reaction` doc comment
say so outright.

Chosen (2026-08-05, `/goal`) as the next step: let the user react to any
message with WhatsApp's own standard quick-reaction set (👍 ❤️ 😂 😮 😢
🙏) — a real jump from "no reactions sendable at all," while staying
inside LightOS's minimal, tap-driven interaction model (the app has no
long-press anywhere today; everything is single-tap via `lightClickable`).

## Non-goals

- A full/open emoji picker (reusing the composer's embedded emoji
  keyboard layout) — the standard 6 is the agreed breadth for this pass.
- Long-press as a trigger — this app has no long-press gesture anywhere;
  tapping the message is the trigger, consistent with every other
  interaction (chat rows, video thumbnails).
- Reacting to a message not currently cached (older than
  `maxMessagesPerChat`, or a chat never opened) — the picker simply isn't
  reachable for a message the app can't render in the first place.
- Optimistic-then-reconciled UI — like `handleSendMessage`, the local
  cache is folded immediately on send-success rather than waiting for any
  server echo, so there's nothing to reconcile.

## Design

### `core/main.go`

- `command` gains `MessageID string` (`json:"message_id,omitempty"`) and
  `Emoji string` (`json:"emoji,omitempty"`); new `"send_reaction"` case in
  `readCommands`, requiring `JID`, `MessageID`, and allowing `Emoji == ""`
  (removal).
- New `handleSendReaction(ctx, client, logger, jidStr, messageID, emoji, messages)`:
  - Parse `jidStr`; look up the target message by `messageID` in
    `messages[jidStr]` under `messagesMu` — silent no-op if absent (same
    posture as `handleReaction`'s missing-target case).
  - Resolve the `sender` JID `BuildMessageKey` needs to identify the
    target message: `types.EmptyJID` if the target is our own message
    (`target.FromMe`), otherwise `jid` itself for a 1:1 chat, or
    `types.ParseJID(target.Sender)` for a group.
  - `client.SendMessage(ctx, jid, client.BuildReaction(jid, sender, messageID, emoji))`.
    On error, `emit(event{Type: "error", ...})`, matching
    `handleSendMessage`/`handleSendAudio`.
  - On success, fold the reaction into the local cache with the existing
    `applyReaction(cur.Reactions, client.Store.ID.String(), "", true, emoji)`
    (own JID as the key so a later removal matches; `SenderName` blank —
    `formatReactions` on the app side never uses it for `fromMe`
    reactions), persist, and `emit(event{Type: "message_update", ...})` —
    same shape as `handleReaction`.
- Drop the "sending a reaction isn't supported" clause from
  `chatMessage.Reactions`'s and `chatReaction`'s doc comments.

### `app/` (`CoreProcess.kt`, `QrLoginViewModel.kt`, `MainActivity.kt`)

- `CoreProcess.sendReaction(jid, messageId, emoji)`: writes
  `{"type":"send_reaction","jid":...,"message_id":...,"emoji":...}`, same
  shape as `sendMessage`/`sendAudio`.
- `QrLoginViewModel.sendReaction(messageId, emoji)`: resolves `jid` from
  `_selectedChat`, no-op if none open — mirrors `sendMessage`.
- Drop the "since sending a reaction isn't supported" clause from
  `Reaction`'s doc comment.
- `ChatDetailScreen` gains `onSendReaction: (String, String) -> Unit` and
  a `reactingTo: Message?` piece of per-chat screen state alongside
  `composing`/`recording`/`playingVideo`; `BackHandler` gets a
  `reactingTo != null -> reactingTo = null` branch (checked before the
  others, since it's the topmost modal when open).
- `MessageRow` gains `onReact: (Message) -> Unit`; its content `Column`
  (the `when (message.type)` block + reactions line, excluding the
  sender/time header) gets `.lightClickable { onReact(message) }`. This
  nests safely under the video thumbnail's and audio play button's own
  `lightClickable`s — Compose consumes a tap at the innermost clickable,
  so those keep working exactly as today, and tapping anywhere else on
  the message opens the picker.
- New `ReactionPickerScreen(message, chatName, onPick: (String) -> Unit, onClose: () -> Unit)`:
  - `LightTopBar` with a CLOSE `leftButton` and the chat name centered,
    matching `VideoPlayerScreen`'s pattern.
  - A short preview of the message being reacted to (its text, or a
    `[Photo]`/`[Voice message]`/etc. placeholder for non-text types,
    reusing the same labels `MessageRow` already uses).
  - The 6 standard emoji (👍 ❤️ 😂 😮 😢 🙏) in a centered row, each a
    plain large `LightText` — no icon assets needed, consistent with how
    a received reaction already renders as a raw emoji glyph via
    `formatReactions`. The emoji matching the message's current own
    reaction (`message.reactions.any { it.fromMe && it.emoji == e }`) is
    visually marked as selected (e.g. underlined).
  - Tapping the already-selected emoji calls `onPick("")` (removes);
    tapping any other emoji calls `onPick(emoji)`; either way the caller
    closes the picker (`reactingTo = null`) right after — no separate
    confirm step.
- Wiring: `onSendReaction = viewModel::sendReaction` at the
  `ChatDetailScreen` call site, `ReactionPickerScreen`'s `onPick` calls
  `onSendReaction(reactingTo.id, emoji)` then clears `reactingTo`.

## Testing

- Go: unit-test `handleSendReaction` — react to the peer's message in a
  1:1 chat, react to our own message, react to a group participant's
  message, remove an existing reaction (`emoji == ""`), target message
  not found.
- Manual end-to-end on the real LP3 (self-chat only, per standing
  practice — no test reactions to real contacts): open a message, react
  with each of the 6 emoji in turn, confirm each shows up under the
  message; tap the active one again and confirm it's removed; react to a
  message from the peer's side (if testable via a second linked device)
  and confirm it's indistinguishable in format from a receive-only
  reaction.
