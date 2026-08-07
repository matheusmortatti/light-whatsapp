# Media Perma-Fail State + Voice-Send Feedback Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Close two documented gaps from `docs/superpowers/reviews/2026-08-06-stress-test-findings.md`: media downloads that 403/404/410 forever retry with no "gone" state, and a failed voice-message send that gives the user no feedback.

**Architecture:** Core (Go) classifies download errors via a pure helper and a shared cache-update helper, then flags permanently-failed media on the cached message and stops requeueing it; the app reads the new flags and swaps in an "unavailable" label. Separately, the app's recording screen gets a local failure state that shows a dismissible full-screen error, reusing the existing QR-login error pattern — no core changes needed for this half.

**Tech Stack:** Go (core, `go.mau.fi/whatsmeow` for the download client/errors), Kotlin + Jetpack Compose (app), `kotlin.test` for app unit tests, Go's `testing` package for core.

## Global Constraints

- Permanent-failure classification is HTTP 403/404/410 only, from `go.mau.fi/whatsmeow`'s `DownloadHTTPError` — every other error (network, timeout, hash/HMAC mismatch, unknown) stays transient and keeps the current retry-on-chat-open behavior.
- No manual retry UI for perma-failed media in this pass — terminal, non-interactive label only. A code comment documents the tap-to-retry gap for later.
- No new toast/snackbar/banner widget — the voice-send failure reuses the existing full-screen `LightText` error pattern already used by `LoginState.Error` (`MainActivity.kt:175`).
- Voice-send failure message is generic ("Voice message wasn't sent") — the real exception is only ever logged (`VoiceRecorder.kt:71`), never returned to the caller, and this plan does not change that.
- Self-chat only for any on-device verification sends, per project convention (see `PROJECT.md`/session notes).

---

### Task 1: `isPermanentDownloadFailure` classifier (core)

**Files:**
- Modify: `core/main.go` (add function near `downloadMedia`, i.e. just above line 768)
- Test: `core/main_test.go` (add test near the other `TestExtract*`/`TestApply*` tests)

**Interfaces:**
- Produces: `func isPermanentDownloadFailure(err error) bool` — used by Task 2's `downloadMedia` change.

- [ ] **Step 1: Write the failing test**

Add to `core/main_test.go` (needs `net/http` added to the import block):

```go
func TestIsPermanentDownloadFailure(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"403 is permanent", whatsmeow.DownloadHTTPError{Response: &http.Response{StatusCode: 403}}, true},
		{"404 is permanent", whatsmeow.DownloadHTTPError{Response: &http.Response{StatusCode: 404}}, true},
		{"410 is permanent", whatsmeow.DownloadHTTPError{Response: &http.Response{StatusCode: 410}}, true},
		{"500 is transient", whatsmeow.DownloadHTTPError{Response: &http.Response{StatusCode: 500}}, false},
		{"wrapped 403 is still permanent", fmt.Errorf("download failed: %w", whatsmeow.DownloadHTTPError{Response: &http.Response{StatusCode: 403}}), true},
		{"generic network error is transient", errors.New("connection reset"), false},
		{"nil is transient", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isPermanentDownloadFailure(c.err); got != c.want {
				t.Errorf("isPermanentDownloadFailure(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}
```

Also add `"errors"`, `"fmt"`, and `"net/http"` to `core/main_test.go`'s import block (`core/main_test.go:3-8`, currently `"context"`, `"io"`, `"os"`, `"strings"`, `"testing"`) — none of the three are currently imported there. Insert alphabetically: `"errors"` and `"fmt"` after `"context"`, and `"net/http"` after `"fmt"` and before `"os"`.

- [ ] **Step 2: Run test to verify it fails**

Run: `cd core && go test ./... -run TestIsPermanentDownloadFailure -v`
Expected: FAIL — `isPermanentDownloadFailure` undefined (build error).

- [ ] **Step 3: Write minimal implementation**

Add to `core/main.go`, directly above `downloadMedia` (line 768):

```go
// isPermanentDownloadFailure reports whether err means the media is gone for
// good (expired link, deleted, no longer authorized) rather than a transient
// failure worth retrying on the next chat open. Only HTTP 403/404/410 are
// treated as permanent; everything else (network errors, timeouts,
// decrypt/hash failures, unknown errors) is transient, matching whatsmeow's
// own DownloadHTTPError sentinels (see go.mau.fi/whatsmeow's errors.go).
func isPermanentDownloadFailure(err error) bool {
	var httpErr whatsmeow.DownloadHTTPError
	if !errors.As(err, &httpErr) || httpErr.Response == nil {
		return false
	}
	switch httpErr.StatusCode {
	case 403, 404, 410:
		return true
	default:
		return false
	}
}
```

Add `"errors"` to `core/main.go`'s import block (`core/main.go:16-33`) — it isn't currently imported there. Insert it alphabetically, right after `"encoding/json"` and before `"fmt"`.

- [ ] **Step 4: Run test to verify it passes**

Run: `cd core && go test ./... -run TestIsPermanentDownloadFailure -v`
Expected: PASS (7 subtests).

- [ ] **Step 5: Commit**

```bash
cd "/Users/matheusmortatti/git/Light Phone Apps/WhatsApp"
git add core/main.go core/main_test.go
git commit -m "core: classify 403/404/410 as permanent media-download failures

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>"
```

---

### Task 2: Shared cache-update helper + `chatMessage` failure fields (core)

**Files:**
- Modify: `core/main.go:106` (`chatMessage` struct — add fields)
- Modify: `core/main.go` (add `updateCachedMessage` helper; refactor `downloadMedia`'s success path to use it)
- Test: `core/main_test.go`

**Interfaces:**
- Consumes: `messagesMu sync.Mutex` (`core/main.go:328`), `saveMessages(jid string, list []chatMessage)` (`core/main.go:370`) — both already exist, unchanged.
- Produces: `func updateCachedMessage(jid string, messages map[string][]chatMessage, id string, mutate func(cm *chatMessage)) (chatMessage, bool)` — used by Task 3's failure-handling branch and by `downloadMedia`'s existing success path.

- [ ] **Step 1: Write the failing test**

Add to `core/main_test.go`:

```go
func TestUpdateCachedMessage(t *testing.T) {
	messages := map[string][]chatMessage{
		"jid1": {
			{ID: "a", Text: "hello"},
			{ID: "b", Text: "world"},
		},
	}

	updated, found := updateCachedMessage("jid1", messages, "b", func(cm *chatMessage) {
		cm.Text = "edited"
	})
	if !found {
		t.Fatal("expected found=true for existing message")
	}
	if updated.Text != "edited" {
		t.Errorf("updated.Text = %q, want %q", updated.Text, "edited")
	}
	if messages["jid1"][1].Text != "edited" {
		t.Errorf("messages map not updated in place: got %q", messages["jid1"][1].Text)
	}

	_, found = updateCachedMessage("jid1", messages, "missing-id", func(cm *chatMessage) {})
	if found {
		t.Error("expected found=false for missing message id")
	}

	_, found = updateCachedMessage("missing-jid", messages, "a", func(cm *chatMessage) {})
	if found {
		t.Error("expected found=false for missing jid")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd core && go test ./... -run TestUpdateCachedMessage -v`
Expected: FAIL — `updateCachedMessage` undefined.

- [ ] **Step 3: Write minimal implementation**

Add the four new fields to `chatMessage` in `core/main.go`, immediately after the `StickerMimetype` field (end of the sticker block, around line 165):

```go

	// Set once a permanent download failure is classified (see
	// isPermanentDownloadFailure) — a 403/404/410 on this type's media.
	// handleOpenChat stops requeueing a download for this message once its
	// type's *Failed is true (its *DirectPath is cleared alongside it, which
	// is what handleOpenChat actually checks). The app renders a terminal
	// "unavailable" label instead of retrying. No retry path exists yet for
	// the user to clear this — see the comment in downloadMedia.
	ImageFailed   bool `json:"image_failed,omitempty"`
	AudioFailed   bool `json:"audio_failed,omitempty"`
	VideoFailed   bool `json:"video_failed,omitempty"`
	StickerFailed bool `json:"sticker_failed,omitempty"`
```

Add `updateCachedMessage` directly above `downloadMedia` (below the new `isPermanentDownloadFailure` from Task 1):

```go
// updateCachedMessage finds the message with the given id in messages[jid],
// applies mutate to it, persists the updated list via saveMessages, and
// returns the mutated message plus whether it was found. Used by
// downloadMedia's success and permanent-failure paths so both share the same
// lock/find/persist bookkeeping.
func updateCachedMessage(jid string, messages map[string][]chatMessage, id string, mutate func(cm *chatMessage)) (chatMessage, bool) {
	messagesMu.Lock()
	defer messagesMu.Unlock()

	list, ok := messages[jid]
	if !ok {
		return chatMessage{}, false
	}
	for i, cur := range list {
		if cur.ID == id {
			mutate(&list[i])
			messages[jid] = list
			saveMessages(jid, list)
			return list[i], true
		}
	}
	messages[jid] = list
	return chatMessage{}, false
}
```

Now refactor `downloadMedia`'s existing success-path bookkeeping (`core/main.go:814-830`, the block currently reading `messagesMu.Lock() ... messagesMu.Unlock()`) to use it:

Replace:

```go
	messagesMu.Lock()
	list, ok := messages[jid]
	var updated chatMessage
	found := false
	if ok {
		for i, cur := range list {
			if cur.ID == m.ID {
				apply(&list[i], path)
				updated = list[i]
				found = true
				break
			}
		}
		messages[jid] = list
		saveMessages(jid, list)
	}
	messagesMu.Unlock()

	if found {
		emit(event{Type: "message_update", JID: jid, Messages: resolveMentionsInList(ctx, client, []chatMessage{updated})})
	}
```

with:

```go
	updated, found := updateCachedMessage(jid, messages, m.ID, func(cm *chatMessage) {
		apply(cm, path)
	})
	if found {
		emit(event{Type: "message_update", JID: jid, Messages: resolveMentionsInList(ctx, client, []chatMessage{updated})})
	}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd core && go build ./... && go test ./... -v`
Expected: full package builds, all tests PASS (including the pre-existing ones — this step also catches any regression from the `downloadMedia` refactor).

- [ ] **Step 5: Commit**

```bash
cd "/Users/matheusmortatti/git/Light Phone Apps/WhatsApp"
git add core/main.go core/main_test.go
git commit -m "core: add chatMessage failure fields, share cache-update via updateCachedMessage

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>"
```

---

### Task 3: Wire permanent-failure handling into `downloadMedia` and its four callers (core)

**Files:**
- Modify: `core/main.go:768-835` (`downloadMedia`)
- Modify: `core/main.go:841-853` (`downloadImage`)
- Modify: `core/main.go:880-892` (`downloadAudio`)
- Modify: `core/main.go:913-925` (`downloadVideo`)
- Modify: `core/main.go:946-953` (`downloadSticker`)
- Test: `core/main_test.go`

**Interfaces:**
- Consumes: `isPermanentDownloadFailure(err error) bool` (Task 1), `updateCachedMessage(...)` (Task 2).
- Produces: `downloadMedia` now takes an additional `applyFailure func(cm *chatMessage)` parameter — used by all four download-type callers.

- [ ] **Step 1: Write the failing test**

`downloadMedia` itself takes a `*whatsmeow.Client` and isn't practical to unit test end-to-end (no seam to inject a fake HTTP failure without a real client). Instead, add a test that locks in the exact field-clearing contract each type's `applyFailure` closure must satisfy, since that's the part with real logic (clearing the right fields, leaving others alone):

```go
func TestApplyImageFailureClearsDownloadState(t *testing.T) {
	cm := chatMessage{
		ID:                 "m1",
		Type:               "image",
		ImageDirectPath:    "/some/path",
		ImageMediaKey:      []byte("key"),
		ImageFileSHA256:    []byte("sha"),
		ImageFileEncSHA256: []byte("encsha"),
		ImageMimetype:      "image/jpeg",
		Text:               "caption preserved",
	}
	applyImageFailure(&cm)

	if !cm.ImageFailed {
		t.Error("expected ImageFailed = true")
	}
	if cm.ImageDirectPath != "" {
		t.Errorf("expected ImageDirectPath cleared, got %q", cm.ImageDirectPath)
	}
	if cm.ImageMediaKey != nil || cm.ImageFileSHA256 != nil || cm.ImageFileEncSHA256 != nil || cm.ImageMimetype != "" {
		t.Error("expected all image key material cleared")
	}
	if cm.Text != "caption preserved" {
		t.Error("applyImageFailure must not touch unrelated fields")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd core && go test ./... -run TestApplyImageFailureClearsDownloadState -v`
Expected: FAIL — `applyImageFailure` undefined.

- [ ] **Step 3: Write minimal implementation**

Change `downloadMedia`'s signature (`core/main.go:768-781`) to accept a second callback, and add the permanent-failure branch to its error handling (`core/main.go:803-807`):

```go
func downloadMedia(
	ctx context.Context,
	client *whatsmeow.Client,
	logger waLog.Logger,
	messages map[string][]chatMessage,
	jid string,
	m chatMessage,
	kind string,
	mediaType whatsmeow.MediaType,
	directPath string,
	fileEncSHA256, fileSHA256, mediaKey []byte,
	path string,
	apply func(cm *chatMessage, path string),
	applyFailure func(cm *chatMessage),
) {
```

Replace the transient-only error handling:

```go
	err = client.DownloadMediaWithPathToFile(ctx, directPath, fileEncSHA256, fileSHA256, mediaKey, mediaType, "", false, f)
	closeErr := f.Close()
	if err != nil {
		logger.Warnf("failed to download %s %s/%s: %v", kind, jid, m.ID, err)
		_ = os.Remove(path)
		return
	}
```

with:

```go
	err = client.DownloadMediaWithPathToFile(ctx, directPath, fileEncSHA256, fileSHA256, mediaKey, mediaType, "", false, f)
	closeErr := f.Close()
	if err != nil {
		logger.Warnf("failed to download %s %s/%s: %v", kind, jid, m.ID, err)
		_ = os.Remove(path)
		if isPermanentDownloadFailure(err) {
			// Known gap: no way for the user to manually retry from here —
			// a tap-to-retry would need a new app->core IPC command plus a
			// way to re-derive a fresh direct path, since the one that just
			// 403/404/410'd is gone for good. Worth scoping separately if
			// real-world 404/410s turn out to be transient-in-disguise
			// (e.g. media re-sent under a new id). Found via on-device
			// testing 2026-08-06.
			if updated, found := updateCachedMessage(jid, messages, m.ID, applyFailure); found {
				emit(event{Type: "message_update", JID: jid, Messages: resolveMentionsInList(ctx, client, []chatMessage{updated})})
			}
		}
		return
	}
```

Also delete the now-superseded "Known gap" comment currently at `core/main.go:794-800` (the one this replaces) — leave the surrounding code (the `MkdirAll`/`OpenFile` block above it) as-is.

Add the four type-specific failure closures as named functions (so Task 3's test can call `applyImageFailure` directly), placed right next to each download function:

```go
func applyImageFailure(cm *chatMessage) {
	cm.ImageFailed = true
	cm.ImageDirectPath = ""
	cm.ImageMediaKey = nil
	cm.ImageFileSHA256 = nil
	cm.ImageFileEncSHA256 = nil
	cm.ImageMimetype = ""
}

func applyAudioFailure(cm *chatMessage) {
	cm.AudioFailed = true
	cm.AudioDirectPath = ""
	cm.AudioMediaKey = nil
	cm.AudioFileSHA256 = nil
	cm.AudioFileEncSHA256 = nil
	cm.AudioMimetype = ""
}

func applyVideoFailure(cm *chatMessage) {
	cm.VideoFailed = true
	cm.VideoDirectPath = ""
	cm.VideoMediaKey = nil
	cm.VideoFileSHA256 = nil
	cm.VideoFileEncSHA256 = nil
	cm.VideoMimetype = ""
}

func applyStickerFailure(cm *chatMessage) {
	cm.StickerFailed = true
	cm.StickerDirectPath = ""
	cm.StickerMediaKey = nil
	cm.StickerFileSHA256 = nil
	cm.StickerFileEncSHA256 = nil
	cm.StickerMimetype = ""
}
```

Update each of the four callers to pass its closure as the new last argument:

`downloadImage` (`core/main.go:841-853`):

```go
func downloadImage(ctx context.Context, client *whatsmeow.Client, logger waLog.Logger, messages map[string][]chatMessage, jid string, m chatMessage) {
	path := imagePath(jid, m.ID, m.ImageMimetype)
	downloadMedia(ctx, client, logger, messages, jid, m, "image", whatsmeow.MediaImage,
		m.ImageDirectPath, m.ImageFileEncSHA256, m.ImageFileSHA256, m.ImageMediaKey, path,
		func(cm *chatMessage, path string) {
			cm.ImagePath = path
			cm.ImageDirectPath = ""
			cm.ImageMediaKey = nil
			cm.ImageFileSHA256 = nil
			cm.ImageFileEncSHA256 = nil
			cm.ImageMimetype = ""
		},
		applyImageFailure,
	)
}
```

`downloadAudio` (`core/main.go:880-892`) — same shape, add `applyAudioFailure,` as the new trailing argument after its existing `func(cm *chatMessage, path string) { ... }` closure.

`downloadVideo` (`core/main.go:913-925`) — add `applyVideoFailure,` the same way.

`downloadSticker` (`core/main.go:946-953`, closure body continues past what's shown above — read the full function before editing) — add `applyStickerFailure,` the same way.

- [ ] **Step 4: Run test to verify it passes**

Run: `cd core && go build ./... && go vet ./... && go test ./... -v`
Expected: full package builds cleanly, `go vet` clean, all tests PASS.

- [ ] **Step 5: Commit**

```bash
cd "/Users/matheusmortatti/git/Light Phone Apps/WhatsApp"
git add core/main.go core/main_test.go
git commit -m "core: mark media permanently failed on 403/404/410, stop retrying

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>"
```

---

### Task 4: Surface failure flags through `CoreProcess` and render "unavailable" (app)

**Files:**
- Modify: `app/src/main/kotlin/com/matheusmortatti/lightwhatsapp/CoreProcess.kt` (`Message` data class ~line 26-59, `parseMessages` ~line 252-275)
- Modify: `app/src/main/kotlin/com/matheusmortatti/lightwhatsapp/MainActivity.kt:921-1008` (renderer)
- Modify: `app/src/test/kotlin/com/matheusmortatti/lightwhatsapp/MergeMessagesTest.kt` (constructor call site)

**Interfaces:**
- Consumes: JSON fields `image_failed`/`audio_failed`/`video_failed`/`sticker_failed` from core (Task 3).
- Produces: `Message.imageFailed/audioFailed/videoFailed/stickerFailed: Boolean` — consumed by the renderer in this same task.

- [ ] **Step 1: Add the fields to `Message` and `parseMessages`**

In `CoreProcess.kt`, add four fields to the `Message` data class right after `stickerIsAnimated` (line 55):

```kotlin
    val stickerIsAnimated: Boolean,
    // True once core has classified this media's download as permanently
    // failed (403/404/410 — see core/main.go's isPermanentDownloadFailure).
    // The corresponding *Path stays null forever in that case; no further
    // download will be retried. No retry action exists yet.
    val imageFailed: Boolean,
    val audioFailed: Boolean,
    val videoFailed: Boolean,
    val stickerFailed: Boolean,
```

In `parseMessages` (line 252-275), add the four fields to the `Message(...)` constructor call, right after `stickerIsAnimated`:

```kotlin
                stickerIsAnimated = o.optBoolean("sticker_is_animated", false),
                imageFailed = o.optBoolean("image_failed", false),
                audioFailed = o.optBoolean("audio_failed", false),
                videoFailed = o.optBoolean("video_failed", false),
                stickerFailed = o.optBoolean("sticker_failed", false),
```

- [ ] **Step 2: Fix the now-broken test helper**

In `app/src/test/kotlin/com/matheusmortatti/lightwhatsapp/MergeMessagesTest.kt`, add the four new required parameters to `testMessage`'s `Message(...)` call, right after `stickerIsAnimated = false,`:

```kotlin
    stickerIsAnimated = false,
    imageFailed = false,
    audioFailed = false,
    videoFailed = false,
    stickerFailed = false,
```

- [ ] **Step 3: Run the app's unit tests to verify the build compiles and existing tests still pass**

Run: `cd "/Users/matheusmortatti/git/Light Phone Apps/WhatsApp" && ./gradlew :app:testDebugUnitTest`
Expected: BUILD SUCCESSFUL, `MergeMessagesTest`'s 3 tests still pass (this is a compile-fix step, not new test coverage — the renderer change below has no unit-test seam, see Task 5's on-device verification).

- [ ] **Step 4: Update the renderer**

In `MainActivity.kt`, update all four media branches (lines 921-1008) to check the failure flag when the path is null. Replace:

```kotlin
        when (message.type) {
            "image" -> {
                val path = message.imagePath
                if (path != null) {
                    val bitmap by rememberDecodedImage(path, targetSize = 200.dp, allowAlpha = false)
                    if (bitmap != null) {
                        Image(
                            bitmap = bitmap!!,
                            contentDescription = "Photo",
                            modifier = Modifier
                                .size(200.dp)
                                .padding(bottom = 4.dp),
                        )
                    } else {
                        MessageBodyText(text = "[Photo]", lighten = true, align = bodyAlign)
                    }
                } else {
                    MessageBodyText(text = "[Photo]", lighten = true, align = bodyAlign)
                }
                if (message.text.isNotBlank()) {
                    MessageBodyText(text = message.text, align = bodyAlign)
                }
            }

            "sticker" -> {
                val path = message.stickerPath
                if (path != null) {
                    val bitmap by rememberDecodedImage(path, targetSize = STICKER_SIZE_DP)
                    if (bitmap != null) {
                        Image(
                            bitmap = bitmap!!,
                            contentDescription = "Sticker",
                            modifier = Modifier
                                .size(STICKER_SIZE_DP)
                                .padding(bottom = 4.dp),
                        )
                    } else {
                        MessageBodyText(text = "[Sticker]", lighten = true, align = bodyAlign)
                    }
                } else {
                    MessageBodyText(text = "[Sticker]", lighten = true, align = bodyAlign)
                }
            }

            "video", "gif" -> {
                val path = message.videoPath
                if (path != null) {
                    val thumbnail by rememberDecodedVideoThumbnail(path)
                    Box(
                        modifier = Modifier
                            .size(200.dp)
                            .padding(bottom = 4.dp)
                            .lightClickable { onPlayVideo(path, message.isGif) },
                        contentAlignment = Alignment.Center,
                    ) {
                        if (thumbnail != null) {
                            Image(
                                bitmap = thumbnail!!,
                                contentDescription = if (message.isGif) "GIF" else "Video",
                                modifier = Modifier.size(200.dp),
                            )
                        }
                        LightIcon(
                            icon = LightIcons.PLAY,
                            size = 3f,
                            contentDescription = "Play",
                        )
                    }
                } else {
                    MessageBodyText(
                        text = if (message.isGif) "[GIF]" else "[Video]",
                        lighten = true,
                        align = bodyAlign,
                    )
                }
                if (message.text.isNotBlank()) {
                    MessageBodyText(text = message.text, align = bodyAlign)
                }
            }

            "audio" -> {
                val path = message.audioPath
                if (path != null) {
                    AudioMessageRow(relativePath = path, seconds = message.audioSeconds)
                } else {
                    MessageBodyText(text = "[Voice message]", lighten = true, align = bodyAlign)
                }
            }
```

with:

```kotlin
        when (message.type) {
            "image" -> {
                val path = message.imagePath
                if (path != null) {
                    val bitmap by rememberDecodedImage(path, targetSize = 200.dp, allowAlpha = false)
                    if (bitmap != null) {
                        Image(
                            bitmap = bitmap!!,
                            contentDescription = "Photo",
                            modifier = Modifier
                                .size(200.dp)
                                .padding(bottom = 4.dp),
                        )
                    } else {
                        MessageBodyText(text = "[Photo]", lighten = true, align = bodyAlign)
                    }
                } else if (message.imageFailed) {
                    MessageBodyText(text = "[Photo unavailable]", lighten = true, align = bodyAlign)
                } else {
                    MessageBodyText(text = "[Photo]", lighten = true, align = bodyAlign)
                }
                if (message.text.isNotBlank()) {
                    MessageBodyText(text = message.text, align = bodyAlign)
                }
            }

            "sticker" -> {
                val path = message.stickerPath
                if (path != null) {
                    val bitmap by rememberDecodedImage(path, targetSize = STICKER_SIZE_DP)
                    if (bitmap != null) {
                        Image(
                            bitmap = bitmap!!,
                            contentDescription = "Sticker",
                            modifier = Modifier
                                .size(STICKER_SIZE_DP)
                                .padding(bottom = 4.dp),
                        )
                    } else {
                        MessageBodyText(text = "[Sticker]", lighten = true, align = bodyAlign)
                    }
                } else if (message.stickerFailed) {
                    MessageBodyText(text = "[Sticker unavailable]", lighten = true, align = bodyAlign)
                } else {
                    MessageBodyText(text = "[Sticker]", lighten = true, align = bodyAlign)
                }
            }

            "video", "gif" -> {
                val path = message.videoPath
                if (path != null) {
                    val thumbnail by rememberDecodedVideoThumbnail(path)
                    Box(
                        modifier = Modifier
                            .size(200.dp)
                            .padding(bottom = 4.dp)
                            .lightClickable { onPlayVideo(path, message.isGif) },
                        contentAlignment = Alignment.Center,
                    ) {
                        if (thumbnail != null) {
                            Image(
                                bitmap = thumbnail!!,
                                contentDescription = if (message.isGif) "GIF" else "Video",
                                modifier = Modifier.size(200.dp),
                            )
                        }
                        LightIcon(
                            icon = LightIcons.PLAY,
                            size = 3f,
                            contentDescription = "Play",
                        )
                    }
                } else if (message.videoFailed) {
                    MessageBodyText(
                        text = if (message.isGif) "[GIF unavailable]" else "[Video unavailable]",
                        lighten = true,
                        align = bodyAlign,
                    )
                } else {
                    MessageBodyText(
                        text = if (message.isGif) "[GIF]" else "[Video]",
                        lighten = true,
                        align = bodyAlign,
                    )
                }
                if (message.text.isNotBlank()) {
                    MessageBodyText(text = message.text, align = bodyAlign)
                }
            }

            "audio" -> {
                val path = message.audioPath
                if (path != null) {
                    AudioMessageRow(relativePath = path, seconds = message.audioSeconds)
                } else if (message.audioFailed) {
                    MessageBodyText(text = "[Voice message unavailable]", lighten = true, align = bodyAlign)
                } else {
                    MessageBodyText(text = "[Voice message]", lighten = true, align = bodyAlign)
                }
            }
```

- [ ] **Step 5: Compile-check**

Run: `cd "/Users/matheusmortatti/git/Light Phone Apps/WhatsApp" && ./gradlew :app:compileDebugKotlin -q`
Expected: no output, exit code 0.

- [ ] **Step 6: Commit**

```bash
cd "/Users/matheusmortatti/git/Light Phone Apps/WhatsApp"
git add app/src/main/kotlin/com/matheusmortatti/lightwhatsapp/CoreProcess.kt app/src/main/kotlin/com/matheusmortatti/lightwhatsapp/MainActivity.kt app/src/test/kotlin/com/matheusmortatti/lightwhatsapp/MergeMessagesTest.kt
git commit -m "app: render \"unavailable\" for permanently failed media

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>"
```

---

### Task 5: On-device verification of media perma-fail (manual, real LP3)

**Files:** none (verification only)

**Interfaces:** none

- [ ] **Step 1: Build and deploy**

Use the `lp3-tools:deploy-debug` skill (or the project's usual `adb`-based build/sideload flow) to build and install the app on the real LP3, serial `LP3LHMA551300893`.

- [ ] **Step 2: Reproduce the known 403 case**

Per the 2026-08-06 review doc, self-chat already has a cached image predating pairing that 403s on every retry. Open that self-chat.

- [ ] **Step 3: Verify the label and no-retry-spam behavior**

Confirm the placeholder now reads "[Photo unavailable]" instead of the old plain "[Photo]", and that `adb logcat` no longer shows a repeated download-retry warning for that message across multiple chat-open/close cycles (open the chat, back out, reopen 2-3 times).

- [ ] **Step 4: Confirm no regression to normal media**

In the same self-chat (or another with real image/audio/video/sticker messages), confirm media that *does* download successfully still renders normally (image/sticker bitmap, audio playback row, video thumbnail+play).

- [ ] **Step 5: Update the review doc**

Edit `docs/superpowers/reviews/2026-08-06-stress-test-findings.md`: remove the "Media downloads retry forever..." bullet from "Known gaps — documented, not fixed" (or move it to a "closed" note referencing this plan's commits), since it's now fixed.

```bash
cd "/Users/matheusmortatti/git/Light Phone Apps/WhatsApp"
git add docs/superpowers/reviews/2026-08-06-stress-test-findings.md
git commit -m "docs: mark media perma-fail gap closed

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>"
```

---

### Task 6: Voice-send failure feedback screen (app)

**Files:**
- Modify: `app/src/main/kotlin/com/matheusmortatti/lightwhatsapp/MainActivity.kt` (composer state + `RecordingScreen` call site, ~lines 450-476; error rendering, modeled on `LoginState.Error` at line 175)

**Interfaces:** none (self-contained local Compose state; no new function signatures consumed elsewhere)

- [ ] **Step 1: Add a `RecordingFailedScreen` composable**

Model this directly on `VideoPlayerScreen`'s `playbackError` state (`MainActivity.kt:718-747`, itself modeled on `RecordingScreen` at line 678) — a `LightTopBar` with a `CLOSE` icon as the dismiss action, matching the app's existing full-screen-with-dismiss pattern. Add this new composable right after `RecordingScreen` (after line 716):

```kotlin
@Composable
private fun RecordingFailedScreen(
    chatName: String,
    onDismiss: () -> Unit,
) {
    Column(
        modifier = Modifier
            .fillMaxSize()
            .background(LightThemeTokens.colors.background),
    ) {
        LightTopBar(
            leftButton = LightBarButton.LightIcon(icon = LightIcons.CLOSE, onClick = onDismiss),
            center = LightTopBarCenter.Text(chatName),
        )
        Box(
            modifier = Modifier
                .weight(1f)
                .fillMaxWidth(),
            contentAlignment = Alignment.Center,
        ) {
            LightText(
                text = "Voice message wasn't sent",
                variant = LightTextVariant.Copy,
                lighten = true,
            )
        }
    }
}
```

- [ ] **Step 2: Add failure state and branch**

Read `MainActivity.kt` lines 420-480 first (the composer's `recording` state and the `RecordingScreen` call site at line 457) to see the exact surrounding state declarations before editing. Add a `recordingFailed` state variable alongside the existing `recording` state (same `remember { mutableStateOf(...) }` or `var ... by remember` pattern already used for `recording` in that block), and add a new early-return branch for it, placed before the existing `if (recording) { ... }` block (line 450) so it takes priority when both could theoretically be relevant:

```kotlin
    if (recordingFailed) {
        RecordingFailedScreen(
            chatName = chat.name,
            onDismiss = { recordingFailed = false },
        )
        return
    }
```

- [ ] **Step 3: Set the failure state on a null `stop()` result**

In the existing `RecordingScreen`'s `onSend` (around line 461-473), replace:

```kotlin
            onSend = {
                // Known gap: when stop() rejects the recording (see
                // VoiceRecorder's Log.w) this screen just closes with no
                // message sent and no user-facing error — logged now, but
                // still indistinguishable from a normal cancel to the user.
                // Found via on-device testing 2026-08-06; not fixed.
                val result = voiceRecorder.stop()
                recording = false
                if (result != null) {
                    val (file, durationMs) = result
                    onSendAudio(file.relativeTo(context.filesDir).path, durationMs)
                }
            },
```

with:

```kotlin
            onSend = {
                val result = voiceRecorder.stop()
                recording = false
                if (result != null) {
                    val (file, durationMs) = result
                    onSendAudio(file.relativeTo(context.filesDir).path, durationMs)
                } else {
                    recordingFailed = true
                }
            },
```

- [ ] **Step 4: Compile-check**

Run: `cd "/Users/matheusmortatti/git/Light Phone Apps/WhatsApp" && ./gradlew :app:compileDebugKotlin -q`
Expected: no output, exit code 0.

- [ ] **Step 5: Commit**

```bash
cd "/Users/matheusmortatti/git/Light Phone Apps/WhatsApp"
git add app/src/main/kotlin/com/matheusmortatti/lightwhatsapp/MainActivity.kt
git commit -m "app: show dismissible error when voice-message send fails

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>"
```

---

### Task 7: On-device verification of voice-send feedback (manual, real LP3)

**Files:** none (verification only)

**Interfaces:** none

- [ ] **Step 1: Build and deploy**

Same as Task 5 Step 1 — rebuild/reinstall via `lp3-tools:deploy-debug` on the real LP3 (serial `LP3LHMA551300893`), since Task 6's change is on top of Task 4's already-deployed build.

- [ ] **Step 2: Reproduce the `stop()` rejection**

In self-chat, start a voice recording and release almost immediately (very short recording — the documented repro for `MediaRecorder.stop()`'s `RuntimeException`).

- [ ] **Step 3: Verify the error screen**

Confirm the "Voice message wasn't sent" screen appears (instead of silently returning to the composer), and that tapping it dismisses back to the normal composer.

- [ ] **Step 4: Confirm no regression to the happy path**

Record a normal-length voice message in self-chat and confirm it still sends and plays back normally (no regression from Task 6's changes).

- [ ] **Step 5: Update the review doc**

Edit `docs/superpowers/reviews/2026-08-06-stress-test-findings.md`: remove the "A failed voice-recording send gives no user-facing feedback" bullet from "Known gaps — documented, not fixed" (or move it to a "closed" note referencing this plan's commits).

```bash
cd "/Users/matheusmortatti/git/Light Phone Apps/WhatsApp"
git add docs/superpowers/reviews/2026-08-06-stress-test-findings.md
git commit -m "docs: mark voice-send feedback gap closed

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>"
```
