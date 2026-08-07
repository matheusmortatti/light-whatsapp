# Reply messages (view-only)

## Context

WhatsApp replies arrive as an ordinary content message (almost always
`ExtendedTextMessage`, but also `ImageMessage`/`AudioMessage`/
`VideoMessage`/`StickerMessage` when replying with media) whose
`ContextInfo` field carries the quoted message: a stanza ID, the quoting
participant's JID, and — critically — a copy of the quoted message's own
content, embedded inline by the sending client. That means the quoted
preview is available immediately, with no fetch of the original message
required, even if this app never cached it.

Today `extractMessage` ignores `ContextInfo` entirely, so replies render
as an ordinary message with no indication they're replying to anything.

Chosen (2026-08-07, `/goal`) as the next thread-UX feature, **view-only**
per explicit scope — no reply *composing* UI or protocol call.

## Non-goals

- Composing/sending a reply from this app (quoting a message when
  sending) — a separate future feature.
- Tap-to-jump from the quoted snippet to the original message in the
  list — inert for this pass; may be worth adding once compose-a-reply
  exists and jump has a clearer use case.
- Resolving a quote whose original message isn't embedded in
  `ContextInfo` (e.g. `QuotedMessage` absent) beyond a generic
  "[Message]"-style fallback — WhatsApp clients always embed it for the
  message types this app supports, so this is a defensive fallback, not
  an expected path.
- Replies to types this app doesn't otherwise render (e.g. quoting a
  poll) — same `"unsupported"` handling the quoted content already gets
  via `extractMessage`'s existing default branch.

## Design

### `core/main.go`

- `extractMessage` gains a `*waE2E.ContextInfo` return value (6th of 7,
  before `ok`), pulled from whichever leaf content field is set —
  `GetContextInfo()` on `ExtendedTextMessage`/`ImageMessage`/
  `AudioMessage`/`VideoMessage`/`StickerMessage`. The plain
  `m.GetConversation() != ""` case returns `nil`: WhatsApp clients always
  upgrade a reply to `ExtendedTextMessage`, so a bare `Conversation`
  string is never a reply. Both call sites (`handleMessage`,
  `extractHistoryMessage`) are updated for the new return value.

- `chatMessage` gains 5 new `omitempty` fields:
  ```go
  // Set only when this message is a reply — see setQuotedFields.
  // QuotedID absent/empty means "not a reply".
  QuotedID         string `json:"quoted_id,omitempty"`
  QuotedFromMe     bool   `json:"quoted_from_me,omitempty"`
  QuotedSenderName string `json:"quoted_sender_name,omitempty"` // best-effort, blank if unknown or QuotedFromMe
  QuotedType       string `json:"quoted_type,omitempty"`        // same vocabulary as Type, or "unsupported"
  QuotedText       string `json:"quoted_text,omitempty"`        // body/caption preview, untruncated (UI truncates)
  ```

- New `setQuotedFields(ctx context.Context, client *whatsmeow.Client, cm *chatMessage, ci *waE2E.ContextInfo)`,
  called right after building `cm` in both `handleMessage` and
  `extractHistoryMessage` (mirrors the existing `setImageFields`/
  `setAudioFields`/etc. call pattern):
  - No-op if `ci == nil || ci.GetStanzaID() == ""`.
  - `cm.QuotedID = ci.GetStanzaID()`.
  - Recurse: `qText, qType, _, _, _, _, _, ok := extractMessage(ci.GetQuotedMessage())`.
    On success, `cm.QuotedType, cm.QuotedText = qType, qText` — reuses
    all of `extractMessage`'s existing type handling (including its
    4-hop ephemeral/view-once unwrap loop) for free. On failure (`!ok`,
    e.g. quoted content is a protocol message or absent), fall back to
    `cm.QuotedType = "unsupported"` with empty text.
  - Participant/self check, reusing the exact self-JID comparison
    `lookupMentionName` already uses: parse `ci.GetParticipant()`; if it
    matches `client.Store.ID`/`client.Store.GetLID()`, set
    `cm.QuotedFromMe = true`; otherwise set
    `cm.QuotedSenderName = cachedMentionName(ctx, client, pjid.User)`
    (already tries LID-then-PN, already cached — no new lookup code).
  - No server-side truncation of `QuotedText`, consistent with `Text`
    itself today — truncation is a display concern.

### `app/` (`CoreProcess.kt`)

- `Message` gains matching nullable/plain fields: `quotedId: String?`,
  `quotedFromMe: Boolean`, `quotedSenderName: String?`,
  `quotedType: String?`, `quotedText: String`. Parsed in `parseMessages`
  the same way every other optional field is (`optString(...).ifBlank { null }`,
  `optBoolean(..., false)`). `quotedType == null` means "not a reply".

### `app/` (`MainActivity.kt`)

- `messagePreviewText(message: Message)` is generalized to a
  `messagePreviewText(type: String, text: String)` overload it delegates
  to, so the same placeholder logic (`"[Photo]"`, `"[Voice message]"`,
  etc.) can render a quoted snippet's synthetic `(quotedType, quotedText)`
  pair, not just a real `Message`.

- `MessageRow` renders two extra small lines above the existing
  header/body, only when `message.quotedType != null`, inside the same
  outer `Column` (so the existing `lightClickable { onReact(message) }`
  still covers them — no new dead zone, no new tap target, matching the
  "inert" scope decision):
  - Line 1 (`ChatMetaText` style): sender label —
    `if (message.quotedFromMe) "You" else message.quotedSenderName ?: chatName`,
    the same fallback chain the real header already uses for a 1:1/group
    sender label.
  - Line 2 (`MessageBodyText` style, `lighten = true`, `maxLines = 1`,
    `TextOverflow.Ellipsis`): `messagePreviewText(message.quotedType, message.quotedText)`.
  - No border/bar/indent decoration — plain stacked text, matching the
    rest of the screen's plain-text LightOS look and avoiding the kind
    of custom-decoration glyph bugs hit previously with the reaction
    picker's underline (see `docs/superpowers/specs/2026-08-05-send-reactions-design.md`).

## Testing

- Go: unit-test `setQuotedFields` — reply to text, reply to image (with
  and without caption), reply from self vs. someone else, group vs. 1:1
  participant resolution, absent `ContextInfo` (not a reply), absent
  `QuotedMessage` payload (fallback to `"unsupported"`).
- Kotlin: extend `MergeMessagesTest`-style coverage or add a small
  `MessageRow`-adjacent test if reasonable; otherwise manual-only for the
  UI layer, consistent with how reactions/status were verified.
- Manual end-to-end on the real LP3 (per this project's
  chat-with-self-only testing rule): reply to a text message, an image,
  a voice message, and a sticker from another device; confirm the quote
  preview renders correctly for each, including in a group chat where
  the quoted sender differs from the message's own sender.
