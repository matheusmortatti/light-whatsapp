# Poll voting

## Context

Polls are currently view-only ([[project_poll_view_feature_2026-08-24]]):
`chatMessage` carries `PollOptions`, `PollSelectableCount`, and
`PollVotes` (upserted-by-sender), rendered as a passive tally
(`PollTallyRows`, `app/.../MainActivity.kt:1159-1191`). Voting from
this app was an explicit non-goal of that feature
(`docs/superpowers/specs/2026-08-24-poll-view-design.md:34`), deferred
as a follow-up. This spec covers that follow-up: letting the user
select/deselect poll options and send their vote.

Reactions (react/switch/remove,
[[project_send_reactions_feature_2026-08-05]]) are the closest shipped
analog for "pick from a small set, send a command to core, no
touchscreen" — this design mirrors that command/handler/local-fold-in
shape wherever the two features are structurally the same, and departs
from it where polls differ (multi-select, live inline UI instead of a
picker screen).

Native WhatsApp lets you change your poll vote at any time; the
underlying protocol vote is always a full **replacement** of your
selection (`BuildPollVote` takes the complete option set, not a
delta) — so does this feature: no explicit "confirm vote" step,
toggling an option sends the new complete selection immediately.

## Non-goals

- Creating a poll from this app (still out of scope, matching the
  view-only spec).
- Reacting (emoji) to a poll message. Currently any message row —
  polls included — opens the reaction picker on tap
  (`MainActivity.kt:952`). This feature repurposes that tap target for
  voting instead; emoji-reacting to a poll becomes unreachable. Low
  value, consistent with "voting is the poll bubble's primary action."
- An optimistic/local-first selection UI. The checkbox reflects
  `message.pollVotes`, which only updates once core's `message_update`
  event round-trips back through the same
  send-then-wait-for-echo pattern reactions already use
  (`handleSendReaction`, `core/main.go:1654-1714`) — no complaints
  about reaction latency on-device so far, so this feature doesn't add
  complexity to pre-empt a problem that hasn't shown up.
- Voting on a poll the app doesn't have cached, same non-goal as the
  view-only feature's vote-receiving side.

## Design

### `core/main.go`

- `command` (94-102) gains:
  ```go
  SelectedOptions []int `json:"selected_options,omitempty"`
  ```
  Reused with `MessageID` for the new command; `Emoji`/others stay as
  they are for existing commands.
- `readCommands`'s switch (1719-1755) gains:
  ```go
  case "send_poll_vote":
      if cmd.JID != "" && cmd.MessageID != "" {
          go handleSendPollVote(ctx, client, logger, cmd.JID, cmd.MessageID, cmd.SelectedOptions, messages)
      }
  ```
  An empty `SelectedOptions` is a valid retract-all-votes call (mirrors
  `chatPollVote`'s existing "empty = retract" convention) — the
  `cmd.MessageID != ""` guard alone is enough, no separate guard needed
  on `SelectedOptions` being non-empty.
- New `handleSendPollVote(ctx, client, logger, jidStr, messageID string, selectedOptions []int, messages map[string][]chatMessage)`,
  structured like `handleSendReaction` (1654-1714):
  1. Parse `jidStr` into `types.JID`.
  2. Lock `messagesMu`, find the target `chatMessage` by `messageID` in
     `messages[jidStr]`; no-op with a warn log if absent or if
     `msgType != "poll"`.
  3. Resolve the poll-creation message's original sender via
     `reactionSenderJID(jid, target)` (1529-1549) — reused exactly as
     written; its "whose message is this" logic is the same question
     for a poll-creation message as for a reacted-to message.
  4. Build the `types.MessageInfo` for the poll (`Chat: jid, Sender:
     <resolved above>, IsFromMe: target.FromMe, IsGroup: jid.Server ==
     types.GroupServer, ID: messageID`).
  5. Map `selectedOptions` indices to option-name strings via
     `target.PollOptions`, skipping any out-of-range index defensively
     (client-side already won't send one, but core doesn't trust it).
  6. `client.BuildPollVote(ctx, pollInfo, optionNames)` then
     `client.SendMessage(ctx, jid, voteMsg)`.
  7. On success: re-lock `messagesMu`, re-find the message, compute
     `ownKey` the same way `handleSendReaction` does (1691-1693), and
     `cur.PollVotes = applyPollVote(cur.PollVotes, ownKey, "", true, selectedOptions)`
     — the same upsert helper `handlePollVote` already uses for
     incoming votes (2182-2203).
  8. Persist (`saveMessages`) and emit `event{Type: "message_update",
     JID: jidStr, Messages: [...]}`, same as `handleSendReaction`.

### `app/` (`CoreProcess.kt`, `MainActivity.kt`)

- New `CoreProcess.sendPollVote(jid: String, messageId: String, selectedOptions: List<Int>)`,
  same shape as `sendReaction` (230-238):
  ```kotlin
  fun sendPollVote(jid: String, messageId: String, selectedOptions: List<Int>) {
      writeCommand(
          JSONObject()
              .put("type", "send_poll_vote")
              .put("jid", jid)
              .put("message_id", messageId)
              .put("selected_options", JSONArray(selectedOptions)),
      )
  }
  ```
  `QrLoginViewModel` gets a matching `sendPollVote(messageId, selectedOptions)`
  wrapper resolving the open chat's JID, mirroring `sendReaction`'s wrapper.
- `MessageRow` (935-953): the outer `.lightClickable { onReact(message) }`
  is applied conditionally — skipped when `message.type == "poll"`, so
  tapping the header/quote/body area of a poll message no longer opens
  the reaction picker (see Non-goals).
- `PollTallyRows` (currently passive, 1159-1191) becomes interactive,
  taking the message's own current selection and a toggle callback:
  ```kotlin
  PollTallyRows(
      options = message.pollOptions,
      votes = message.pollVotes,
      selectableCount = message.pollSelectableCount,
      ownSelection = message.pollVotes.firstOrNull { it.fromMe }?.selectedOptions ?: emptyList(),
      onToggle = { index -> onTogglePollOption(message, index) },
      bodyAlign = bodyAlign,
  )
  ```
  Each option row is individually wrapped in its own
  `.lightClickable { onToggle(index) }`, nested inside the row the same
  way the video thumbnail's play button already nests inside the outer
  row's click target (`MainActivity.kt:1053` inside the row wrapped at
  952) — same proven pattern, no new interaction primitive.
  The leading `"▸ "` per-option prefix becomes a checkbox glyph
  reflecting `ownSelection`: `"☑ "` if `index in ownSelection`, else
  `"☐ "`. Bar/count column stays exactly as-is (unaffected by this
  feature, already fixed-width/aligned per the prior bar-alignment
  fix).
- Toggle semantics, computed where `onTogglePollOption` is handled
  (`ChatDetailScreen`, alongside `onSendReaction`):
  - If `index` is currently selected: remove it from the set.
  - Else if the current selection size `< message.pollSelectableCount`:
    add it.
  - Else (already at the cap and tapping a new, unselected option):
    ignored — no-op, no command sent. The user must deselect one first.
    (`pollSelectableCount` is always ≥ 1, so a single-select poll's
    "tap a different option" is naturally covered: at cap 1, tapping a
    new option is a no-op — matches native WhatsApp single-choice
    behavior of needing to deselect first, and keeps the toggle rule
    uniform across select-count instead of special-casing
    single-select as "replace.")
  - In every non-no-op case, call `onSendPollVote(message.id, newSelection)`
    immediately — full replacement set, no confirm step.
- `messagePreviewText`'s existing `"poll" -> "[Poll] " + text"` case is
  unaffected.

## Testing

- Go: unit-test `handleSendPollVote` — first vote, vote replacement
  (send again with a different set), retract (empty set), target
  message not found, target message not a poll, matching the existing
  `handleSendReaction`/`handlePollVote` test coverage style in
  `core/main_test.go`.
- Manual end-to-end, per [[feedback_device_testing_self_chat_only]]:
  self-chat or a second test account — vote on a poll and confirm the
  tally updates live (own row's checkbox flips, bar/count update);
  toggle a second option on a multi-select poll and confirm both
  register; toggle an already-selected option off and confirm the vote
  retracts; confirm tapping a poll message no longer opens the
  reaction picker; confirm a single-select poll won't let a second
  option get checked without first unchecking the first one.
