# Reply Messages (View-Only) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Render WhatsApp replies with a quoted-message preview above the reply body, view-only (no compose/send-a-reply UI, no tap-to-jump).

**Architecture:** `core/`'s `extractMessage` starts returning the sending client's embedded `ContextInfo` alongside the content it already extracts; a new `setQuotedFields` helper turns that into 5 new `chatMessage` fields (quoted id/sender/type/text), reusing `extractMessage` itself recursively to decode the quoted content and reusing the existing LID/PN self- and contact-name resolution already used for @-mentions. The Kotlin side parses those 5 fields onto `Message` and `MessageRow` renders a small two-line quote block (sender label + one-line lightened preview) above the reply's own header/body, inside the row's existing tap target.

**Tech Stack:** Go (`core/main.go`, whatsmeow `waE2E` protobufs), Kotlin/Jetpack Compose (`app/`).

## Global Constraints

- View-only: no reply-composing/sending UI or protocol call in this plan.
- Tapping the quote block is inert — no scroll-to-original, no new tap target (it stays inside the row's existing `onReact` click area).
- No server-side truncation of the quoted preview text — same as `chatMessage.Text` today; truncation (ellipsis) is a UI concern.
- No border/bar decoration on the quote block — plain stacked text only (`ChatMetaText` + `MessageBodyText` styles), matching the app's existing plain-text look and avoiding the custom-decoration glyph issues hit previously with the reaction picker's underline.
- Reuse existing name-resolution helpers (`cachedMentionName`, the self-JID check `lookupMentionName` already does) — no new contact-lookup code paths.
- On-device verification uses chat-with-self only, never a real contact (project rule).

---

### Task 1: `extractMessage` returns the message's `ContextInfo`

**Files:**
- Modify: `core/main.go:433-469` (`extractMessage`)
- Modify: `core/main.go:696` (`extractHistoryMessage`'s call site)
- Modify: `core/main.go:1857` (`handleMessage`'s call site)
- Test: `core/main_test.go:355-387` (`TestExtractMessage`)

**Interfaces:**
- Produces: `extractMessage(m *waE2E.Message) (text, msgType string, img *waE2E.ImageMessage, audio *waE2E.AudioMessage, video *waE2E.VideoMessage, sticker *waE2E.StickerMessage, ci *waE2E.ContextInfo, ok bool)` — one new return value, `ci`, 7th of 8, `nil` whenever the matched content type carries no `ContextInfo` (the plain-`Conversation` case) or nothing matched.

- [ ] **Step 1: Update `TestExtractMessage`'s existing subtests for the new return arity, and add two new subtests for `ci`**

Edit `core/main_test.go`, replacing the whole `TestExtractMessage` function (lines 355-387) with:

```go
func TestExtractMessage(t *testing.T) {
	t.Run("video message", func(t *testing.T) {
		msg := &waE2E.Message{VideoMessage: &waE2E.VideoMessage{Caption: proto.String("look")}}
		text, msgType, _, _, video, sticker, _, ok := extractMessage(msg)
		if !ok || msgType != "video" || text != "look" || video == nil || sticker != nil {
			t.Fatalf("extractMessage() = text=%q type=%q video=%v sticker=%v ok=%v", text, msgType, video, sticker, ok)
		}
	})

	t.Run("gif is a video message with GifPlayback set", func(t *testing.T) {
		msg := &waE2E.Message{VideoMessage: &waE2E.VideoMessage{GifPlayback: proto.Bool(true)}}
		_, msgType, _, _, video, _, _, ok := extractMessage(msg)
		if !ok || msgType != "gif" || video == nil {
			t.Fatalf("extractMessage() = type=%q video=%v ok=%v", msgType, video, ok)
		}
	})

	t.Run("sticker message", func(t *testing.T) {
		msg := &waE2E.Message{StickerMessage: &waE2E.StickerMessage{IsAnimated: proto.Bool(true)}}
		_, msgType, _, _, _, sticker, _, ok := extractMessage(msg)
		if !ok || msgType != "sticker" || sticker == nil {
			t.Fatalf("extractMessage() = type=%q sticker=%v ok=%v", msgType, sticker, ok)
		}
	})

	t.Run("lottie sticker falls back to unsupported", func(t *testing.T) {
		msg := &waE2E.Message{StickerMessage: &waE2E.StickerMessage{IsLottie: proto.Bool(true)}}
		text, msgType, _, _, _, sticker, _, ok := extractMessage(msg)
		if !ok || msgType != "unsupported" || text != "lottie sticker" || sticker != nil {
			t.Fatalf("extractMessage() = text=%q type=%q sticker=%v ok=%v", text, msgType, sticker, ok)
		}
	})

	t.Run("extended text message carries its ContextInfo", func(t *testing.T) {
		ci := &waE2E.ContextInfo{StanzaID: proto.String("s1")}
		msg := &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{Text: proto.String("hi"), ContextInfo: ci}}
		text, msgType, _, _, _, _, gotCi, ok := extractMessage(msg)
		if !ok || msgType != "text" || text != "hi" || gotCi.GetStanzaID() != "s1" {
			t.Fatalf("extractMessage() = text=%q type=%q ci=%v ok=%v", text, msgType, gotCi, ok)
		}
	})

	t.Run("plain conversation text carries no ContextInfo", func(t *testing.T) {
		msg := &waE2E.Message{Conversation: proto.String("hi")}
		_, msgType, _, _, _, _, gotCi, ok := extractMessage(msg)
		if !ok || msgType != "text" || gotCi != nil {
			t.Fatalf("extractMessage() = type=%q ci=%v ok=%v", msgType, gotCi, ok)
		}
	})
}
```

- [ ] **Step 2: Run the test to verify it fails to compile (arity mismatch against the still-unmodified `extractMessage`)**

Run: `cd core && go test ./... -run TestExtractMessage -v`
Expected: build FAILs — `too many return values` / `not enough return values` referencing `extractMessage`.

- [ ] **Step 3: Update `extractMessage` itself**

Edit `core/main.go`, replacing the whole function (lines 433-469) with:

```go
func extractMessage(m *waE2E.Message) (text, msgType string, img *waE2E.ImageMessage, audio *waE2E.AudioMessage, video *waE2E.VideoMessage, sticker *waE2E.StickerMessage, ci *waE2E.ContextInfo, ok bool) {
	for i := 0; i < 4 && m != nil; i++ {
		switch {
		case m.GetConversation() != "":
			return m.GetConversation(), "text", nil, nil, nil, nil, nil, true
		case m.GetExtendedTextMessage() != nil:
			etm := m.GetExtendedTextMessage()
			return etm.GetText(), "text", nil, nil, nil, nil, etm.GetContextInfo(), true
		case m.GetImageMessage() != nil:
			im := m.GetImageMessage()
			return im.GetCaption(), "image", im, nil, nil, nil, im.GetContextInfo(), true
		case m.GetAudioMessage() != nil:
			am := m.GetAudioMessage()
			return "", "audio", nil, am, nil, nil, am.GetContextInfo(), true
		case m.GetVideoMessage() != nil:
			vm := m.GetVideoMessage()
			vType := "video"
			if vm.GetGifPlayback() {
				vType = "gif"
			}
			return vm.GetCaption(), vType, nil, nil, vm, nil, vm.GetContextInfo(), true
		case m.GetStickerMessage() != nil && !m.GetStickerMessage().GetIsLottie():
			sm := m.GetStickerMessage()
			return "", "sticker", nil, nil, nil, sm, sm.GetContextInfo(), true
		case m.GetEphemeralMessage() != nil:
			m = m.GetEphemeralMessage().GetMessage()
		case m.GetViewOnceMessage() != nil:
			m = m.GetViewOnceMessage().GetMessage()
		case m.GetViewOnceMessageV2() != nil:
			m = m.GetViewOnceMessageV2().GetMessage()
		case m.GetProtocolMessage() != nil:
			// Internal plumbing (history-sync notifications, app-state key
			// distribution, ephemeral-setting changes, revokes, ...), never
			// user-authored content. Drop instead of showing as unsupported.
			return "", "", nil, nil, nil, nil, nil, false
		default:
			return unsupportedMessageLabel(m), "unsupported", nil, nil, nil, nil, nil, true
		}
	}
	return "", "", nil, nil, nil, nil, nil, false
}
```

- [ ] **Step 4: Fix the two call sites so the package still compiles**

In `core/main.go:696` (inside `extractHistoryMessage`), change:
```go
	text, msgType, img, audio, video, sticker, ok := extractMessage(waMsg)
```
to:
```go
	text, msgType, img, audio, video, sticker, ci, ok := extractMessage(waMsg)
```
and immediately silence the now-unused `ci` for this step only by leaving it referenced nowhere yet — Go will report `ci declared and not used`. Fix that by adding `_ = ci` directly below the call for now; Task 3 replaces this line with the real `setQuotedFields` call.

In `core/main.go:1857` (inside `handleMessage`), make the same two changes: rename the destructured var and add `_ = ci` below it.

- [ ] **Step 5: Run the full test suite and verify it passes**

Run: `cd core && go build ./... && go test ./... -v`
Expected: PASS, including the two new `TestExtractMessage` subtests.

- [ ] **Step 6: Commit**

```bash
git add core/main.go core/main_test.go
git commit -m "core: extractMessage returns the message's ContextInfo"
```

---

### Task 2: `chatMessage` quoted fields + `setQuotedFields`

**Files:**
- Modify: `core/main.go:107-186` (`chatMessage` struct)
- Modify: `core/main.go:603-614` (`lookupMentionName` — factor out `isSelfUser`)
- Modify: `core/main.go` (new `isSelfUser` and `setQuotedFields` functions, placed near `setStickerFields`, i.e. after line 681)
- Test: `core/main_test.go` (new `TestSetQuotedFields`, new fake `store.ContactStore` test double)

**Interfaces:**
- Consumes: `extractMessage` from Task 1 (`text, msgType, img, audio, video, sticker, ci, ok := extractMessage(m)`); `cachedMentionName(ctx, client, user string) string` (existing); `contactName(ctx, client, jid types.JID) string` (existing).
- Produces: `isSelfUser(client *whatsmeow.Client, user string) bool`; `setQuotedFields(ctx context.Context, client *whatsmeow.Client, cm *chatMessage, ci *waE2E.ContextInfo)`; `chatMessage.QuotedID/QuotedFromMe/QuotedSenderName/QuotedType/QuotedText`.

- [ ] **Step 1: Write the failing test**

Append to `core/main_test.go`:

```go
// fakeContactStore is a minimal store.ContactStore test double: GetContact
// reports a fixed name for one specific JID and "not found" for everything
// else, letting TestSetQuotedFields exercise the real contact-lookup path
// (cachedMentionName -> contactName -> client.Store.Contacts.GetContact)
// without a real SQLite-backed store.
type fakeContactStore struct {
	known types.JID
	name  string
}

func (f fakeContactStore) PutPushName(ctx context.Context, user types.JID, pushName string) (bool, string, error) {
	return false, "", nil
}
func (f fakeContactStore) PutBusinessName(ctx context.Context, user types.JID, businessName string) (bool, string, error) {
	return false, "", nil
}
func (f fakeContactStore) PutContactName(ctx context.Context, user types.JID, fullName, firstName string) error {
	return nil
}
func (f fakeContactStore) PutAllContactNames(ctx context.Context, contacts []store.ContactEntry) error {
	return nil
}
func (f fakeContactStore) PutManyRedactedPhones(ctx context.Context, entries []store.RedactedPhoneEntry) error {
	return nil
}
func (f fakeContactStore) GetContact(ctx context.Context, user types.JID) (types.ContactInfo, error) {
	if user == f.known {
		return types.ContactInfo{Found: true, FullName: f.name}, nil
	}
	return types.ContactInfo{Found: false}, nil
}
func (f fakeContactStore) GetAllContacts(ctx context.Context) (map[types.JID]types.ContactInfo, error) {
	return nil, nil
}

func TestSetQuotedFields(t *testing.T) {
	ownJID := types.NewJID("111", types.DefaultUserServer)
	otherJID := types.NewJID("222", types.DefaultUserServer)

	newClient := func() *whatsmeow.Client {
		return whatsmeow.NewClient(&store.Device{
			ID:       &ownJID,
			Contacts: fakeContactStore{known: types.NewJID("222", types.HiddenUserServer), name: "Alice"},
		}, waLog.Noop)
	}

	t.Run("nil ContextInfo is a no-op", func(t *testing.T) {
		cm := chatMessage{ID: "m1"}
		setQuotedFields(context.Background(), newClient(), &cm, nil)
		if cm.QuotedID != "" || cm.QuotedType != "" {
			t.Fatalf("expected no quoted fields set, got %+v", cm)
		}
	})

	t.Run("blank stanza id is a no-op", func(t *testing.T) {
		cm := chatMessage{ID: "m1"}
		setQuotedFields(context.Background(), newClient(), &cm, &waE2E.ContextInfo{})
		if cm.QuotedID != "" || cm.QuotedType != "" {
			t.Fatalf("expected no quoted fields set, got %+v", cm)
		}
	})

	t.Run("reply to our own text message sets QuotedFromMe", func(t *testing.T) {
		cm := chatMessage{ID: "m1"}
		ci := &waE2E.ContextInfo{
			StanzaID:      proto.String("orig1"),
			Participant:   proto.String(ownJID.String()),
			QuotedMessage: &waE2E.Message{Conversation: proto.String("Sure, sounds good")},
		}
		setQuotedFields(context.Background(), newClient(), &cm, ci)
		if cm.QuotedID != "orig1" || !cm.QuotedFromMe || cm.QuotedSenderName != "" {
			t.Fatalf("got %+v", cm)
		}
		if cm.QuotedType != "text" || cm.QuotedText != "Sure, sounds good" {
			t.Fatalf("got QuotedType=%q QuotedText=%q", cm.QuotedType, cm.QuotedText)
		}
	})

	t.Run("reply to someone else's message resolves their name via the contact store", func(t *testing.T) {
		cm := chatMessage{ID: "m1"}
		ci := &waE2E.ContextInfo{
			StanzaID:      proto.String("orig2"),
			Participant:   proto.String(otherJID.String()),
			QuotedMessage: &waE2E.Message{ImageMessage: &waE2E.ImageMessage{Caption: proto.String("nice")}},
		}
		setQuotedFields(context.Background(), newClient(), &cm, ci)
		if cm.QuotedFromMe {
			t.Fatalf("expected QuotedFromMe=false, got %+v", cm)
		}
		if cm.QuotedSenderName != "Alice" {
			t.Fatalf("expected QuotedSenderName=Alice, got %q", cm.QuotedSenderName)
		}
		if cm.QuotedType != "image" || cm.QuotedText != "nice" {
			t.Fatalf("got QuotedType=%q QuotedText=%q", cm.QuotedType, cm.QuotedText)
		}
	})

	t.Run("quoted message with no embedded payload falls back to unsupported", func(t *testing.T) {
		cm := chatMessage{ID: "m1"}
		ci := &waE2E.ContextInfo{
			StanzaID:    proto.String("orig3"),
			Participant: proto.String(ownJID.String()),
		}
		setQuotedFields(context.Background(), newClient(), &cm, ci)
		if cm.QuotedType != "unsupported" || cm.QuotedText != "" {
			t.Fatalf("got QuotedType=%q QuotedText=%q", cm.QuotedType, cm.QuotedText)
		}
	})

	t.Run("quoted protocol message falls back to unsupported", func(t *testing.T) {
		cm := chatMessage{ID: "m1"}
		ci := &waE2E.ContextInfo{
			StanzaID:      proto.String("orig4"),
			Participant:   proto.String(ownJID.String()),
			QuotedMessage: &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{}},
		}
		setQuotedFields(context.Background(), newClient(), &cm, ci)
		if cm.QuotedType != "unsupported" {
			t.Fatalf("got QuotedType=%q", cm.QuotedType)
		}
	})
}
```

- [ ] **Step 2: Run the test to verify it fails to compile**

Run: `cd core && go test ./... -run TestSetQuotedFields -v`
Expected: build FAILs — `undefined: setQuotedFields` (and `chatMessage` has no field `QuotedID` etc.).

- [ ] **Step 3: Add the 5 new fields to `chatMessage`**

Edit `core/main.go`, adding after the `Reactions` field (end of the struct, before the closing `}` at line 186):

```go

	// Set only when this message is a reply — see setQuotedFields. QuotedID
	// absent/empty means "not a reply". QuotedType uses the same vocabulary
	// as Type, or "unsupported" if the quoted content couldn't be decoded
	// (e.g. no payload embedded in ContextInfo). QuotedText is untruncated,
	// same as Text — truncation is a display concern.
	QuotedID         string `json:"quoted_id,omitempty"`
	QuotedFromMe     bool   `json:"quoted_from_me,omitempty"`
	QuotedSenderName string `json:"quoted_sender_name,omitempty"`
	QuotedType       string `json:"quoted_type,omitempty"`
	QuotedText       string `json:"quoted_text,omitempty"`
```

- [ ] **Step 4: Factor out `isSelfUser` and add `setQuotedFields`**

Edit `core/main.go`, replacing `lookupMentionName` (lines 603-614) with:

```go
// isSelfUser reports whether user (a bare JID user part — digits for a
// phone-number JID, an opaque id for a lid) refers to this device's own
// account, checked against both its phone-number and lid identities — the
// same LID/PN duality resolveMentions navigates for @-mentions.
func isSelfUser(client *whatsmeow.Client, user string) bool {
	return (client.Store.ID != nil && user == client.Store.ID.User) || user == client.Store.GetLID().User
}

func lookupMentionName(ctx context.Context, client *whatsmeow.Client, user string) string {
	if name := contactName(ctx, client, types.NewJID(user, types.HiddenUserServer)); name != "" {
		return name
	}
	if name := contactName(ctx, client, types.NewJID(user, types.DefaultUserServer)); name != "" {
		return name
	}
	if client.Store.PushName != "" && isSelfUser(client, user) {
		return client.Store.PushName
	}
	return ""
}
```

Then add `setQuotedFields` after `setStickerFields` (after line 681):

```go

// setQuotedFields fills in cm's reply-preview fields from ci, the
// ContextInfo of whichever content field extractMessage matched — a no-op
// if ci is nil or carries no stanza ID (i.e. this message isn't a reply).
// The quoted content itself is decoded via a recursive extractMessage call
// on ci.GetQuotedMessage(), reusing all of its existing type handling; a
// nil/undecodable quoted payload (e.g. a quoted protocol message) falls
// back to QuotedType "unsupported" with no text, the same label
// extractMessage itself would give an unsupported top-level message.
func setQuotedFields(ctx context.Context, client *whatsmeow.Client, cm *chatMessage, ci *waE2E.ContextInfo) {
	if ci == nil || ci.GetStanzaID() == "" {
		return
	}
	cm.QuotedID = ci.GetStanzaID()

	qText, qType, _, _, _, _, _, ok := extractMessage(ci.GetQuotedMessage())
	if ok {
		cm.QuotedType = qType
		cm.QuotedText = qText
	} else {
		cm.QuotedType = "unsupported"
	}

	participant := ci.GetParticipant()
	if participant == "" {
		return
	}
	pjid, err := types.ParseJID(participant)
	if err != nil {
		return
	}
	if isSelfUser(client, pjid.User) {
		cm.QuotedFromMe = true
	} else {
		cm.QuotedSenderName = cachedMentionName(ctx, client, pjid.User)
	}
}
```

- [ ] **Step 5: Add the `store` import needed by the new test double**

`core/main_test.go` already imports `"go.mau.fi/whatsmeow/store"` (used by `TestCanonicalizeChatJIDBeforePairing`) — no import change needed. Confirm this before moving on: `grep -n '"go.mau.fi/whatsmeow/store"' core/main_test.go` should print one line.

- [ ] **Step 6: Run the full test suite and verify it passes**

Run: `cd core && go build ./... && go test ./... -v`
Expected: PASS, including all `TestSetQuotedFields` subtests.

- [ ] **Step 7: Commit**

```bash
git add core/main.go core/main_test.go
git commit -m "core: add setQuotedFields and chatMessage quoted-reply fields"
```

---

### Task 3: Wire `setQuotedFields` into `handleMessage` and `extractHistoryMessage`

**Files:**
- Modify: `core/main.go:696-753` (`extractHistoryMessage`)
- Modify: `core/main.go:1857-1917` (`handleMessage`)

**Interfaces:**
- Consumes: `setQuotedFields(ctx, client, cm *chatMessage, ci *waE2E.ContextInfo)` from Task 2; the `ci` return value `extractMessage` now produces (Task 1), already destructured with a placeholder `_ = ci` in Task 1 Step 4.

- [ ] **Step 1: Wire it into `extractHistoryMessage`**

In `core/main.go`, inside `extractHistoryMessage`, remove the `_ = ci` placeholder line added in Task 1 Step 4 and, immediately after the block that builds `cm` and before the `if key.GetFromMe() && jid.Server != types.GroupServer {` status block (i.e. right after the `setStickerFields` conditional around line 719), add:

```go
	setQuotedFields(ctx, client, &cm, ci)
```

- [ ] **Step 2: Wire it into `handleMessage`**

In `core/main.go`, inside `handleMessage`, remove that function's `_ = ci` placeholder and, immediately after the block that builds `cm` (right after the `setStickerFields` conditional around line 1879) and before the `if evt.Info.IsFromMe && !evt.Info.IsGroup {` status block, add:

```go
	setQuotedFields(ctx, client, &cm, ci)
```

- [ ] **Step 3: Build and run the full Go test suite**

Run: `cd core && go build ./... && go vet ./... && go test ./... -v`
Expected: PASS, no new failures. (No new unit tests in this task — `handleMessage`/`extractHistoryMessage` have no existing direct unit-test coverage in this codebase; this wiring is exercised by Task 4's manual end-to-end pass.)

- [ ] **Step 4: Commit**

```bash
git add core/main.go
git commit -m "core: populate quoted-reply fields on live and history-sync messages"
```

---

### Task 4: Kotlin — parse quoted fields and render the quote block

**Files:**
- Modify: `app/src/main/kotlin/com/matheusmortatti/lightwhatsapp/CoreProcess.kt:26-67` (`Message`), `:260-287` (`parseMessages`)
- Modify: `app/src/main/kotlin/com/matheusmortatti/lightwhatsapp/MainActivity.kt:829-837` (`messagePreviewText`), `:928-1082` (`MessageRow`), `:1141-1155` (`MessageBodyText`)
- Modify: `app/src/test/kotlin/com/matheusmortatti/lightwhatsapp/MergeMessagesTest.kt:6-26` (`testMessage` helper — compile fix for the new required `Message` fields)

**Interfaces:**
- Consumes: JSON fields `quoted_id`, `quoted_from_me`, `quoted_sender_name`, `quoted_type`, `quoted_text` from `core/main.go`'s `chatMessage` (Task 2).
- Produces: `Message.quotedId: String?`, `.quotedFromMe: Boolean`, `.quotedSenderName: String?`, `.quotedType: String?`, `.quotedText: String`; `messagePreviewText(type: String, text: String): String` overload.

- [ ] **Step 1: Add the 5 fields to `Message` and update `MergeMessagesTest`'s `testMessage` helper (compile fix first, TDD-style — this makes the currently-passing test suite fail to compile, which we then fix in the same step since there's no new behavior to test here yet)**

Edit `app/src/main/kotlin/com/matheusmortatti/lightwhatsapp/CoreProcess.kt`, in the `Message` data class, after the `reactions` field (end of the class, before the closing `)` at line 67):

```kotlin
    val reactions: List<Reaction>,
    // Set only when this message is a reply — see core/main.go's
    // chatMessage.QuotedID. quotedType == null means "not a reply".
    // quotedSenderName is null when quotedFromMe is true, or when the
    // quoting participant's name isn't known yet.
    val quotedId: String?,
    val quotedFromMe: Boolean,
    val quotedSenderName: String?,
    val quotedType: String?,
    val quotedText: String,
)
```

Edit `app/src/test/kotlin/com/matheusmortatti/lightwhatsapp/MergeMessagesTest.kt`'s `testMessage` helper, adding the 5 new args after `reactions = emptyList(),`:

```kotlin
    reactions = emptyList(),
    quotedId = null,
    quotedFromMe = false,
    quotedSenderName = null,
    quotedType = null,
    quotedText = "",
)
```

- [ ] **Step 2: Run the Kotlin unit tests to verify compile + pass**

Run: `./gradlew :app:testDebugUnitTest`
Expected: PASS (build succeeds, `MergeMessagesTest` passes unchanged).

- [ ] **Step 3: Parse the 5 fields in `parseMessages`**

Edit `app/src/main/kotlin/com/matheusmortatti/lightwhatsapp/CoreProcess.kt`, in `parseMessages`, after `reactions = parseReactions(o.optJSONArray("reactions")),` (end of the `Message(...)` constructor call, before its closing `)`):

```kotlin
                reactions = parseReactions(o.optJSONArray("reactions")),
                quotedId = o.optString("quoted_id").ifBlank { null },
                quotedFromMe = o.optBoolean("quoted_from_me", false),
                quotedSenderName = o.optString("quoted_sender_name").ifBlank { null },
                quotedType = o.optString("quoted_type").ifBlank { null },
                quotedText = o.optString("quoted_text"),
```

- [ ] **Step 4: Generalize `messagePreviewText` to accept a raw (type, text) pair**

Edit `app/src/main/kotlin/com/matheusmortatti/lightwhatsapp/MainActivity.kt`, replacing the existing function (lines 829-837):

```kotlin
private fun messagePreviewText(message: Message): String = messagePreviewText(message.type, message.text)

// Same placeholder logic as the Message overload above, but usable on a
// synthetic (type, text) pair — specifically MessageRow's quoted-reply
// preview, which has a quotedType/quotedText but no full Message for the
// message it's quoting.
private fun messagePreviewText(type: String, text: String): String = when (type) {
    "image" -> text.ifBlank { "[Photo]" }
    "sticker" -> "[Sticker]"
    "video" -> text.ifBlank { "[Video]" }
    "gif" -> text.ifBlank { "[GIF]" }
    "audio" -> "[Voice message]"
    "unsupported" -> "[Unsupported message]"
    else -> text
}
```

- [ ] **Step 5: Give `MessageBodyText` optional `maxLines`/`overflow` (default-preserving, needed for the quote line's single-line ellipsis)**

Edit `app/src/main/kotlin/com/matheusmortatti/lightwhatsapp/MainActivity.kt`, replacing `MessageBodyText` (lines 1141-1155):

```kotlin
@Composable
private fun MessageBodyText(
    text: String,
    modifier: Modifier = Modifier,
    lighten: Boolean = false,
    align: TextAlign = TextAlign.Start,
    maxLines: Int = Int.MAX_VALUE,
    overflow: TextOverflow = TextOverflow.Clip,
) {
    val style = scaledStyle(LightThemeTokens.typography.copy, MESSAGE_FONT_SCALE, MESSAGE_LINE_HEIGHT_MULTIPLIER)
        .copy(textAlign = align)
    Text(
        text = text,
        modifier = modifier,
        color = if (lighten) LightThemeTokens.colors.contentSecondary else LightThemeTokens.colors.content,
        style = style,
        maxLines = maxLines,
        overflow = overflow,
    )
}
```

- [ ] **Step 6: Render the quote block in `MessageRow`**

Edit `app/src/main/kotlin/com/matheusmortatti/lightwhatsapp/MainActivity.kt`'s `MessageRow`, inserting a new block immediately after the existing `if (showHeader) { ... }` block (ends around line 966) and before the `when (message.type) {` block (starts around line 968):

```kotlin
        // Quote preview for a reply — inert (no tap-to-jump), inside the
        // row's existing onReact click target. Rendered below the row's own
        // header (if shown) and above its body, matching where a reply's
        // quote reads naturally: who/when this message is, what it's
        // replying to, then its own content.
        if (message.quotedType != null) {
            val quotedSenderLabel = if (message.quotedFromMe) {
                "You"
            } else {
                message.quotedSenderName ?: chatName
            }
            ChatMetaText(text = quotedSenderLabel)
            MessageBodyText(
                text = messagePreviewText(message.quotedType, message.quotedText),
                lighten = true,
                align = bodyAlign,
                maxLines = 1,
                overflow = TextOverflow.Ellipsis,
                modifier = Modifier.fillMaxWidth(),
            )
        }

```

- [ ] **Step 7: Build the app and verify it compiles**

Run: `./gradlew :app:assembleDebug`
Expected: BUILD SUCCESSFUL.

- [ ] **Step 8: Run the Kotlin unit tests again**

Run: `./gradlew :app:testDebugUnitTest`
Expected: PASS.

- [ ] **Step 9: Manual end-to-end verification on the real LP3 (chat-with-self only — see the `lp3-tools:deploy-debug` skill for build/install/log steps)**

From another device (e.g. WhatsApp Web, or WhatsApp on a phone signed into the same account, in the self-chat), send:
1. A reply to a text message.
2. A reply to an image (with and without your own caption on the reply).
3. A reply to a voice message.
4. A reply to a sticker.
5. In a group chat, a reply where the quoted sender differs from the group message's own sender.

For each, confirm on the LP3: the quote block shows the right sender label ("You" vs. the actual name/chat name) and a correct one-line preview (truncated with an ellipsis if long), positioned above the reply's own body, and tapping it does nothing beyond the existing reaction-picker behavior.

- [ ] **Step 10: Commit**

```bash
git add app/src/main/kotlin/com/matheusmortatti/lightwhatsapp/CoreProcess.kt \
        app/src/main/kotlin/com/matheusmortatti/lightwhatsapp/MainActivity.kt \
        app/src/test/kotlin/com/matheusmortatti/lightwhatsapp/MergeMessagesTest.kt
git commit -m "app: render a quoted-reply preview above reply messages"
```
