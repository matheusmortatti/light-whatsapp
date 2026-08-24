# Polls (view-only)

## Context

WhatsApp polls arrive over the wire as two message kinds: a
`PollCreationMessage` (the poll itself — question + options) and, for
each vote cast, a separate `PollUpdateMessage` referencing the creation
message's key. Today `extractMessage` doesn't recognize either, so a
poll falls into the `unsupported` default branch and shows as a fake
message ("Unsupported message: poll creation v3") — see
[[project_unsupported_message_bare_fallback_2026-08-07]]. `PollUpdateMessage`
arrives as its own `*events.Message` the same way `ReactionMessage`
does, so it's silently dropped by the same unsupported-branch path
today too (or worse, would show as its own fake "unsupported" message
if it ever reached the default branch instead of being filtered).

The vendored `whatsmeow` (`core/go.mod`, pinned 2026-07-22) has full
native poll support: `BuildPollCreation`, `BuildPollVote`,
`DecryptPollVote`. The per-poll secret key needed to decrypt votes is
stored and retrieved automatically by whatsmeow's own store
(`Client.Store.MsgSecrets`) on send/receive/history-sync — no new
storage layer needed.

Chosen (2026-08-24) as the next small message-type feature, following
the same "view/receive first" pattern already used for reactions and
video/gif/sticker: view-only here, with poll *voting* and *creation*
from this app deferred as later, separately-scoped features.

## Non-goals

- Voting on a poll from this app.
- Creating a poll from this app.
- Poll closing/edit events (`EncSecretPollEdit`, `EncSecretPollAddOption`)
  — niche, no evidence yet they're common enough to prioritize.
- Showing *who* voted for what — WhatsApp itself only reveals this on
  tap into the poll; the approved tally format shows counts + a bar
  per option, not per-voter identity. The underlying data model still
  carries per-voter records (mirroring reactions), so this is a
  presentation choice, not a data gap — a later feature could surface
  it without a core/main.go change.
- A vote for a poll this app doesn't currently have cached (older than
  `maxMessagesPerChat`, or a chat never opened) — silently dropped,
  same as an unmatched reaction/receipt today.
- Backfilling vote tallies for polls pulled in via history sync.
  WhatsApp delivers historical reactions/votes as side-channel fields
  on `waWeb.WebMessageInfo` (`Reactions []*Reaction` field 41,
  `PollUpdates []*PollUpdate` field 45) rather than as `.Message`
  content — `extractHistoryMessage` doesn't read either field today,
  so historical *reactions* already don't show up after a fresh
  history sync (a pre-existing gap, not something this feature
  introduces). Poll votes get the same gap for v1: a poll pulled in by
  history sync renders with its question/options but a 0 tally until a
  vote arrives live from that point forward. Backfilling both via
  `WebMessageInfo.GetReactions()`/`GetPollUpdates()` is a fix that
  benefits reactions too and should be scoped as its own feature.

## Design

### `core/main.go`

- `extractMessage` gains a poll branch. WhatsApp has revised the poll
  protobuf field several times — `PollCreationMessage`, `V2`, `V3`,
  `V5`, `V6` all carry the real `*PollCreationMessage` payload (`V4` is
  a `FutureProofMessage` placeholder, not real content, and is
  skipped). The "poll creation v3" label already seen in the wild
  (see Context) confirms `V3` is what's actually sent today, so the
  branch must check all five real variants, not just the base field —
  a single case testing `m.GetPollCreationMessage() != nil ||
  m.GetPollCreationMessageV2() != nil || ... != nil` (or an equivalent
  helper that returns the first non-nil), not just
  `GetPollCreationMessage()`. Whichever variant matched, question
  becomes `text`, type becomes `"poll"`, options captured via a new
  `setPollFields(&cm, poll)` helper (same shape as `setImageFields`/
  `setVideoFields`), following the same one-more-return-value pattern
  the function already uses for image/audio/video/sticker.
- `chatMessage` gains:
  ```go
  PollOptions         []string       `json:"poll_options,omitempty"`
  PollSelectableCount int            `json:"poll_selectable_count,omitempty"`
  PollVotes           []chatPollVote `json:"poll_votes,omitempty"`
  ```
- New type, `chatReaction`'s shape with `Emoji` swapped for indices:
  ```go
  // chatPollVote is one person's current vote on a poll message, keyed
  // by Sender so a later vote from the same person replaces their
  // earlier one — WhatsApp allows re-voting, which fully replaces the
  // previous selection (not additive).
  type chatPollVote struct {
      Sender          string `json:"sender"`
      SenderName      string `json:"sender_name,omitempty"` // group chats only
      FromMe          bool   `json:"from_me,omitempty"`
      SelectedOptions []int  `json:"selected_options"` // indices into the poll message's PollOptions
  }
  ```
- `handleMessage` gets an early check alongside the existing
  `ReactionMessage` one: if `evt.Message.GetPollUpdateMessage() != nil`,
  delegate to a new `handlePollVote` and return — before the
  chat-bump/unread logic, same reasoning as reactions (a vote isn't a
  new message).
- New `handlePollVote(ctx, client, logger, evt, messages)`:
  - Look up the target poll message via
    `PollUpdateMessage.PollCreationMessageKey.ID` in the chat's cached
    list; no-op if absent (see Non-goals).
  - Call `client.DecryptPollVote(ctx, evt)` to get the `PollVoteMessage`
    (`SelectedOptions [][]byte`, SHA-256 hashes).
  - Compute `HashPollOptions(pollMsg.PollOptions)` for the cached poll
    and match each selected hash back to its option index.
  - Upsert into that message's `PollVotes` by
    `evt.Info.Sender.String()`, replacing any prior entry — same
    replace semantics `handleReaction` already uses.
  - Persist and emit `message_update` for the changed message, same
    pattern as `handleReaction`/`downloadMedia`.
- `setQuotedFields` gains a poll case (same all-variants check as
  above): `QuotedType = "poll"`, `QuotedText` = the poll question —
  same per-type dispatch already there for image/audio/video/sticker.
- `extractHistoryMessage` gains the matching poll-creation branch (same
  all-variants check) so backfilled polls parse the same way live ones
  do, with a 0 tally until a live vote arrives (see Non-goals —
  historical vote backfill is out of scope, matching the existing gap
  for historical reactions).
- Doc comments referencing the old scope need updating: `chatMessage`'s
  comment currently reads "Only text, image, and audio messages are
  represented — everything else (documents, polls, ...) is dropped" —
  drop "polls" from that list. The `command` struct's comment doesn't
  need a change (no new command is added by this feature).

### `app/` (`CoreProcess.kt`, `MainActivity.kt`)

- New `PollVote(sender, senderName, fromMe, selectedOptions: List<Int>)`
  data class; `Message` gains `pollOptions: List<String>`,
  `pollSelectableCount: Int`, `pollVotes: List<PollVote>` (empty
  list/0 defaults), parsed in `parseMessages`.
- New composable renders the poll bubble when `message.type == "poll"`,
  below the question text (which already renders via the existing
  `text` field):
  ```
  Best pizza topping? 📊
  ▸ Pepperoni      ■■■■□ 4
  ▸ Mushroom       ■□□□□ 1
  ▸ Pineapple      □□□□□ 0
  5 votes
  ```
  Per-option counts computed client-side from `pollVotes` via
  `flatMap { it.selectedOptions }.groupingBy { it }.eachCount()`, same
  client-side-aggregation idiom `formatReactions` already uses for
  reactions rather than precomputing counts in Go. Bar length scales to
  the highest count among the options (5 segments, matching the
  approved preview). "N votes" = `pollVotes.size` (distinct voters),
  not the sum of selections, since multi-select polls let one voter
  pick several options.
- `messagePreviewText`: new `"poll" -> "[Poll] " + text` case — reuses
  existing chat-list-row, quoted-reply-preview, and (if ever needed)
  reaction-picker-header call sites for free, same as every other type
  in that `when`.

## Testing

- Go: unit-test the new `extractMessage` poll branch and
  `handlePollVote`'s vote-tally aggregation — add, replace (same
  sender votes again with a different selection), target poll not
  found, matching the existing coverage style for `handleReaction` in
  `core/main_test.go`.
- Manual end-to-end: per [[feedback_device_testing_self_chat_only]], a
  poll sent to self-chat or from a second test account (not a real
  contact) — confirm the poll renders with correct options, a vote
  from another device updates the tally live without reopening the
  chat, a changed vote replaces (not adds to) the previous tally, and
  a poll quoted in a reply shows `[Poll] <question>`.
