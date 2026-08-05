# Send Reactions Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let the user react to any message with WhatsApp's standard 6 quick-reactions (👍 ❤️ 😂 😮 😢 🙏), tapping the message to open the picker — turning today's receive-only reactions into full send+receive.

**Architecture:** Go core gains a `send_reaction` stdin command that calls `whatsmeow`'s `BuildReaction`/`SendMessage`, then folds the result into its own local message cache and emits `message_update` (mirroring how `handleSendMessage` already handles text sends — WhatsApp doesn't echo your own outgoing message/reaction back as an event). The Android app adds one new plumbing method (`CoreProcess` → `QrLoginViewModel`) and one new full-screen Compose picker, triggered by tapping a message row.

**Tech Stack:** Go (`core/`, `whatsmeow` v0.0.0-20260722203353-e9a033b24933), Kotlin/Jetpack Compose (`app/`), LightOS SDK (`sdk/ui`).

## Global Constraints

- Reaction set is exactly `👍 ❤️ 😂 😮 😢 🙏` — no larger/open picker (per spec, `docs/superpowers/specs/2026-08-05-send-reactions-design.md`).
- No long-press anywhere in this app — tapping the message row is the only trigger.
- On-device testing is self-chat only — never send test reactions to a real contact (see `feedback_device_testing_self_chat_only` memory).
- Go: only pure logic gets a unit test (no network-client mocking in this codebase — `handleSendMessage`/`handleSendAudio` aren't unit-tested either, only `applyReaction`/`extractMessage`/etc. are). Kotlin: this app has no unit test infra — verification is manual, on the emulator or real LP3.

---

## Task 1: `reactionSenderJID` — pure JID-resolution helper (Go, TDD)

**Files:**
- Modify: `core/main.go` (new function, placed right before `handleSendAudio` at line 1241 — i.e. immediately after the `recordedAudioMimetype` const block ends at line 1232, or immediately after `handleSendMessage` ends at line 1225; place it directly above `const recordedAudioMimetype`)
- Test: `core/main_test.go` (new test, inserted after `TestApplyReaction` ends at line 188, before `func TestExtractMessage` at line 189)

**Interfaces:**
- Produces: `func reactionSenderJID(chat types.JID, target chatMessage) (types.JID, error)` — used by Task 2's `handleSendReaction`.

- [ ] **Step 1: Write the failing test**

Insert into `core/main_test.go`, right after `TestApplyReaction`'s closing `}` (line 188) and before `func TestExtractMessage`:

```go
func TestReactionSenderJID(t *testing.T) {
	t.Run("our own message resolves to EmptyJID", func(t *testing.T) {
		chat := types.NewJID("111", types.DefaultUserServer)
		target := chatMessage{ID: "m1", FromMe: true}
		got, err := reactionSenderJID(chat, target)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != types.EmptyJID {
			t.Errorf("got %v, want EmptyJID", got)
		}
	})

	t.Run("1:1 peer's message resolves to the chat JID itself", func(t *testing.T) {
		chat := types.NewJID("111", types.DefaultUserServer)
		target := chatMessage{ID: "m1", FromMe: false}
		got, err := reactionSenderJID(chat, target)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != chat {
			t.Errorf("got %v, want chat JID %v", got, chat)
		}
	})

	t.Run("group participant's message resolves to their JID", func(t *testing.T) {
		chat := types.NewJID("222", types.GroupServer)
		participant := types.NewJID("333", types.DefaultUserServer)
		target := chatMessage{ID: "m1", FromMe: false, Sender: participant.String()}
		got, err := reactionSenderJID(chat, target)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != participant {
			t.Errorf("got %v, want %v", got, participant)
		}
	})

	t.Run("group message with unparseable sender is an error", func(t *testing.T) {
		chat := types.NewJID("222", types.GroupServer)
		target := chatMessage{ID: "m1", FromMe: false, Sender: "not a jid"}
		_, err := reactionSenderJID(chat, target)
		if err == nil {
			t.Fatal("expected an error, got nil")
		}
	})
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd core && go test ./... -run TestReactionSenderJID -v`
Expected: FAIL — `undefined: reactionSenderJID` (compile error).

- [ ] **Step 3: Write the implementation**

Insert into `core/main.go`, directly above `const recordedAudioMimetype = "audio/mp4"` (line 1232):

```go
// reactionSenderJID resolves the JID BuildMessageKey needs as its "sender"
// argument to identify target within chat when building a reaction to it:
// types.EmptyJID for our own message (BuildMessageKey then marks the built
// key FromMe: true), chat itself for a 1:1 peer's message (the chat IS the
// peer's JID there), or target.Sender for a group participant's message.
func reactionSenderJID(chat types.JID, target chatMessage) (types.JID, error) {
	if target.FromMe {
		return types.EmptyJID, nil
	}
	if chat.Server != types.GroupServer {
		return chat, nil
	}
	sender, err := types.ParseJID(target.Sender)
	if err != nil {
		return types.EmptyJID, fmt.Errorf("bad sender jid %q: %w", target.Sender, err)
	}
	return sender, nil
}

```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd core && go test ./... -run TestReactionSenderJID -v`
Expected: PASS (all 4 subtests).

- [ ] **Step 5: Run the full Go test suite to check for regressions**

Run: `cd core && go test ./...`
Expected: PASS (no existing test broken).

- [ ] **Step 6: Commit**

```bash
git add core/main.go core/main_test.go
git commit -m "core: add reactionSenderJID helper"
```

---

## Task 2: `send_reaction` command + `handleSendReaction` (Go)

**Files:**
- Modify: `core/main.go`
  - `command` struct (lines 83-95): new fields + updated doc comment
  - `chatMessage.Reactions` field doc comment (lines 162-165)
  - New `handleSendReaction` function, placed directly after `handleSendAudio` ends (line 1322), before `func readCommands` (line 1327)
  - `readCommands`'s switch statement (around line 1344-1351, inside the block already containing `case "send_audio":`): new `case "send_reaction":`

**Interfaces:**
- Consumes: `reactionSenderJID(chat types.JID, target chatMessage) (types.JID, error)` from Task 1; `applyReaction(reactions []chatReaction, sender, senderName string, fromMe bool, emoji string) []chatReaction` (existing); `resolveMentionsInList(ctx, client, []chatMessage) []chatMessage` (existing, used by `handleSendMessage`).
- Produces: `readCommands` now accepts `{"type":"send_reaction","jid":"...","message_id":"...","emoji":"..."}` on stdin — consumed by Task 3's `CoreProcess.sendReaction`.

- [ ] **Step 1: Update the `command` struct and its doc comment**

In `core/main.go`, replace lines 83-95:

```go
// command is one line of the stdin protocol: the app asking core for
// something — "open_chat" to fetch (and start filling in images/audio for)
// one conversation's messages and mark it read, "close_chat" to tell core
// the app navigated away (see openChatJID), "send_message" to send a text
// message to a jid, "send_audio" to upload and send a recorded voice
// message (AudioPath, relative to the working dir, plus its DurationMs), or
// "send_reaction" to react to an existing message (MessageID) with Emoji
// ("" removes a previously-sent reaction).
type command struct {
	Type       string `json:"type"`
	JID        string `json:"jid,omitempty"`
	Text       string `json:"text,omitempty"`
	AudioPath  string `json:"audio_path,omitempty"`
	DurationMs int64  `json:"duration_ms,omitempty"`
	MessageID  string `json:"message_id,omitempty"`
	Emoji      string `json:"emoji,omitempty"`
}
```

- [ ] **Step 2: Update `chatMessage.Reactions`'s stale doc comment**

Replace lines 162-165:

```go
	// Reactions on this message (see handleReaction and handleSendReaction),
	// populated live whether the message is ours or someone else's.
	Reactions []chatReaction `json:"reactions,omitempty"`
```

- [ ] **Step 3: Write `handleSendReaction`**

Insert directly after `handleSendAudio`'s closing `}` (line 1322), before `func readCommands` (line 1327):

```go
// handleSendReaction sends emoji as a reaction to messageID within jidStr
// via WhatsApp (emoji == "" removes a previously-sent reaction — see
// whatsmeow.RemoveReactionText), then folds it into the local cache the
// same way handleReaction does for an incoming one, since WhatsApp doesn't
// echo our own outgoing reaction back as an event (mirrors handleSendMessage
// not waiting for its own text message to echo back either).
func handleSendReaction(ctx context.Context, client *whatsmeow.Client, logger waLog.Logger, jidStr, messageID, emoji string, messages map[string][]chatMessage) {
	jid, err := types.ParseJID(jidStr)
	if err != nil {
		logger.Warnf("send_reaction: bad jid %q: %v", jidStr, err)
		return
	}

	messagesMu.Lock()
	list := messages[jidStr]
	idx := -1
	for i, m := range list {
		if m.ID == messageID {
			idx = i
			break
		}
	}
	if idx == -1 {
		messagesMu.Unlock()
		logger.Warnf("send_reaction: message %s not found in %s", messageID, jidStr)
		return
	}
	target := list[idx]
	messagesMu.Unlock()

	sender, err := reactionSenderJID(jid, target)
	if err != nil {
		logger.Warnf("send_reaction: %v", err)
		return
	}

	_, err = client.SendMessage(ctx, jid, client.BuildReaction(jid, sender, messageID, emoji))
	if err != nil {
		logger.Warnf("send_reaction in %s failed: %v", jidStr, err)
		emit(event{Type: "error", Message: fmt.Sprintf("failed to send reaction: %v", err)})
		return
	}

	messagesMu.Lock()
	list = messages[jidStr]
	var updated chatMessage
	found := false
	for i, cur := range list {
		if cur.ID == messageID {
			cur.Reactions = applyReaction(cur.Reactions, client.Store.ID.String(), "", true, emoji)
			list[i] = cur
			updated = cur
			found = true
			break
		}
	}
	if found {
		messages[jidStr] = list
		saveMessages(jidStr, list)
	}
	messagesMu.Unlock()

	if found {
		emit(event{Type: "message_update", JID: jidStr, Messages: resolveMentionsInList(ctx, client, []chatMessage{updated})})
	}
}

```

- [ ] **Step 4: Wire it into `readCommands`**

In `core/main.go`, in the `switch cmd.Type` block, right after the existing:

```go
		case "send_audio":
			if cmd.JID != "" && cmd.AudioPath != "" {
				go handleSendAudio(ctx, client, logger, cmd.JID, cmd.AudioPath, cmd.DurationMs, chats, messages)
			}
```

add:

```go
		case "send_reaction":
			if cmd.JID != "" && cmd.MessageID != "" {
				go handleSendReaction(ctx, client, logger, cmd.JID, cmd.MessageID, cmd.Emoji, messages)
			}
```

- [ ] **Step 5: Build and run the full test suite**

Run: `cd core && go build ./... && go vet ./... && go test ./...`
Expected: builds clean, `go vet` clean, all tests PASS. (`handleSendReaction` itself has no unit test — it calls `client.SendMessage`, a live network call, same as `handleSendMessage`/`handleSendAudio`, which are likewise untested at this level in this codebase; its pure logic (`reactionSenderJID`) is already covered by Task 1.)

- [ ] **Step 6: Commit**

```bash
git add core/main.go
git commit -m "core: add send_reaction command"
```

---

## Task 3: `CoreProcess.sendReaction` + `QrLoginViewModel.sendReaction` (Kotlin plumbing)

**Files:**
- Modify: `app/src/main/kotlin/com/matheusmortatti/lightwhatsapp/CoreProcess.kt`
  - `Message.reactions` doc comment (lines 56-59)
  - New `sendReaction` method, inserted after `sendAudio` (ends line 183), before `private fun writeCommand` (line 185)
- Modify: `app/src/main/kotlin/com/matheusmortatti/lightwhatsapp/QrLoginViewModel.kt`
  - New `sendReaction` method, inserted after `sendAudio` (ends line 116), before the `mergeMessages` comment block (line 118)

**Interfaces:**
- Consumes: `core/main.go`'s `"send_reaction"` command from Task 2.
- Produces: `QrLoginViewModel.sendReaction(messageId: String, emoji: String)` — used by Task 4's `MainActivity.kt` wiring (`onSendReaction = viewModel::sendReaction`).

- [ ] **Step 1: Update `Message.reactions`'s stale doc comment**

In `CoreProcess.kt`, replace lines 56-59:

```kotlin
    // Reactions on this message — see core/main.go's chatMessage.Reactions.
    // Populated whether the message is ours or theirs.
    val reactions: List<Reaction>,
```

- [ ] **Step 2: Add `CoreProcess.sendReaction`**

In `CoreProcess.kt`, insert right after `sendAudio`'s closing `}` (line 183), before `private fun writeCommand`:

```kotlin

    /**
     * Sends a "send_reaction" command to core's stdin, asking it to react
     * to an existing message — see core/main.go's
     * readCommands/handleSendReaction. [emoji] == "" removes a previously-
     * sent reaction. A no-op if the subprocess isn't running.
     */
    fun sendReaction(jid: String, messageId: String, emoji: String) {
        writeCommand(
            JSONObject()
                .put("type", "send_reaction")
                .put("jid", jid)
                .put("message_id", messageId)
                .put("emoji", emoji),
        )
    }
```

- [ ] **Step 3: Add `QrLoginViewModel.sendReaction`**

In `QrLoginViewModel.kt`, insert right after `sendAudio`'s closing `}` (line 116), before the `// Applies a message_update's delta...` comment (line 118):

```kotlin

    /**
     * Reacts to messageId in the currently open chat with emoji ("" removes
     * a previously-sent reaction). A no-op if no chat is open.
     */
    fun sendReaction(messageId: String, emoji: String) {
        val jid = _selectedChat.value?.jid ?: return
        coreProcess.sendReaction(jid, messageId, emoji)
    }
```

- [ ] **Step 4: Build the app module**

Run: `./gradlew :app:compileDebugKotlin`
Expected: BUILD SUCCESSFUL, no compile errors.

- [ ] **Step 5: Commit**

```bash
git add app/src/main/kotlin/com/matheusmortatti/lightwhatsapp/CoreProcess.kt app/src/main/kotlin/com/matheusmortatti/lightwhatsapp/QrLoginViewModel.kt
git commit -m "app: add sendReaction plumbing"
```

---

## Task 4: Reaction picker UI (Kotlin/Compose)

**Files:**
- Modify: `app/src/main/kotlin/com/matheusmortatti/lightwhatsapp/MainActivity.kt`
  - `QrLoginScreen` (lines 124-130): pass `onSendReaction`
  - `ChatDetailScreen` signature (lines 318-324): new `onSendReaction` param
  - `ChatDetailScreen` state (after line 343): new `reactingTo` state
  - `ChatDetailScreen`'s `BackHandler` (lines 365-372): new branch
  - `ChatDetailScreen` body (after the `playingVideo?.let { ... }` block, i.e. after line 471, before line 473's `Column(`): new early-return block rendering `ReactionPickerScreen`
  - `MessageRow` call site (lines 535-541): pass `onReact`
  - `MessageRow` signature (lines 745-753) and its outer `Column` (line 757): new `onReact` param + `.lightClickable`
  - New `ReactionPickerScreen` composable and `messagePreviewText`/`QUICK_REACTIONS` helpers, inserted after `VideoPlayerScreen` ends (line 743), before `private fun MessageRow` (line 745)

**Interfaces:**
- Consumes: `QrLoginViewModel.sendReaction(messageId: String, emoji: String)` from Task 3; existing `Message`, `Reaction` data classes from `CoreProcess.kt`; existing `LightTopBar`, `LightBarButton`, `LightTopBarCenter`, `LightText`, `LightTextVariant`, `lightClickable` from `sdk/ui` (all already imported in this file).
- Produces: nothing consumed elsewhere — this is the top of the feature.

- [ ] **Step 1: Wire `onSendReaction` through `QrLoginScreen`**

In `MainActivity.kt`, replace:

```kotlin
        selectedChat != null -> ChatDetailScreen(
            chat = selectedChat!!,
            messages = messages,
            onBack = viewModel::closeChat,
            onSend = viewModel::sendMessage,
            onSendAudio = viewModel::sendAudio,
        )
```

with:

```kotlin
        selectedChat != null -> ChatDetailScreen(
            chat = selectedChat!!,
            messages = messages,
            onBack = viewModel::closeChat,
            onSend = viewModel::sendMessage,
            onSendAudio = viewModel::sendAudio,
            onSendReaction = viewModel::sendReaction,
        )
```

- [ ] **Step 2: Add `onSendReaction` to `ChatDetailScreen`'s signature**

Replace:

```kotlin
private fun ChatDetailScreen(
    chat: Chat,
    messages: List<Message>,
    onBack: () -> Unit,
    onSend: (String) -> Unit,
    onSendAudio: (String, Long) -> Unit,
) {
```

with:

```kotlin
private fun ChatDetailScreen(
    chat: Chat,
    messages: List<Message>,
    onBack: () -> Unit,
    onSend: (String) -> Unit,
    onSendAudio: (String, Long) -> Unit,
    onSendReaction: (String, String) -> Unit,
) {
```

- [ ] **Step 3: Add `reactingTo` state**

Replace:

```kotlin
    // Path (relative to context.filesDir) + loop flag of the video/gif
    // currently open in the full-screen player, or null if none. Not
    // rememberSaveable — re-opening from the thumbnail on process restart
    // is cheap and there's no live player state worth restoring.
    var playingVideo by remember(chat.jid) { mutableStateOf<Pair<String, Boolean>?>(null) }
```

with:

```kotlin
    // Path (relative to context.filesDir) + loop flag of the video/gif
    // currently open in the full-screen player, or null if none. Not
    // rememberSaveable — re-opening from the thumbnail on process restart
    // is cheap and there's no live player state worth restoring.
    var playingVideo by remember(chat.jid) { mutableStateOf<Pair<String, Boolean>?>(null) }

    // The message whose reaction picker is currently open, or null. Not
    // rememberSaveable — same reasoning as playingVideo, re-opening it is
    // one tap away and there's no meaningful state to restore.
    var reactingTo by remember(chat.jid) { mutableStateOf<Message?>(null) }
```

- [ ] **Step 4: Add the `BackHandler` branch**

Replace:

```kotlin
    BackHandler(onBack = {
        when {
            playingVideo != null -> playingVideo = null
            composing -> composing = false
            recording -> cancelRecording()
            else -> onBack()
        }
    })
```

with:

```kotlin
    BackHandler(onBack = {
        when {
            reactingTo != null -> reactingTo = null
            playingVideo != null -> playingVideo = null
            composing -> composing = false
            recording -> cancelRecording()
            else -> onBack()
        }
    })
```

- [ ] **Step 5: Add the early-return picker block**

Replace:

```kotlin
    playingVideo?.let { (path, loop) ->
        VideoPlayerScreen(
            chatName = chat.name,
            relativePath = path,
            loop = loop,
            onClose = { playingVideo = null },
        )
        return
    }

    Column(
```

with:

```kotlin
    playingVideo?.let { (path, loop) ->
        VideoPlayerScreen(
            chatName = chat.name,
            relativePath = path,
            loop = loop,
            onClose = { playingVideo = null },
        )
        return
    }

    reactingTo?.let { target ->
        ReactionPickerScreen(
            message = target,
            chatName = chat.name,
            onPick = { emoji ->
                onSendReaction(target.id, emoji)
                reactingTo = null
            },
            onClose = { reactingTo = null },
        )
        return
    }

    Column(
```

- [ ] **Step 6: Pass `onReact` at the `MessageRow` call site**

Replace:

```kotlin
                        MessageRow(
                            message = message,
                            isGroup = chat.isGroup,
                            chatName = chat.name,
                            showHeader = showHeader || showStatus,
                            showStatus = showStatus,
                            onPlayVideo = { path, loop -> playingVideo = path to loop },
```

with:

```kotlin
                        MessageRow(
                            message = message,
                            isGroup = chat.isGroup,
                            chatName = chat.name,
                            showHeader = showHeader || showStatus,
                            showStatus = showStatus,
                            onPlayVideo = { path, loop -> playingVideo = path to loop },
                            onReact = { reactingTo = it },
```

- [ ] **Step 7: Add `onReact` to `MessageRow` and make its content tappable**

Replace:

```kotlin
@Composable
private fun MessageRow(
    message: Message,
    isGroup: Boolean,
    chatName: String,
    showHeader: Boolean,
    showStatus: Boolean,
    onPlayVideo: (path: String, loop: Boolean) -> Unit,
    modifier: Modifier = Modifier,
) {
    val bodyAlign = if (message.fromMe) TextAlign.End else TextAlign.Start
    Column(
        modifier = modifier.fillMaxWidth(),
        horizontalAlignment = if (message.fromMe) Alignment.End else Alignment.Start,
    ) {
```

with:

```kotlin
@Composable
private fun MessageRow(
    message: Message,
    isGroup: Boolean,
    chatName: String,
    showHeader: Boolean,
    showStatus: Boolean,
    onPlayVideo: (path: String, loop: Boolean) -> Unit,
    onReact: (Message) -> Unit,
    modifier: Modifier = Modifier,
) {
    val bodyAlign = if (message.fromMe) TextAlign.End else TextAlign.Start
    Column(
        modifier = modifier
            .fillMaxWidth()
            .lightClickable { onReact(message) },
        horizontalAlignment = if (message.fromMe) Alignment.End else Alignment.Start,
    ) {
```

This nests safely under the video thumbnail's and audio play button's own inner `lightClickable`s (see `MessageRow`'s `"video", "gif"` branch and `AudioMessageRow`) — Compose consumes a tap at the innermost clickable, so a tap on those still plays/pauses as before, and a tap anywhere else on the row (text, timestamp, empty space) now opens the reaction picker.

- [ ] **Step 8: Add `ReactionPickerScreen` and its helpers**

Insert directly after `VideoPlayerScreen`'s closing `}` (line 743), before `private fun MessageRow` (line 745):

```kotlin
// WhatsApp's own standard quick-reaction set — see
// docs/superpowers/specs/2026-08-05-send-reactions-design.md for why this
// is the agreed breadth rather than a larger or fully open picker.
private val QUICK_REACTIONS = listOf("👍", "❤️", "😂", "😮", "😢", "🙏")

// A one-line summary of message shown atop the reaction picker, reusing the
// same placeholder labels MessageRow renders for a not-yet-downloaded or
// non-text message.
private fun messagePreviewText(message: Message): String = when (message.type) {
    "image" -> message.text.ifBlank { "[Photo]" }
    "sticker" -> "[Sticker]"
    "video" -> message.text.ifBlank { "[Video]" }
    "gif" -> message.text.ifBlank { "[GIF]" }
    "audio" -> "[Voice message]"
    "unsupported" -> "[Unsupported message]"
    else -> message.text
}

@Composable
private fun ReactionPickerScreen(
    message: Message,
    chatName: String,
    onPick: (emoji: String) -> Unit,
    onClose: () -> Unit,
) {
    val currentReaction = message.reactions.firstOrNull { it.fromMe }?.emoji
    Column(
        modifier = Modifier
            .fillMaxSize()
            .background(LightThemeTokens.colors.background),
    ) {
        LightTopBar(
            leftButton = LightBarButton.LightIcon(icon = LightIcons.CLOSE, onClick = onClose),
            center = LightTopBarCenter.Text(chatName),
        )
        LightText(
            text = messagePreviewText(message),
            variant = LightTextVariant.Copy,
            lighten = true,
            maxLines = 2,
            overflow = TextOverflow.Ellipsis,
            modifier = Modifier
                .fillMaxWidth()
                .padding(horizontal = 24.dp, vertical = 16.dp),
        )
        Row(
            modifier = Modifier
                .weight(1f)
                .fillMaxWidth()
                .padding(horizontal = 16.dp),
            horizontalArrangement = Arrangement.SpaceEvenly,
            verticalAlignment = Alignment.CenterVertically,
        ) {
            for (emoji in QUICK_REACTIONS) {
                LightText(
                    text = emoji,
                    variant = LightTextVariant.Title,
                    underline = emoji == currentReaction,
                    modifier = Modifier.lightClickable {
                        onPick(if (emoji == currentReaction) "" else emoji)
                    },
                )
            }
        }
    }
}

```

- [ ] **Step 9: Build the app module**

Run: `./gradlew :app:compileDebugKotlin`
Expected: BUILD SUCCESSFUL, no compile errors, no unused-parameter warnings on the new `onReact`/`onSendReaction` params.

- [ ] **Step 10: Commit**

```bash
git add app/src/main/kotlin/com/matheusmortatti/lightwhatsapp/MainActivity.kt
git commit -m "app: add reaction picker UI"
```

---

## Task 5: Manual end-to-end verification

**Files:** none (verification only).

- [ ] **Step 1: Install and launch on the emulator or real LP3**

Use the lp3-tools:deploy-debug skill (or the project's established build/install/launch flow — see `project_dev_env_ready` memory) to build `:app`, install, and launch it.

- [ ] **Step 2: React to a message with each of the 6 emoji, in your self-chat**

Per standing practice, use chat-with-self only — never send test reactions to a real contact. Open self-chat, tap a message, confirm the picker shows the 6 emoji plus the message preview and chat name. Tap 👍; confirm the picker closes and 👍 appears under the message. Repeat for the other 5, confirming each replaces the previous one (never stacking).

- [ ] **Step 3: Confirm removal**

Tap the message again, confirm the currently-active emoji is shown underlined, tap it, confirm the reaction disappears from under the message.

- [ ] **Step 4: Confirm video/audio taps still work**

Send yourself (or use an existing) video and voice message in self-chat. Confirm tapping the video thumbnail still opens the full-screen player (not the reaction picker), and tapping the audio row's play button still starts playback (not the reaction picker). Then confirm tapping elsewhere on those same message rows (e.g. below the thumbnail, or the audio row's empty space) does open the reaction picker.

- [ ] **Step 5: Confirm back button closes the picker**

Open the picker, press the device back button/gesture, confirm it closes the picker without changing the reaction, and that a second back press then behaves as it did before this feature (closes composer/recording, or leaves the chat).

- [ ] **Step 6: Update memory**

If all checks pass, record in project memory that send-reactions shipped and verified (following this project's existing memory conventions — see e.g. `project_message_reactions_feature_2026-08-05`).
