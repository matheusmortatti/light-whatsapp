# Message Delivery/Read Status Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Show sent → delivered → read status on your own messages in 1:1 WhatsApp chats, so you know whether a message you sent actually got through.

**Architecture:** `core/main.go` (the Go/whatsmeow subprocess) already receives `*events.Receipt` for messages you sent but drops the "actual recipient acked it" case — extend it to track a new `Status` field on cached messages and re-emit. `app/`'s `CoreProcess.kt` parses that field, and `MainActivity.kt`'s `ChatDetailScreen` shows it as a text suffix (`You  14:32 · Read`) on the most recent message you sent, reusing the existing header-line text style.

**Tech Stack:** Go 1.26 (`core/`, whatsmeow), Kotlin/Jetpack Compose (`app/`).

**Spec:** `docs/superpowers/specs/2026-08-03-message-status-design.md`

## Global Constraints

- 1:1 chats only. Group messages keep today's behavior (no status shown/tracked) — full per-member group read-tracking is an explicit non-goal.
- No "sending…" transient state: a message only ever appears in the UI after `core/`'s `client.SendMessage` call has already succeeded (server-acked), so every message that appears starts at `"sent"`.
- Status never downgrades: once a message is `"read"`, a later out-of-order `"delivered"` receipt must not revert it.
- Status is shown as plain text next to the existing timestamp (`You  14:32 · Read`), not a new icon — matches the app's current icon set (`LightIcons`), which has no check/double-check glyph.
- Status is shown only on the single most recent own message in a chat (not on every bubble) — it's forced visible even if that message would otherwise be hidden by the existing message-clustering logic (`showHeader`).

---

### Task 1: `core/` — track and emit message delivery/read status

**Files:**
- Modify: `core/main.go`
  - `chatMessage` struct (currently lines 92-122)
  - `handleSendMessage` (currently lines 791-830)
  - the `client.AddEventHandler` dispatch switch (currently lines 1399-1418)
- Test: `core/main_test.go` (new file)

**Interfaces:**
- Consumes: existing `chatMessage`, `types.MessageSource`, `events.Receipt`, `canonicalizeChatJID(ctx, client, jid) types.JID`, `resolveMentionsInList(ctx, client, list) []chatMessage`, `messagesMu sync.Mutex`, `saveMessages(jid string, list []chatMessage)`, `emit(event)`.
- Produces: `chatMessage.Status string` (JSON tag `status`, values `"sent"` / `"delivered"` / `"read"` / `""`), two pure helpers `receiptStatusFor(evt *events.Receipt) string` and `applyMessageStatus(list []chatMessage, ids []types.MessageID, status string) bool`, and `handleSentMessageStatusReceipt(ctx context.Context, client *whatsmeow.Client, evt *events.Receipt, messages map[string][]chatMessage)` — Task 2 (the Android side) only depends on the new `status` JSON field, not on these Go symbols directly.

- [ ] **Step 1: Write the failing tests for the new pure logic**

Create `core/main_test.go`:

```go
package main

import (
	"testing"

	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

func TestReceiptStatusFor(t *testing.T) {
	tests := []struct {
		name     string
		isFromMe bool
		isGroup  bool
		rtype    types.ReceiptType
		want     string
	}{
		{"delivered receipt from recipient", false, false, types.ReceiptTypeDelivered, "delivered"},
		{"read receipt from recipient", false, false, types.ReceiptTypeRead, "read"},
		{"self read-sync ignored (handled by handleReadReceipt)", true, false, types.ReceiptTypeRead, ""},
		{"group chat ignored", false, true, types.ReceiptTypeDelivered, ""},
		{"unrelated receipt type ignored", false, false, types.ReceiptTypeRetry, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			evt := &events.Receipt{
				MessageSource: types.MessageSource{IsFromMe: tt.isFromMe, IsGroup: tt.isGroup},
				Type:          tt.rtype,
			}
			if got := receiptStatusFor(evt); got != tt.want {
				t.Errorf("receiptStatusFor() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestApplyMessageStatus(t *testing.T) {
	t.Run("sets status on matching from-me message", func(t *testing.T) {
		list := []chatMessage{
			{ID: "m1", FromMe: true, Status: "sent"},
			{ID: "m2", FromMe: false},
		}
		changed := applyMessageStatus(list, []types.MessageID{"m1"}, "delivered")
		if !changed {
			t.Fatal("expected changed = true")
		}
		if list[0].Status != "delivered" {
			t.Errorf("list[0].Status = %q, want %q", list[0].Status, "delivered")
		}
		if list[1].Status != "" {
			t.Errorf("list[1].Status should stay untouched, got %q", list[1].Status)
		}
	})

	t.Run("never downgrades read back to delivered", func(t *testing.T) {
		list := []chatMessage{{ID: "m1", FromMe: true, Status: "read"}}
		changed := applyMessageStatus(list, []types.MessageID{"m1"}, "delivered")
		if changed {
			t.Fatal("expected changed = false, read must not downgrade")
		}
		if list[0].Status != "read" {
			t.Errorf("list[0].Status = %q, want %q", list[0].Status, "read")
		}
	})

	t.Run("message ID not found is a no-op", func(t *testing.T) {
		list := []chatMessage{{ID: "m1", FromMe: true, Status: "sent"}}
		changed := applyMessageStatus(list, []types.MessageID{"missing"}, "delivered")
		if changed {
			t.Fatal("expected changed = false")
		}
		if list[0].Status != "sent" {
			t.Errorf("list[0].Status = %q, want unchanged %q", list[0].Status, "sent")
		}
	})

	t.Run("ignores non-from-me messages even if ID matches", func(t *testing.T) {
		list := []chatMessage{{ID: "m1", FromMe: false, Status: ""}}
		changed := applyMessageStatus(list, []types.MessageID{"m1"}, "delivered")
		if changed {
			t.Fatal("expected changed = false")
		}
		if list[0].Status != "" {
			t.Errorf("list[0].Status should stay empty, got %q", list[0].Status)
		}
	})
}
```

- [ ] **Step 2: Run the tests to verify they fail on missing symbols**

Run: `cd core && go test ./... -run 'TestReceiptStatusFor|TestApplyMessageStatus' -v`
Expected: FAIL — compile error, `undefined: receiptStatusFor` (and `applyMessageStatus`)

- [ ] **Step 3: Add the `Status` field to `chatMessage`**

In `core/main.go`, the `chatMessage` struct currently reads:

```go
type chatMessage struct {
	ID         string `json:"id"`
	Timestamp  int64  `json:"timestamp"`
	FromMe     bool   `json:"from_me"`
	Sender     string `json:"sender,omitempty"`      // sender JID, group chats only
	SenderName string `json:"sender_name,omitempty"` // best-effort display name, group chats only
	Type       string `json:"type"`                  // "text" | "image" | "audio"
	Text       string `json:"text,omitempty"`        // body, or an image's caption
	ImagePath  string `json:"image_path,omitempty"`  // path (relative to the working dir) once downloaded
```

Change it to add a `Status` field right after `FromMe`:

```go
type chatMessage struct {
	ID        string `json:"id"`
	Timestamp int64  `json:"timestamp"`
	FromMe    bool   `json:"from_me"`

	// Sent -> delivered -> read status for a message *we* sent in a 1:1
	// chat, driven by handleSentMessageStatusReceipt. Never set for
	// incoming messages or group chats (WhatsApp only reports "read" once
	// every group member has read a message, which needs per-recipient
	// aggregation this app doesn't do yet). "" means unknown/not
	// applicable, not "unsent".
	Status string `json:"status,omitempty"` // "sent" | "delivered" | "read"

	Sender     string `json:"sender,omitempty"`      // sender JID, group chats only
	SenderName string `json:"sender_name,omitempty"` // best-effort display name, group chats only
	Type       string `json:"type"`                  // "text" | "image" | "audio"
	Text       string `json:"text,omitempty"`        // body, or an image's caption
	ImagePath  string `json:"image_path,omitempty"`  // path (relative to the working dir) once downloaded
```

- [ ] **Step 4: Implement the pure helpers and the event handler**

Add this new code to `core/main.go`, directly after `handleReadReceipt` (which currently ends at line 743, right before `handleOpenChat`):

```go
// receiptStatusFor maps evt to the chatMessage.Status it implies for a
// message *we* sent, or "" if this receipt doesn't carry that (it's the
// self-read-sync case handleReadReceipt already handles, a group chat — see
// chatMessage.Status — or a receipt type this app doesn't track).
func receiptStatusFor(evt *events.Receipt) string {
	if evt.MessageSource.IsFromMe || evt.MessageSource.IsGroup {
		return ""
	}
	switch evt.Type {
	case types.ReceiptTypeDelivered:
		return "delivered"
	case types.ReceiptTypeRead:
		return "read"
	default:
		return ""
	}
}

// applyMessageStatus sets Status to status, in place, on every message in
// list whose ID is in ids and is FromMe — except it never downgrades an
// existing "read" back to "delivered" (a delivered receipt can in
// principle arrive after a read one, out of order). Returns whether
// anything changed.
func applyMessageStatus(list []chatMessage, ids []types.MessageID, status string) bool {
	idSet := make(map[types.MessageID]bool, len(ids))
	for _, id := range ids {
		idSet[id] = true
	}
	changed := false
	for i, m := range list {
		if !m.FromMe || !idSet[m.ID] || m.Status == "read" {
			continue
		}
		list[i].Status = status
		changed = true
	}
	return changed
}

// handleSentMessageStatusReceipt reacts to a *events.Receipt telling us the
// actual recipient (not our own read-sync — see handleReadReceipt) received
// or read a message we sent, updating that message's Status and re-emitting
// the chat's messages so the UI updates live.
func handleSentMessageStatusReceipt(ctx context.Context, client *whatsmeow.Client, evt *events.Receipt, messages map[string][]chatMessage) {
	status := receiptStatusFor(evt)
	if status == "" {
		return
	}
	jidStr := canonicalizeChatJID(ctx, client, evt.MessageSource.Chat).String()

	messagesMu.Lock()
	list, ok := messages[jidStr]
	if !ok {
		messagesMu.Unlock()
		return
	}
	changed := applyMessageStatus(list, evt.MessageIDs, status)
	if changed {
		saveMessages(jidStr, list)
	}
	messagesMu.Unlock()

	if changed {
		emit(event{Type: "messages", JID: jidStr, Messages: resolveMentionsInList(ctx, client, list)})
	}
}
```

- [ ] **Step 5: Wire the new handler into the event dispatch**

In `core/main.go`, the dispatch switch currently has:

```go
		case *events.Receipt:
			handleReadReceipt(ctx, client, e, chats)
```

Change to:

```go
		case *events.Receipt:
			handleReadReceipt(ctx, client, e, chats)
			handleSentMessageStatusReceipt(ctx, client, e, messages)
```

- [ ] **Step 6: Set the initial `"sent"` status when sending a 1:1 message**

In `handleSendMessage`, the message construction currently reads:

```go
	cm := chatMessage{
		ID:        resp.ID,
		Timestamp: timestamp,
		FromMe:    true,
		Type:      "text",
		Text:      text,
	}
```

`c` (the chat's `chatSummary`, already looked up just above this point in the function) tells us whether this is a group chat. Change to:

```go
	status := "sent"
	if c.IsGroup {
		status = ""
	}
	cm := chatMessage{
		ID:        resp.ID,
		Timestamp: timestamp,
		FromMe:    true,
		Status:    status,
		Type:      "text",
		Text:      text,
	}
```

- [ ] **Step 7: Run the tests to verify they pass**

Run: `cd core && go test ./... -run 'TestReceiptStatusFor|TestApplyMessageStatus' -v`
Expected: PASS (9 subtests total)

- [ ] **Step 8: Build the whole module to catch anything the targeted test run missed**

Run: `cd core && go build ./... && go vet ./...`
Expected: no output, exit code 0

- [ ] **Step 9: Commit**

```bash
git add core/main.go core/main_test.go
git commit -m "core: track sent/delivered/read status for 1:1 messages"
```

---

### Task 2: `app/` — parse and render message status in the chat thread

**Files:**
- Modify: `app/src/main/kotlin/com/matheusmortatti/lightwhatsapp/CoreProcess.kt`
  - `Message` data class (currently lines 26-41)
  - `parseMessages` (currently lines 194-210)
- Modify: `app/src/main/kotlin/com/matheusmortatti/lightwhatsapp/MainActivity.kt`
  - `ChatDetailScreen` (currently lines 300-538), specifically the `itemsIndexed` block (currently lines 469-506)
  - `MessageRow` (currently lines 629-700)

**Interfaces:**
- Consumes: Task 1's `"status"` JSON field on a `"messages"` event's entries (`"sent"` / `"delivered"` / `"read"`, absent for group/incoming messages).
- Produces: no new public symbols consumed elsewhere — this is the leaf UI layer.

No automated test infrastructure exists for `app/` today (no `app/src/test` or `app/src/androidTest` directory — every prior feature in this project, per `PROJECT.md`, was verified manually on the LightOS emulator or a real Light Phone III). This task follows that same established convention: build + install + manually verify on-device, rather than introducing a new Compose test harness for one small feature.

- [ ] **Step 1: Add the `status` field to `Message` and parse it**

In `CoreProcess.kt`, the `Message` data class currently reads:

```kotlin
data class Message(
    val id: String,
    val timestamp: Long,
    val fromMe: Boolean,
    val senderName: String?,
    val type: String,
    val text: String,
```

Change to add `status` right after `fromMe`:

```kotlin
data class Message(
    val id: String,
    val timestamp: Long,
    val fromMe: Boolean,
    // "sent" | "delivered" | "read", 1:1 chats only — null for group chats,
    // incoming messages, and messages sent before this field existed. See
    // core/main.go's chatMessage.Status.
    val status: String?,
    val senderName: String?,
    val type: String,
    val text: String,
```

And in `parseMessages`, the construction currently reads:

```kotlin
            Message(
                id = o.getString("id"),
                timestamp = o.optLong("timestamp", 0L),
                fromMe = o.optBoolean("from_me", false),
                senderName = o.optString("sender_name").ifBlank { null },
```

Change to:

```kotlin
            Message(
                id = o.getString("id"),
                timestamp = o.optLong("timestamp", 0L),
                fromMe = o.optBoolean("from_me", false),
                status = o.optString("status").ifBlank { null },
                senderName = o.optString("sender_name").ifBlank { null },
```

- [ ] **Step 2: Compile to catch the constructor-argument break early**

Run: `cd "/Users/matheusmortatti/git/Light Phone Apps/WhatsApp" && ./gradlew :app:compileDebugKotlin -q`
Expected: build succeeds (the only `Message(...)` construction site is the one just updated).

- [ ] **Step 3: Show status on the most recent own message in a 1:1 chat**

In `MainActivity.kt`, `ChatDetailScreen`'s `LightLazyScrollView` block currently reads:

```kotlin
            LightLazyScrollView(
                modifier = Modifier
                    .weight(1f)
                    .fillMaxWidth(),
                listState = listState,
                uniformItemHeightGridUnits = MESSAGE_ROW_HEIGHT_GRID_UNITS,
            ) {
                itemsIndexed(messages, key = { _, message -> message.id }) { index, message ->
                    val previous = messages.getOrNull(index - 1)
                    val sameSender = previous != null &&
                        previous.fromMe == message.fromMe &&
                        (!chat.isGroup || previous.senderName == message.senderName)
                    val withinClusterWindow = previous != null &&
                        (message.timestamp - previous.timestamp) <= MESSAGE_CLUSTER_WINDOW_SECONDS
                    val dateChanged = previous == null || !isSameLocalDate(previous.timestamp, message.timestamp)
                    val showHeader = dateChanged || !(sameSender && withinClusterWindow)
                    Column {
                        if (dateChanged) {
                            DateSeparator(
                                timestampSeconds = message.timestamp,
                                modifier = Modifier.padding(
                                    start = 24.dp,
                                    end = 24.dp,
                                    top = if (index == 0) 0.dp else 16.dp,
                                ),
                            )
                        }
                        MessageRow(
                            message = message,
                            isGroup = chat.isGroup,
                            chatName = chat.name,
                            showHeader = showHeader,
                            modifier = Modifier.padding(
                                start = 24.dp,
                                end = 24.dp,
                                top = if (showHeader) 14.dp else 9.dp,
                            ),
                        )
                    }
                }
            }
```

Change to compute `lastOwnMessageId` once (outside the per-item lambda, so it's not recomputed per row) and pass a `showStatus` flag down, forcing the header on for that one message:

```kotlin
            // Status (see MessageRow) only matters on the message you sent
            // most recently — WhatsApp's own ticks work the same way, older
            // sent messages aren't worth the visual noise. Explicitly null
            // in group chats (rather than relying on message.status being
            // empty there) — otherwise the last own message in a group
            // would still get its header forced visible below, for a
            // status suffix that never renders.
            val lastOwnMessageId = remember(messages, chat.isGroup) {
                if (chat.isGroup) null else messages.lastOrNull { it.fromMe }?.id
            }
            LightLazyScrollView(
                modifier = Modifier
                    .weight(1f)
                    .fillMaxWidth(),
                listState = listState,
                uniformItemHeightGridUnits = MESSAGE_ROW_HEIGHT_GRID_UNITS,
            ) {
                itemsIndexed(messages, key = { _, message -> message.id }) { index, message ->
                    val previous = messages.getOrNull(index - 1)
                    val sameSender = previous != null &&
                        previous.fromMe == message.fromMe &&
                        (!chat.isGroup || previous.senderName == message.senderName)
                    val withinClusterWindow = previous != null &&
                        (message.timestamp - previous.timestamp) <= MESSAGE_CLUSTER_WINDOW_SECONDS
                    val dateChanged = previous == null || !isSameLocalDate(previous.timestamp, message.timestamp)
                    val showHeader = dateChanged || !(sameSender && withinClusterWindow)
                    val showStatus = message.id == lastOwnMessageId
                    Column {
                        if (dateChanged) {
                            DateSeparator(
                                timestampSeconds = message.timestamp,
                                modifier = Modifier.padding(
                                    start = 24.dp,
                                    end = 24.dp,
                                    top = if (index == 0) 0.dp else 16.dp,
                                ),
                            )
                        }
                        MessageRow(
                            message = message,
                            isGroup = chat.isGroup,
                            chatName = chat.name,
                            showHeader = showHeader || showStatus,
                            showStatus = showStatus,
                            modifier = Modifier.padding(
                                start = 24.dp,
                                end = 24.dp,
                                top = if (showHeader || showStatus) 14.dp else 9.dp,
                            ),
                        )
                    }
                }
            }
```

- [ ] **Step 4: Render the status suffix in `MessageRow`**

`MessageRow`'s signature currently reads:

```kotlin
@Composable
private fun MessageRow(
    message: Message,
    isGroup: Boolean,
    chatName: String,
    showHeader: Boolean,
    modifier: Modifier = Modifier,
) {
```

Change to add `showStatus`:

```kotlin
@Composable
private fun MessageRow(
    message: Message,
    isGroup: Boolean,
    chatName: String,
    showHeader: Boolean,
    showStatus: Boolean,
    modifier: Modifier = Modifier,
) {
```

Its header block currently reads:

```kotlin
        if (showHeader) {
            val senderLabel = when {
                message.fromMe -> "You"
                isGroup -> message.senderName?.takeIf { it.isNotBlank() } ?: chatName
                else -> chatName
            }
            Row {
                ChatMetaText(text = senderLabel)
                ChatMetaText(text = "  " + formatMessageTime(message.timestamp))
            }
        }
```

Change to:

```kotlin
        if (showHeader) {
            val senderLabel = when {
                message.fromMe -> "You"
                isGroup -> message.senderName?.takeIf { it.isNotBlank() } ?: chatName
                else -> chatName
            }
            val statusLabel = if (showStatus) formatMessageStatus(message.status) else null
            Row {
                ChatMetaText(text = senderLabel)
                ChatMetaText(text = "  " + formatMessageTime(message.timestamp))
                if (statusLabel != null) {
                    ChatMetaText(text = " · $statusLabel")
                }
            }
        }
```

Then add this helper near `formatMessageTime` (right after it, so status/time/date formatting live together):

```kotlin
// message.status is only ever "sent" | "delivered" | "read" | null (see
// core/main.go's chatMessage.Status) — anything else (including null)
// renders no suffix at all.
private fun formatMessageStatus(status: String?): String? = when (status) {
    "sent" -> "Sent"
    "delivered" -> "Delivered"
    "read" -> "Read"
    else -> null
}
```

- [ ] **Step 5: Compile**

Run: `cd "/Users/matheusmortatti/git/Light Phone Apps/WhatsApp" && ./gradlew :app:compileDebugKotlin -q`
Expected: build succeeds.

- [ ] **Step 6: Build the debug APK**

Run: `cd "/Users/matheusmortatti/git/Light Phone Apps/WhatsApp" && ./gradlew :app:assembleDebug -q`
Expected: build succeeds, produces `app/build/outputs/apk/debug/app-debug.apk`.

- [ ] **Step 7: Install on the real LP3 and manually verify**

Run: `adb -s LP3LHMA551300893 install -r app/build/outputs/apk/debug/app-debug.apk`

Then, on the device (or ask the user to drive this if it needs a second WhatsApp account to reply from):
1. Launch the app, open a 1:1 chat.
2. Send a text message. Confirm its header now reads `You  HH:MM · Sent` immediately (no reopening the chat needed).
3. Have the recipient's phone come online / open the chat. Confirm the label updates live to `· Delivered` then `· Read`, still without reopening the chat on the LP3.
4. Send a second message in the same chat. Confirm the status suffix moves to the new message and the previous one goes back to showing no status suffix (unless clustering already hid its header, in which case it simply disappears along with the header).
5. Open a group chat and confirm no status suffix ever appears there.

- [ ] **Step 8: Commit**

```bash
git add app/src/main/kotlin/com/matheusmortatti/lightwhatsapp/CoreProcess.kt app/src/main/kotlin/com/matheusmortatti/lightwhatsapp/MainActivity.kt
git commit -m "app: show sent/delivered/read status on your latest 1:1 message"
```
