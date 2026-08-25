# Poll Voting Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let the user select/deselect options on a poll they've received, sending their vote to WhatsApp, with the vote reflected as inline checkboxes directly in the poll bubble (no separate picker screen).

**Architecture:** Mirrors the shipped reaction-send flow end to end: a new `send_poll_vote` JSON command from the app to core's stdin, a `handleSendPollVote` handler in `core/main.go` that resolves the poll's original sender (reusing `reactionSenderJID`), calls whatsmeow's `BuildPollVote` + `SendMessage`, then folds the vote into the local cache (`applyPollVote`, already shipped for incoming votes) and emits `message_update` — the same "send, then locally fold in since WhatsApp doesn't echo our own outgoing message" pattern every other outbound command already uses. On the app side, each poll option row in the already-shipped `PollTallyRows` composable becomes its own tap target (nested inside the message row's `Column`, the same way the video thumbnail's play button already nests inside the row's own click target) that toggles the option and immediately sends the new complete selection — no confirm step.

**Tech Stack:** Go (core, vendored whatsmeow `go.mau.fi/whatsmeow`), Kotlin/Jetpack Compose (app, `light-sdk`'s `lightClickable`).

**Spec:** `docs/superpowers/specs/2026-08-24-poll-voting-design.md`

## Global Constraints

- A poll vote is always sent as a **full replacement** of the selection, never a delta — `whatsmeow.BuildPollVote(optionNames []string)` takes the complete set (spec's Context section).
- No optimistic local UI state: the checkbox reflects `message.pollVotes`, updated only once core's `message_update` event round-trips back (spec's Non-goals).
- Reacting (emoji) to a poll message becomes unreachable — tapping a poll bubble now drives voting instead of opening the reaction picker (spec's Non-goals).
- Multi-select cap: tapping a new (unselected) option while already at `pollSelectableCount` selections is a no-op — the user must deselect one first (spec's Design section, app/ subsection).

---

### Task 1: Core — pure option-name lookup helper

**Files:**
- Modify: `core/main.go`
- Test: `core/main_test.go`

**Interfaces:**
- Produces: `pollVoteOptionNames(pollOptions []string, selectedIndices []int) []string` — maps indices into `pollOptions` to their name strings, silently skipping any out-of-range index. Used by Task 2's `handleSendPollVote`.

- [ ] **Step 1: Write the failing test**

Add to `core/main_test.go`, near `TestMatchPollVoteOptions` (its inverse — that one maps hashes back to indices, this one maps indices to names):

```go
func TestPollVoteOptionNames(t *testing.T) {
	options := []string{"Pepperoni", "Mushroom", "Pineapple"}

	t.Run("maps a single index to its option name", func(t *testing.T) {
		got := pollVoteOptionNames(options, []int{1})
		if len(got) != 1 || got[0] != "Mushroom" {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("maps multiple indices in order", func(t *testing.T) {
		got := pollVoteOptionNames(options, []int{2, 0})
		if len(got) != 2 || got[0] != "Pineapple" || got[1] != "Pepperoni" {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("an out-of-range index is skipped, not appended as empty string", func(t *testing.T) {
		got := pollVoteOptionNames(options, []int{5, -1, 1})
		if len(got) != 1 || got[0] != "Mushroom" {
			t.Fatalf("got %+v, want just Mushroom", got)
		}
	})

	t.Run("no selected indices returns an empty (not nil-vs-empty-sensitive) slice", func(t *testing.T) {
		got := pollVoteOptionNames(options, nil)
		if len(got) != 0 {
			t.Fatalf("got %+v, want empty", got)
		}
	})
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd "/Users/matheusmortatti/git/Light Phone Apps/WhatsApp/core" && go test ./... -run TestPollVoteOptionNames -v`
Expected: FAIL — `undefined: pollVoteOptionNames`.

- [ ] **Step 3: Write the implementation**

Add to `core/main.go`, directly above `matchPollVoteOptions` (line 2205) — its inverse, same doc-comment style:

```go
// pollVoteOptionNames maps selectedIndices (indices into pollOptions,
// e.g. from a "send_poll_vote" command) to their option-name strings, the
// input shape whatsmeow.BuildPollVote/HashPollOptions actually needs — the
// same hash is computed both here (indirectly, via BuildPollVote) and by
// matchPollVoteOptions when decoding an incoming vote, so encoding by name
// rather than index is what keeps the two sides consistent. An
// out-of-range index (the client-side toggle logic shouldn't send one, but
// core doesn't trust it) is silently skipped rather than panicking or
// appending a bogus empty-string option name.
func pollVoteOptionNames(pollOptions []string, selectedIndices []int) []string {
	names := make([]string, 0, len(selectedIndices))
	for _, idx := range selectedIndices {
		if idx < 0 || idx >= len(pollOptions) {
			continue
		}
		names = append(names, pollOptions[idx])
	}
	return names
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd "/Users/matheusmortatti/git/Light Phone Apps/WhatsApp/core" && go test ./... -run TestPollVoteOptionNames -v`
Expected: PASS, all 4 subtests.

- [ ] **Step 5: Commit**

```bash
cd "/Users/matheusmortatti/git/Light Phone Apps/WhatsApp"
git add core/main.go core/main_test.go
git commit -m "feat(core): add pollVoteOptionNames helper for sending poll votes"
```

---

### Task 2: Core — `send_poll_vote` command and handler

**Files:**
- Modify: `core/main.go`

**Interfaces:**
- Consumes: `pollVoteOptionNames` (Task 1), `reactionSenderJID(chat types.JID, target chatMessage) (types.JID, error)` (`core/main.go:1534`, unmodified), `applyPollVote(votes []chatPollVote, sender, senderName string, fromMe bool, selectedOptions []int) []chatPollVote` (`core/main.go:2182`, unmodified), `canonicalizeChatJID(ctx, client, jid) types.JID` (`core/main.go:1773`, unmodified), whatsmeow's `client.BuildPollVote(ctx, pollInfo *types.MessageInfo, optionNames []string) (*waE2E.Message, error)`.
- Produces: `command.SelectedOptions []int` field; `handleSendPollVote(ctx context.Context, client *whatsmeow.Client, logger waLog.Logger, jidStr, messageID string, selectedOptions []int, messages map[string][]chatMessage)`; the `"send_poll_vote"` case in `readCommands`'s switch.

This task has no new automated test of its own — `handleSendPollVote` needs a live `*whatsmeow.Client`, the same reason `handleSendReaction` (its direct model) has none either; its correctness is covered by Task 1's unit test for the one piece of new pure logic, plus manual end-to-end verification in Task 6.

- [ ] **Step 1: Add `SelectedOptions` to the `command` struct**

In `core/main.go`, the `command` struct (94-102):

```go
type command struct {
	Type            string `json:"type"`
	JID             string `json:"jid,omitempty"`
	Text            string `json:"text,omitempty"`
	AudioPath       string `json:"audio_path,omitempty"`
	DurationMs      int64  `json:"duration_ms,omitempty"`
	MessageID       string `json:"message_id,omitempty"`
	Emoji           string `json:"emoji,omitempty"`
	SelectedOptions []int  `json:"selected_options,omitempty"`
}
```

- [ ] **Step 2: Add `handleSendPollVote`**

Add directly after `handleSendReaction` ends (line 1714, right before the `readCommands` doc comment):

```go
// handleSendPollVote sends selectedOptions (indices into the cached poll's
// PollOptions) as jidStr's vote on the poll message messageID, then folds
// it into the local cache the same way handleSendReaction does for a
// reaction, since WhatsApp doesn't echo our own outgoing vote back as an
// event either. An empty selectedOptions retracts any existing vote (see
// applyPollVote/chatPollVote's doc comments) — WhatsApp represents "no
// selection" the same way whether it's an initial empty vote or a retract.
func handleSendPollVote(ctx context.Context, client *whatsmeow.Client, logger waLog.Logger, jidStr, messageID string, selectedOptions []int, messages map[string][]chatMessage) {
	jid, err := types.ParseJID(jidStr)
	if err != nil {
		logger.Warnf("send_poll_vote: bad jid %q: %v", jidStr, err)
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
		logger.Warnf("send_poll_vote: message %s not found in %s", messageID, jidStr)
		return
	}
	target := list[idx]
	messagesMu.Unlock()

	if target.Type != "poll" {
		logger.Warnf("send_poll_vote: message %s in %s is not a poll", messageID, jidStr)
		return
	}

	sender, err := reactionSenderJID(jid, target)
	if err != nil {
		logger.Warnf("send_poll_vote: %v", err)
		return
	}

	pollInfo := &types.MessageInfo{
		MessageSource: types.MessageSource{
			Chat:     jid,
			Sender:   sender,
			IsFromMe: target.FromMe,
			IsGroup:  jid.Server == types.GroupServer,
		},
		ID: messageID,
	}

	optionNames := pollVoteOptionNames(target.PollOptions, selectedOptions)
	voteMsg, err := client.BuildPollVote(ctx, pollInfo, optionNames)
	if err != nil {
		logger.Warnf("send_poll_vote: failed to build vote for %s: %v", messageID, err)
		emit(event{Type: "error", Message: fmt.Sprintf("failed to build poll vote: %v", err)})
		return
	}

	_, err = client.SendMessage(ctx, jid, voteMsg)
	if err != nil {
		logger.Warnf("send_poll_vote in %s failed: %v", jidStr, err)
		emit(event{Type: "error", Message: fmt.Sprintf("failed to send poll vote: %v", err)})
		return
	}

	messagesMu.Lock()
	list = messages[jidStr]
	var updated chatMessage
	found := false
	for i, cur := range list {
		if cur.ID == messageID {
			ownKey := canonicalizeChatJID(ctx, client, client.Store.GetJID()).ToNonAD().String()
			cur.PollVotes = applyPollVote(cur.PollVotes, ownKey, "", true, selectedOptions)
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

- [ ] **Step 3: Add the `send_poll_vote` case to `readCommands`**

In `readCommands`'s switch (`core/main.go:1719-1755`), add after the `case "send_reaction":` block:

```go
		case "send_poll_vote":
			if cmd.JID != "" && cmd.MessageID != "" {
				go handleSendPollVote(ctx, client, logger, cmd.JID, cmd.MessageID, cmd.SelectedOptions, messages)
			}
```

- [ ] **Step 4: Build and run the full test suite to verify nothing broke**

Run: `cd "/Users/matheusmortatti/git/Light Phone Apps/WhatsApp/core" && go build ./... && go test ./...`
Expected: build succeeds, `ok` for the package (all existing tests plus Task 1's still pass).

- [ ] **Step 5: Commit**

```bash
cd "/Users/matheusmortatti/git/Light Phone Apps/WhatsApp"
git add core/main.go
git commit -m "feat(core): send poll votes via send_poll_vote command"
```

---

### Task 3: Kotlin — `sendPollVote` command plumbing

**Files:**
- Modify: `app/src/main/kotlin/com/matheusmortatti/lightwhatsapp/CoreProcess.kt`
- Modify: `app/src/main/kotlin/com/matheusmortatti/lightwhatsapp/QrLoginViewModel.kt`

**Interfaces:**
- Produces: `CoreProcess.sendPollVote(jid: String, messageId: String, selectedOptions: List<Int>)`; `QrLoginViewModel.sendPollVote(messageId: String, selectedOptions: List<Int>)`.

No new automated test — mirrors `sendReaction`/`CoreProcess.sendReaction`, which have none either (thin JSON-serialization wrappers, covered by Task 6's manual end-to-end pass).

- [ ] **Step 1: Add `CoreProcess.sendPollVote`**

In `CoreProcess.kt`, directly after `sendReaction` (ends line 238):

```kotlin
    /**
     * Sends a "send_poll_vote" command to core's stdin, asking it to cast
     * (or change) this device's vote on a poll message — see
     * core/main.go's readCommands/handleSendPollVote. [selectedOptions] is
     * the complete replacement selection (indices into the poll's
     * options), not a delta — an empty list retracts the vote. A no-op if
     * the subprocess isn't running.
     */
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

- [ ] **Step 2: Add `QrLoginViewModel.sendPollVote`**

In `QrLoginViewModel.kt`, directly after `sendReaction` (ends line 135):

```kotlin
    fun sendPollVote(messageId: String, selectedOptions: List<Int>) {
        val jid = _selectedChat.value?.jid ?: return
        coreProcess.sendPollVote(jid, messageId, selectedOptions)
    }
```

- [ ] **Step 3: Build to verify it compiles**

Run: `cd "/Users/matheusmortatti/git/Light Phone Apps/WhatsApp" && ./gradlew :app:compileDebugKotlin`
Expected: `BUILD SUCCESSFUL`.

- [ ] **Step 4: Commit**

```bash
cd "/Users/matheusmortatti/git/Light Phone Apps/WhatsApp"
git add app/src/main/kotlin/com/matheusmortatti/lightwhatsapp/CoreProcess.kt app/src/main/kotlin/com/matheusmortatti/lightwhatsapp/QrLoginViewModel.kt
git commit -m "feat(app): add sendPollVote command plumbing"
```

---

### Task 4: Kotlin — pure toggle/cap selection logic

**Files:**
- Modify: `app/src/main/kotlin/com/matheusmortatti/lightwhatsapp/MainActivity.kt`
- Test: `app/src/test/kotlin/com/matheusmortatti/lightwhatsapp/PollSelectionTest.kt` (new)

**Interfaces:**
- Produces: `internal fun nextPollSelection(current: List<Int>, tapped: Int, cap: Int): List<Int>` — computes the new complete selection after tapping option `tapped`, given the poll's `pollSelectableCount` as `cap`. Consumed by Task 5.

- [ ] **Step 1: Write the failing test**

Create `app/src/test/kotlin/com/matheusmortatti/lightwhatsapp/PollSelectionTest.kt`:

```kotlin
package com.matheusmortatti.lightwhatsapp

import kotlin.test.Test
import kotlin.test.assertEquals

class PollSelectionTest {

    @Test
    fun `tapping an unselected option adds it`() {
        assertEquals(listOf(0), nextPollSelection(current = emptyList(), tapped = 0, cap = 2))
    }

    @Test
    fun `tapping an already-selected option removes it`() {
        assertEquals(listOf(1), nextPollSelection(current = listOf(0, 1), tapped = 0, cap = 2))
    }

    @Test
    fun `tapping a new option at cap is a no-op`() {
        assertEquals(listOf(0, 1), nextPollSelection(current = listOf(0, 1), tapped = 2, cap = 2))
    }

    @Test
    fun `single-select poll ignores a second option until the first is deselected`() {
        assertEquals(listOf(0), nextPollSelection(current = listOf(0), tapped = 1, cap = 1))
    }

    @Test
    fun `single-select poll allows deselecting the current option`() {
        assertEquals(emptyList(), nextPollSelection(current = listOf(0), tapped = 0, cap = 1))
    }

    @Test
    fun `multi-select poll allows adding up to the cap`() {
        assertEquals(listOf(0, 1, 2), nextPollSelection(current = listOf(0, 1), tapped = 2, cap = 3))
    }
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd "/Users/matheusmortatti/git/Light Phone Apps/WhatsApp" && ./gradlew :app:testDebugUnitTest --tests "com.matheusmortatti.lightwhatsapp.PollSelectionTest"`
Expected: FAIL to compile — `nextPollSelection` is unresolved.

- [ ] **Step 3: Implement `nextPollSelection`**

In `MainActivity.kt`, directly above `PollTallyRows` (before line 1130's `POLL_BAR_SEGMENTS` constant):

```kotlin
// Computes the complete new selection after tapping option tapped on a
// poll whose selectable-option count is cap: deselects if already
// selected, otherwise adds it unless the selection is already at cap (the
// user must deselect one first — this also naturally covers single-select,
// cap == 1: tapping a different option while one is already selected is a
// no-op). A poll vote is always sent as this complete replacement set, not
// a delta, so this returns the full next selection, not just tapped.
internal fun nextPollSelection(current: List<Int>, tapped: Int, cap: Int): List<Int> {
    if (tapped in current) return current - tapped
    if (current.size >= cap) return current
    return current + tapped
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd "/Users/matheusmortatti/git/Light Phone Apps/WhatsApp" && ./gradlew :app:testDebugUnitTest --tests "com.matheusmortatti.lightwhatsapp.PollSelectionTest"`
Expected: `BUILD SUCCESSFUL`, all 6 tests pass.

- [ ] **Step 5: Commit**

```bash
cd "/Users/matheusmortatti/git/Light Phone Apps/WhatsApp"
git add app/src/main/kotlin/com/matheusmortatti/lightwhatsapp/MainActivity.kt app/src/test/kotlin/com/matheusmortatti/lightwhatsapp/PollSelectionTest.kt
git commit -m "feat(app): add nextPollSelection toggle/cap logic"
```

---

### Task 5: Kotlin — inline interactive poll rows

**Files:**
- Modify: `app/src/main/kotlin/com/matheusmortatti/lightwhatsapp/MainActivity.kt`

**Interfaces:**
- Consumes: `nextPollSelection` (Task 4), `QrLoginViewModel.sendPollVote` (Task 3), `Message.pollVotes: List<PollVote>` / `PollVote.fromMe: Boolean` / `PollVote.selectedOptions: List<Int>` / `Message.pollSelectableCount: Int` (all already shipped, unmodified).
- Produces: `PollTallyRows` gains `ownSelection: List<Int>` and `onToggle: (Int) -> Unit` parameters; `MessageRow` gains `onTogglePollOption: (Message, Int) -> Unit`; `ChatDetailScreen` gains `onSendPollVote: (String, List<Int>) -> Unit`; `QrLoginScreen` wires `onSendPollVote = viewModel::sendPollVote`.

No new automated test — this is Compose UI wiring around already-tested pure logic (Task 4) and already-tested parsing (shipped); covered by Task 6's manual pass, matching how the shipped `ReactionPickerScreen` UI has no automated test either.

- [ ] **Step 1: Make `PollTallyRows` interactive**

Replace `PollTallyRows` (`MainActivity.kt:1159-1191`) with:

```kotlin
@Composable
private fun PollTallyRows(
    options: List<String>,
    votes: List<PollVote>,
    ownSelection: List<Int>,
    onToggle: (Int) -> Unit,
    bodyAlign: TextAlign,
) {
    val counts = IntArray(options.size)
    for (vote in votes) {
        for (index in vote.selectedOptions) {
            if (index in counts.indices) counts[index]++
        }
    }
    val maxCount = counts.max()
    Column(modifier = Modifier.fillMaxWidth()) {
        options.forEachIndexed { i, option ->
            val count = counts[i]
            val filled = if (maxCount == 0) 0 else (count * POLL_BAR_SEGMENTS + maxCount - 1) / maxCount
            val bar = "■".repeat(filled) + "□".repeat(POLL_BAR_SEGMENTS - filled)
            val marker = if (i in ownSelection) "☑" else "☐"
            Row(
                modifier = Modifier
                    .fillMaxWidth()
                    .lightClickable { onToggle(i) },
                verticalAlignment = Alignment.CenterVertically,
            ) {
                MessageBodyText(
                    text = "$marker $option",
                    lighten = true,
                    align = bodyAlign,
                    modifier = Modifier.weight(1f),
                )
                MessageBodyText(
                    text = "$bar $count",
                    lighten = true,
                    align = TextAlign.End,
                    monospace = true,
                    maxLines = 1,
                    modifier = Modifier.width(POLL_TALLY_COLUMN_WIDTH),
                )
            }
        }
    }
}
```

(Only change from the shipped version: two new parameters, the `☑`/`☐` marker replacing the static `"▸ "` prefix, and each `Row` gaining its own `.lightClickable`.)

- [ ] **Step 2: Wire `onTogglePollOption` through `MessageRow`**

In `MessageRow`'s signature (`MainActivity.kt:935-944`), add the new parameter after `onReact`:

```kotlin
private fun MessageRow(
    message: Message,
    isGroup: Boolean,
    chatName: String,
    showHeader: Boolean,
    showStatus: Boolean,
    onPlayVideo: (path: String, loop: Boolean) -> Unit,
    onReact: (Message) -> Unit,
    onTogglePollOption: (Message, Int) -> Unit,
    modifier: Modifier = Modifier,
) {
```

Replace the outer `Column`'s modifier (lines 946-952) so a poll message's row no longer opens the reaction picker on tap (voting is its tap-driven action instead — see Non-goals):

```kotlin
    val bodyAlign = if (message.fromMe) TextAlign.End else TextAlign.Start
    // Deliberately on the outer Column, so the tap target includes the
    // sender/time header rather than just the body below — a bigger tap
    // target, and the header has no competing action of its own. Skipped
    // for polls: voting (via PollTallyRows' own per-option tap targets) is
    // a poll bubble's tap-driven action instead of reacting.
    val outerModifier = modifier.fillMaxWidth().let {
        if (message.type == "poll") it else it.lightClickable { onReact(message) }
    }
    Column(
        modifier = outerModifier,
        horizontalAlignment = if (message.fromMe) Alignment.End else Alignment.Start,
    ) {
```

Update the `"poll" ->` branch (`MainActivity.kt:1098-1105`):

```kotlin
            "poll" -> {
                MessageBodyText(text = message.text, align = bodyAlign)
                if (message.pollOptions.isNotEmpty()) {
                    val ownSelection = message.pollVotes.firstOrNull { it.fromMe }?.selectedOptions ?: emptyList()
                    PollTallyRows(
                        options = message.pollOptions,
                        votes = message.pollVotes,
                        ownSelection = ownSelection,
                        onToggle = { index -> onTogglePollOption(message, index) },
                        bodyAlign = bodyAlign,
                    )
                    val voterCount = message.pollVotes.size
                    ChatMetaText(text = if (voterCount == 1) "1 vote" else "$voterCount votes")
                }
            }
```

- [ ] **Step 3: Wire `onSendPollVote` through `ChatDetailScreen`**

Add the parameter to `ChatDetailScreen`'s signature (`MainActivity.kt:320-327`), after `onSendReaction`:

```kotlin
private fun ChatDetailScreen(
    chat: Chat,
    messages: List<Message>,
    onBack: () -> Unit,
    onSend: (String) -> Unit,
    onSendAudio: (String, Long) -> Unit,
    onSendReaction: (String, String) -> Unit,
    onSendPollVote: (String, List<Int>) -> Unit,
) {
```

At the `MessageRow` call site (`MainActivity.kt:578-595`), add the new argument after `onReact`:

```kotlin
                        MessageRow(
                            message = message,
                            isGroup = chat.isGroup,
                            chatName = chat.name,
                            showHeader = showHeader || showStatus,
                            showStatus = showStatus,
                            onPlayVideo = { path, loop -> playingVideo = path to loop },
                            onReact = { reactingTo = it },
                            onTogglePollOption = { message, index ->
                                val current = message.pollVotes.firstOrNull { it.fromMe }?.selectedOptions ?: emptyList()
                                val next = nextPollSelection(current, index, message.pollSelectableCount)
                                if (next != current) onSendPollVote(message.id, next)
                            },
                            modifier = Modifier.padding(
                                start = 24.dp,
                                end = 24.dp,
                                top = if (showHeader || showStatus) 14.dp else 9.dp,
                            ),
                        )
```

- [ ] **Step 4: Wire `onSendPollVote` at the top-level call site**

In `QrLoginScreen` (`MainActivity.kt:125-132`), add after `onSendReaction`:

```kotlin
        selectedChat != null -> ChatDetailScreen(
            chat = selectedChat!!,
            messages = messages,
            onBack = viewModel::closeChat,
            onSend = viewModel::sendMessage,
            onSendAudio = viewModel::sendAudio,
            onSendReaction = viewModel::sendReaction,
            onSendPollVote = viewModel::sendPollVote,
        )
```

- [ ] **Step 5: Build to verify it compiles**

Run: `cd "/Users/matheusmortatti/git/Light Phone Apps/WhatsApp" && ./gradlew :app:compileDebugKotlin`
Expected: `BUILD SUCCESSFUL`.

- [ ] **Step 6: Commit**

```bash
cd "/Users/matheusmortatti/git/Light Phone Apps/WhatsApp"
git add app/src/main/kotlin/com/matheusmortatti/lightwhatsapp/MainActivity.kt
git commit -m "feat(app): vote on polls via inline checkboxes"
```

---

### Task 6: End-to-end verification

**Files:** none (manual verification only).

**Post-implementation note:** the final whole-branch review (commit `3d94295`, on top of Task 5's `2f4d4d5`) found and fixed 3 issues not caught by any task-scoped review: voting on a poll *you* created failed deterministically (`reactionSenderJID`'s `EmptyJID`-for-`FromMe` sentinel doesn't carry over to `BuildPollVote`, which needs a real JID as a secret-store lookup key — fixed by overriding `sender` to your own JID in that case); a failed vote-build used to dump the whole UI into an unrecoverable full-screen error (now log-only, matching `handlePollVote`'s treatment of the symmetric decrypt-failure case); and `pollSelectableCount == 0` (WhatsApp's own "unlimited" sentinel, wire-indistinguishable from the field being absent) silently disabled voting forever (now falls back to the poll's option count). Steps 9-10 below cover the first two; the `takeIf { it > 0 }` fallback is exercised naturally by any real "allow multiple answers" poll in Step 5.

- [ ] **Step 1: Build and install the debug APK**

Run: `cd "/Users/matheusmortatti/git/Light Phone Apps/WhatsApp" && core/build_android.sh && ./gradlew :app:installDebug` (or the emulator/device flow already documented in `PROJECT.md`/prior feature memories — use whichever this repo's current dev setup expects).

- [ ] **Step 2: Vote on a single-select poll**

Per `feedback_device_testing_self_chat_only`: from a second device/account (or WhatsApp Web on the same account), send a single-select poll (2-3 options) to self-chat. Open it in this app and tap one option.

Verify: that option's checkbox fills in (☑), the bar/count updates to reflect your vote, "1 vote" shows, and — on the sending device/WhatsApp Web — your vote is visible there too.

- [ ] **Step 3: Change a single-select vote**

Tap a different option on the same poll.

Verify: the previous option's checkbox clears, the new one fills in, and the tally reflects only the new selection (not both) — confirming the vote replaced rather than added.

- [ ] **Step 4: Deselect (retract) a vote**

Tap the currently-selected option again.

Verify: its checkbox clears, the tally drops back to 0 for that option, and "0 votes" shows.

- [ ] **Step 5: Vote on a multi-select poll**

Send a multi-select poll (`selectableOptionsCount` ≥ 2, e.g. 3 options, pick up to 2) to self-chat. Tap two different options in this app.

Verify: both checkboxes fill in, both tallies update, and "1 vote" shows (one voter, two selections — matches the shipped `voterCount` semantics counting distinct voters, not selections).

- [ ] **Step 6: Confirm the multi-select cap**

On the same poll (cap 2, two options already selected), tap the third, unselected option.

Verify: nothing happens — the third option's checkbox stays unchecked and no vote is sent (deselect one of the first two, then the third becomes selectable).

- [ ] **Step 7: Confirm reacting to a poll is no longer reachable**

Tap anywhere on the poll bubble outside an option row's own area (e.g. the question text or sender/time header).

Verify: no reaction picker opens (this is the expected, intentional trade-off — see the spec's Non-goals).

- [ ] **Step 8: Confirm non-poll messages are unaffected**

Tap a text message.

Verify: the reaction picker still opens as before.

- [ ] **Step 9: Vote on a poll you created yourself**

From this app (or note: creating a poll from this app is out of scope — send the poll from a second device/WhatsApp Web on the *same* account you're logged into on the LP3, so it's a `FromMe` poll from this app's perspective), vote on it from this app.

Verify: the vote succeeds (checkbox fills in, tally updates) — this is the exact path the final-review fix (`3d94295`) addressed; prior to that fix this would have failed every time.

- [ ] **Step 10: Confirm a routine vote-build failure doesn't break the UI**

Hard to force directly, but worth a light check: if any vote tap ever produces no visible effect and no crash (rather than dumping you to a full-screen error), that's the intended log-only behavior for a `BuildPollVote` failure (e.g. secret key never seen for a history-synced poll) — check `adb logcat` for a `send_poll_vote: failed to build vote for ...` warning if you want to confirm the no-op path fired rather than nothing happening at all.

- [ ] **Step 11: Note the real `poll_selectable_count` for a WhatsApp "Allow multiple answers" poll**

While testing Step 5, if you can inspect core's logs or the JSON `messages`/`message_update` event for that poll, note whether `poll_selectable_count` actually arrives as `0` (as the final review hypothesized `BuildPollCreation` would send for "unlimited") or as the real option count. Either way voting should work (Step 5's fallback handles both), but this settles an open question the spec previously got wrong (`pollSelectableCount` is *not* reliably ≥ 1 — see the corrected note in the spec's Design section) and is worth confirming for the record.
