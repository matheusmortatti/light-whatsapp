# Video/GIF/Sticker Message Support Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Render incoming WhatsApp video, GIF, and sticker messages in the chat thread, receive-only, matching how images already work.

**Architecture:** Extend the existing image/audio pipeline (extract in `core/main.go` → persist a download reference on `chatMessage` → lazily download on `open_chat`/live arrival → emit → render in `app/`'s `MessageRow`) to two new WhatsApp content types (`VideoMessage`, covering both real video and GIF via its `GifPlayback` flag, and `StickerMessage`). No new IPC commands — `open_chat` already triggers downloads for whatever's missing.

**Tech Stack:** Go (`core/`, whatsmeow), Kotlin/Compose (`app/`), no new dependencies on either side (video plays via Android's built-in `VideoView`, stickers decode via the existing `BitmapFactory` path).

## Global Constraints

- Receive-only — no send path for video/GIF/stickers in this pass (spec: `docs/superpowers/specs/2026-08-03-video-gif-sticker-design.md`).
- Lottie stickers (`StickerMessage.IsLottie`) are explicitly out of scope — must fall through to the existing `"unsupported"` label path, not attempt to render.
- No new Gradle dependencies in `app/` (see spec's Design section — hand-rolled media handling, consistent with the project's existing `VoiceRecorder`).
- Follow the exact `Image*`/`Audio*` field/function naming pattern already established in `core/main.go` for every new type (`Video*`, `Sticker*`).

---

### Task 1: Go — extend `extractMessage` for video/GIF/sticker, with tests

**Files:**
- Modify: `core/main.go:304-379` (`extractMessage`, `unsupportedMessageLabel`)
- Test: `core/main_test.go`

**Interfaces:**
- Produces: `extractMessage(m *waE2E.Message) (text, msgType string, img *waE2E.ImageMessage, audio *waE2E.AudioMessage, video *waE2E.VideoMessage, sticker *waE2E.StickerMessage, ok bool)` — two new out-params inserted after `audio`, before `ok`. `msgType` can now also be `"video"`, `"gif"`, or `"sticker"`.
- Consumes: nothing new (this task only touches extraction, not the callers — those are updated in Task 2).

Note: after this task, `core/main.go` will **not compile** — `extractHistoryMessage` and `handleMessage` still call `extractMessage` with the old 5-return-value signature. That's expected; Task 2 fixes both call sites immediately after. Do not run `go build` between Task 1 and Task 2; the test in this task runs via `go test -run TestExtractMessage` (path-scoped), which is not affected by the other functions failing to compile... actually Go test builds the whole package, so **Task 1 and Task 2 must be done together before running any build/test**. Steps below reflect this: write both tasks' code, then build/test once.

- [ ] **Step 1: Update `extractMessage`'s signature and body**

Replace the whole function at `core/main.go:304-327`:

```go
func extractMessage(m *waE2E.Message) (text, msgType string, img *waE2E.ImageMessage, audio *waE2E.AudioMessage, video *waE2E.VideoMessage, sticker *waE2E.StickerMessage, ok bool) {
	for i := 0; i < 4 && m != nil; i++ {
		switch {
		case m.GetConversation() != "":
			return m.GetConversation(), "text", nil, nil, nil, nil, true
		case m.GetExtendedTextMessage() != nil:
			return m.GetExtendedTextMessage().GetText(), "text", nil, nil, nil, nil, true
		case m.GetImageMessage() != nil:
			im := m.GetImageMessage()
			return im.GetCaption(), "image", im, nil, nil, nil, true
		case m.GetAudioMessage() != nil:
			return "", "audio", nil, m.GetAudioMessage(), nil, nil, true
		case m.GetVideoMessage() != nil:
			vm := m.GetVideoMessage()
			vType := "video"
			if vm.GetGifPlayback() {
				vType = "gif"
			}
			return vm.GetCaption(), vType, nil, nil, vm, nil, true
		case m.GetStickerMessage() != nil && !m.GetStickerMessage().GetIsLottie():
			return "", "sticker", nil, nil, nil, m.GetStickerMessage(), true
		case m.GetEphemeralMessage() != nil:
			m = m.GetEphemeralMessage().GetMessage()
		case m.GetViewOnceMessage() != nil:
			m = m.GetViewOnceMessage().GetMessage()
		case m.GetViewOnceMessageV2() != nil:
			m = m.GetViewOnceMessageV2().GetMessage()
		default:
			return unsupportedMessageLabel(m), "unsupported", nil, nil, nil, nil, true
		}
	}
	return "", "", nil, nil, nil, nil, false
}
```

A Lottie sticker's `case` condition above is `false` (guarded by `!IsLottie`), so it falls through the loop to `default` and lands in `unsupportedMessageLabel` — handled by Step 2.

- [ ] **Step 2: Add a Lottie-sticker special case to `unsupportedMessageLabel`**

In `core/main.go`, `unsupportedMessageLabel` (currently `:336-363`) already special-cases GIF before its generic reflection walk:

```go
	if vm := m.GetVideoMessage(); vm != nil && vm.GetGifPlayback() {
		return "gif"
	}
```

Add a matching case right after it, so a Lottie sticker gets the label `"lottie sticker"` instead of the generic reflection-derived `"sticker"`:

```go
	if vm := m.GetVideoMessage(); vm != nil && vm.GetGifPlayback() {
		return "gif"
	}
	if sm := m.GetStickerMessage(); sm != nil && sm.GetIsLottie() {
		return "lottie sticker"
	}
```

- [ ] **Step 3: Do not build/test yet — proceed to Task 2's call-site updates first, then return here for Step 4.**

(This step is a marker, not an action — `core/main.go` will not compile until Task 2's edits land too, since `extractHistoryMessage`/`handleMessage` still call the old 5-value signature.)

- [ ] **Step 4: Write the new test cases**

Add to `core/main_test.go`. First, add the `proto` import (needed for `proto.String`/`proto.Bool`) alongside the existing `waE2E` import — the test file currently imports only `testing`, `go.mau.fi/whatsmeow/types`, and `go.mau.fi/whatsmeow/types/events`:

```go
import (
	"testing"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)
```

Then append this test function at the end of `core/main_test.go`:

```go
func TestExtractMessage(t *testing.T) {
	t.Run("video message", func(t *testing.T) {
		msg := &waE2E.Message{VideoMessage: &waE2E.VideoMessage{Caption: proto.String("look")}}
		text, msgType, _, _, video, sticker, ok := extractMessage(msg)
		if !ok || msgType != "video" || text != "look" || video == nil || sticker != nil {
			t.Fatalf("extractMessage() = text=%q type=%q video=%v sticker=%v ok=%v", text, msgType, video, sticker, ok)
		}
	})

	t.Run("gif is a video message with GifPlayback set", func(t *testing.T) {
		msg := &waE2E.Message{VideoMessage: &waE2E.VideoMessage{GifPlayback: proto.Bool(true)}}
		_, msgType, _, _, video, _, ok := extractMessage(msg)
		if !ok || msgType != "gif" || video == nil {
			t.Fatalf("extractMessage() = type=%q video=%v ok=%v", msgType, video, ok)
		}
	})

	t.Run("sticker message", func(t *testing.T) {
		msg := &waE2E.Message{StickerMessage: &waE2E.StickerMessage{IsAnimated: proto.Bool(true)}}
		_, msgType, _, _, _, sticker, ok := extractMessage(msg)
		if !ok || msgType != "sticker" || sticker == nil {
			t.Fatalf("extractMessage() = type=%q sticker=%v ok=%v", msgType, sticker, ok)
		}
	})

	t.Run("lottie sticker falls back to unsupported", func(t *testing.T) {
		msg := &waE2E.Message{StickerMessage: &waE2E.StickerMessage{IsLottie: proto.Bool(true)}}
		text, msgType, _, _, _, sticker, ok := extractMessage(msg)
		if !ok || msgType != "unsupported" || text != "lottie sticker" || sticker != nil {
			t.Fatalf("extractMessage() = text=%q type=%q sticker=%v ok=%v", text, msgType, sticker, ok)
		}
	})
}
```

Continue directly to Task 2 — the build/test run happens once, at the end of Task 2.

---

### Task 2: Go — `chatMessage` fields, `setVideoFields`/`setStickerFields`, download plumbing, wiring

**Files:**
- Modify: `core/main.go` (`chatMessage` struct at `:92-131`; `extractHistoryMessage` at `:466-516`; new functions after `setAudioFields` at `:453`; new download functions after `downloadAudio` at `:648`; `handleOpenChat` at `:832-864`; `handleMessage` at `:1242-1322`)

**Interfaces:**
- Consumes: `extractMessage`'s new signature from Task 1.
- Produces: `setVideoFields(cm *chatMessage, v *waE2E.VideoMessage)`, `setStickerFields(cm *chatMessage, s *waE2E.StickerMessage)`, `videoPath(jid, msgID, mimetype string) string`, `stickerPath(jid, msgID, mimetype string) string`, `downloadVideo(ctx, client, logger, messages, jid, m)`, `downloadSticker(ctx, client, logger, messages, jid, m)` — new `chatMessage` JSON fields `video_path`, `video_seconds`, `is_gif`, `sticker_path`, `sticker_is_animated` (plus their `*_direct_path`/`*_media_key`/`*_file_sha256`/`*_file_enc_sha256`/`*_mimetype` download-reference counterparts) consumed by Task 3's Kotlin parsing.

- [ ] **Step 1: Add the new field groups to `chatMessage`**

In `core/main.go`, insert after the existing `Audio*` fields (right before the struct's closing `}` at `:131`):

```go
	VideoPath          string `json:"video_path,omitempty"`    // path (relative to the working dir) once downloaded
	VideoSeconds       uint32 `json:"video_seconds,omitempty"` // duration, known up front unlike images
	IsGif              bool   `json:"is_gif,omitempty"`        // true if this "video" is WhatsApp's GIF encoding (VideoMessage.GifPlayback)

	// Same deal as the Image*/Audio* fields above, but for video (see downloadVideo).
	VideoDirectPath    string `json:"video_direct_path,omitempty"`
	VideoMediaKey      []byte `json:"video_media_key,omitempty"`
	VideoFileSHA256    []byte `json:"video_file_sha256,omitempty"`
	VideoFileEncSHA256 []byte `json:"video_file_enc_sha256,omitempty"`
	VideoMimetype      string `json:"video_mimetype,omitempty"`

	StickerPath       string `json:"sticker_path,omitempty"`        // path (relative to the working dir) once downloaded
	StickerIsAnimated bool   `json:"sticker_is_animated,omitempty"` // WhatsApp stickers are usually static or animated WebP

	// Same deal again, but for stickers (see downloadSticker).
	StickerDirectPath    string `json:"sticker_direct_path,omitempty"`
	StickerMediaKey      []byte `json:"sticker_media_key,omitempty"`
	StickerFileSHA256    []byte `json:"sticker_file_sha256,omitempty"`
	StickerFileEncSHA256 []byte `json:"sticker_file_enc_sha256,omitempty"`
	StickerMimetype      string `json:"sticker_mimetype,omitempty"`
```

- [ ] **Step 2: Add `setVideoFields`/`setStickerFields`**

Insert directly after `setAudioFields` (`core/main.go:453-460`):

```go
// setVideoFields is setImageFields' counterpart for video (and GIF, which
// WhatsApp encodes as a VideoMessage with GifPlayback set — see
// extractMessage) — fills in cm's persisted download reference from v, plus
// its duration and whether it's a GIF.
func setVideoFields(cm *chatMessage, v *waE2E.VideoMessage) {
	cm.VideoDirectPath = v.GetDirectPath()
	cm.VideoMediaKey = v.GetMediaKey()
	cm.VideoFileSHA256 = v.GetFileSHA256()
	cm.VideoFileEncSHA256 = v.GetFileEncSHA256()
	cm.VideoMimetype = v.GetMimetype()
	cm.VideoSeconds = v.GetSeconds()
	cm.IsGif = v.GetGifPlayback()
}

// setStickerFields is setImageFields' counterpart for stickers — fills in
// cm's persisted download reference from s, plus whether it's animated.
// Lottie stickers never reach here (see extractMessage).
func setStickerFields(cm *chatMessage, s *waE2E.StickerMessage) {
	cm.StickerDirectPath = s.GetDirectPath()
	cm.StickerMediaKey = s.GetMediaKey()
	cm.StickerFileSHA256 = s.GetFileSHA256()
	cm.StickerFileEncSHA256 = s.GetFileEncSHA256()
	cm.StickerMimetype = s.GetMimetype()
	cm.StickerIsAnimated = s.GetIsAnimated()
}
```

- [ ] **Step 3: Add `videoExtension`/`videoPath` and `stickerExtension`/`stickerPath`**

Insert directly after `downloadAudio` (`core/main.go:648`), before `markChatRead`:

```go
// videoExtension maps a media mimetype to a file extension; WhatsApp videos
// (and GIFs, encoded as videos — see extractMessage) are almost always
// MP4/H.264, so that's the fallback for anything unrecognized.
func videoExtension(mimetype string) string {
	switch {
	case strings.Contains(mimetype, "3gpp"):
		return "3gp"
	default:
		return "mp4"
	}
}

func videoPath(jid, msgID, mimetype string) string {
	return filepath.Join("media", jid, msgID+"."+videoExtension(mimetype))
}

// downloadVideo is downloadImage's counterpart for video/GIF messages:
// fetches the media (using the persisted download reference set by
// setVideoFields), writes it to disk, and updates the stored chatMessage
// with the resulting path, re-emitting the chat's message list so the app
// can render a thumbnail/player once it lands. Meant to run in its own
// goroutine.
func downloadVideo(ctx context.Context, client *whatsmeow.Client, logger waLog.Logger, messages map[string][]chatMessage, jid string, m chatMessage) {
	data, err := client.DownloadMediaWithPath(ctx, m.VideoDirectPath, m.VideoFileEncSHA256, m.VideoFileSHA256, m.VideoMediaKey, whatsmeow.MediaVideo, "", false)
	if err != nil {
		logger.Warnf("failed to download video %s/%s: %v", jid, m.ID, err)
		return
	}
	path := videoPath(jid, m.ID, m.VideoMimetype)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		logger.Warnf("failed to create media dir for %s: %v", jid, err)
		return
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		logger.Warnf("failed to write video %s/%s: %v", jid, m.ID, err)
		return
	}

	messagesMu.Lock()
	list, ok := messages[jid]
	if ok {
		for i, cur := range list {
			if cur.ID == m.ID {
				list[i].VideoPath = path
				list[i].VideoDirectPath = ""
				list[i].VideoMediaKey = nil
				list[i].VideoFileSHA256 = nil
				list[i].VideoFileEncSHA256 = nil
				list[i].VideoMimetype = ""
				break
			}
		}
		messages[jid] = list
		saveMessages(jid, list)
	}
	messagesMu.Unlock()

	if ok {
		emit(event{Type: "messages", JID: jid, Messages: resolveMentionsInList(ctx, client, list)})
	}
}

// stickerExtension maps a media mimetype to a file extension; WhatsApp
// stickers are almost always WebP (static or animated).
func stickerExtension(mimetype string) string {
	switch mimetype {
	case "image/png":
		return "png"
	default:
		return "webp"
	}
}

func stickerPath(jid, msgID, mimetype string) string {
	return filepath.Join("media", jid, msgID+"."+stickerExtension(mimetype))
}

// downloadSticker is downloadImage's counterpart for sticker messages.
// Stickers download as whatsmeow.MediaImage (confirmed via whatsmeow's
// classToMediaType map in download.go — there's no separate sticker media
// type). Meant to run in its own goroutine.
func downloadSticker(ctx context.Context, client *whatsmeow.Client, logger waLog.Logger, messages map[string][]chatMessage, jid string, m chatMessage) {
	data, err := client.DownloadMediaWithPath(ctx, m.StickerDirectPath, m.StickerFileEncSHA256, m.StickerFileSHA256, m.StickerMediaKey, whatsmeow.MediaImage, "", false)
	if err != nil {
		logger.Warnf("failed to download sticker %s/%s: %v", jid, m.ID, err)
		return
	}
	path := stickerPath(jid, m.ID, m.StickerMimetype)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		logger.Warnf("failed to create media dir for %s: %v", jid, err)
		return
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		logger.Warnf("failed to write sticker %s/%s: %v", jid, m.ID, err)
		return
	}

	messagesMu.Lock()
	list, ok := messages[jid]
	if ok {
		for i, cur := range list {
			if cur.ID == m.ID {
				list[i].StickerPath = path
				list[i].StickerDirectPath = ""
				list[i].StickerMediaKey = nil
				list[i].StickerFileSHA256 = nil
				list[i].StickerFileEncSHA256 = nil
				list[i].StickerMimetype = ""
				break
			}
		}
		messages[jid] = list
		saveMessages(jid, list)
	}
	messagesMu.Unlock()

	if ok {
		emit(event{Type: "messages", JID: jid, Messages: resolveMentionsInList(ctx, client, list)})
	}
}
```

- [ ] **Step 4: Update `extractHistoryMessage`'s call site**

In `core/main.go`, replace lines `475-492`:

```go
	text, msgType, img, audio, ok := extractMessage(waMsg)
	if !ok {
		return
	}

	cm := chatMessage{
		ID:        key.GetID(),
		Timestamp: int64(info.GetMessageTimestamp()),
		FromMe:    key.GetFromMe(),
		Type:      msgType,
		Text:      text,
	}
	if msgType == "image" && img != nil {
		setImageFields(&cm, img)
	}
	if msgType == "audio" && audio != nil {
		setAudioFields(&cm, audio)
	}
```

with:

```go
	text, msgType, img, audio, video, sticker, ok := extractMessage(waMsg)
	if !ok {
		return
	}

	cm := chatMessage{
		ID:        key.GetID(),
		Timestamp: int64(info.GetMessageTimestamp()),
		FromMe:    key.GetFromMe(),
		Type:      msgType,
		Text:      text,
	}
	if msgType == "image" && img != nil {
		setImageFields(&cm, img)
	}
	if msgType == "audio" && audio != nil {
		setAudioFields(&cm, audio)
	}
	if (msgType == "video" || msgType == "gif") && video != nil {
		setVideoFields(&cm, video)
	}
	if msgType == "sticker" && sticker != nil {
		setStickerFields(&cm, sticker)
	}
```

- [ ] **Step 5: Update `handleOpenChat`'s lazy-download scan**

In `core/main.go`, replace the whole function body (`:832-864`):

```go
func handleOpenChat(ctx context.Context, client *whatsmeow.Client, logger waLog.Logger, jid string, chats map[string]chatSummary, messages map[string][]chatMessage) {
	setOpenChatJID(jid)

	messagesMu.Lock()
	list, ok := messages[jid]
	if !ok {
		list = loadCachedMessages(jid)
		messages[jid] = list
	}
	var toDownload []chatMessage
	var toDownloadAudio []chatMessage
	var toDownloadVideo []chatMessage
	var toDownloadSticker []chatMessage
	for _, m := range list {
		if m.Type == "image" && m.ImagePath == "" && m.ImageDirectPath != "" {
			toDownload = append(toDownload, m)
		}
		if m.Type == "audio" && m.AudioPath == "" && m.AudioDirectPath != "" {
			toDownloadAudio = append(toDownloadAudio, m)
		}
		if (m.Type == "video" || m.Type == "gif") && m.VideoPath == "" && m.VideoDirectPath != "" {
			toDownloadVideo = append(toDownloadVideo, m)
		}
		if m.Type == "sticker" && m.StickerPath == "" && m.StickerDirectPath != "" {
			toDownloadSticker = append(toDownloadSticker, m)
		}
	}
	messagesMu.Unlock()

	logger.Debugf("open_chat %s: %d cached messages, %d images to download, %d audio to download, %d video to download, %d stickers to download", jid, len(list), len(toDownload), len(toDownloadAudio), len(toDownloadVideo), len(toDownloadSticker))
	emit(event{Type: "messages", JID: jid, Messages: resolveMentionsInList(ctx, client, list)})

	go markChatReadAndClearUnread(ctx, client, logger, jid, list, chats)

	for _, m := range toDownload {
		go downloadImage(ctx, client, logger, messages, jid, m)
	}
	for _, m := range toDownloadAudio {
		go downloadAudio(ctx, client, logger, messages, jid, m)
	}
	for _, m := range toDownloadVideo {
		go downloadVideo(ctx, client, logger, messages, jid, m)
	}
	for _, m := range toDownloadSticker {
		go downloadSticker(ctx, client, logger, messages, jid, m)
	}
}
```

- [ ] **Step 6: Update `handleMessage`'s call site and live-download dispatch**

In `core/main.go`, replace lines `1284-1321`:

```go
	text, msgType, img, audio, ok := extractMessage(evt.Message)
	if !ok {
		return
	}
	cm := chatMessage{
		ID:        evt.Info.ID,
		Timestamp: timestamp,
		FromMe:    evt.Info.IsFromMe,
		Type:      msgType,
		Text:      text,
	}
	if msgType == "image" && img != nil {
		setImageFields(&cm, img)
	}
	if msgType == "audio" && audio != nil {
		setAudioFields(&cm, audio)
	}
	if evt.Info.IsGroup && !evt.Info.IsFromMe {
		cm.Sender = evt.Info.Sender.String()
		cm.SenderName = evt.Info.PushName
	}

	messagesMu.Lock()
	msgList := upsertMessage(messages, jid.String(), cm)
	messagesMu.Unlock()

	emit(event{Type: "messages", JID: jid.String(), Messages: resolveMentionsInList(ctx, client, msgList)})

	if !evt.Info.IsFromMe && isOpen {
		go markChatRead(ctx, client, logger, jid, []chatMessage{cm})
	}

	if msgType == "image" && img != nil {
		go downloadImage(ctx, client, logger, messages, jid.String(), cm)
	}
	if msgType == "audio" && audio != nil {
		go downloadAudio(ctx, client, logger, messages, jid.String(), cm)
	}
}
```

with:

```go
	text, msgType, img, audio, video, sticker, ok := extractMessage(evt.Message)
	if !ok {
		return
	}
	cm := chatMessage{
		ID:        evt.Info.ID,
		Timestamp: timestamp,
		FromMe:    evt.Info.IsFromMe,
		Type:      msgType,
		Text:      text,
	}
	if msgType == "image" && img != nil {
		setImageFields(&cm, img)
	}
	if msgType == "audio" && audio != nil {
		setAudioFields(&cm, audio)
	}
	if (msgType == "video" || msgType == "gif") && video != nil {
		setVideoFields(&cm, video)
	}
	if msgType == "sticker" && sticker != nil {
		setStickerFields(&cm, sticker)
	}
	if evt.Info.IsGroup && !evt.Info.IsFromMe {
		cm.Sender = evt.Info.Sender.String()
		cm.SenderName = evt.Info.PushName
	}

	messagesMu.Lock()
	msgList := upsertMessage(messages, jid.String(), cm)
	messagesMu.Unlock()

	emit(event{Type: "messages", JID: jid.String(), Messages: resolveMentionsInList(ctx, client, msgList)})

	if !evt.Info.IsFromMe && isOpen {
		go markChatRead(ctx, client, logger, jid, []chatMessage{cm})
	}

	if msgType == "image" && img != nil {
		go downloadImage(ctx, client, logger, messages, jid.String(), cm)
	}
	if msgType == "audio" && audio != nil {
		go downloadAudio(ctx, client, logger, messages, jid.String(), cm)
	}
	if (msgType == "video" || msgType == "gif") && video != nil {
		go downloadVideo(ctx, client, logger, messages, jid.String(), cm)
	}
	if msgType == "sticker" && sticker != nil {
		go downloadSticker(ctx, client, logger, messages, jid.String(), cm)
	}
}
```

- [ ] **Step 7: Build and run the full Go test suite**

Run: `cd core && go build ./... && go test ./... -v`
Expected: build succeeds; all tests pass, including the four new `TestExtractMessage` subtests from Task 1 and the existing `TestReceiptStatusFor`/`TestApplyMessageStatus`.

- [ ] **Step 8: Commit**

```bash
git add core/main.go core/main_test.go
git commit -m "core: extract, download, and cache video/gif/sticker messages

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>"
```

---

### Task 3: Kotlin — extend `Message` model and JSON parsing

**Files:**
- Modify: `app/src/main/kotlin/com/matheusmortatti/lightwhatsapp/CoreProcess.kt:23-45,198-215`

**Interfaces:**
- Consumes: the new `chatMessage` JSON fields from Task 2 (`video_path`, `video_seconds`, `is_gif`, `sticker_path`, `sticker_is_animated`).
- Produces: `Message` gains `videoPath: String?`, `videoSeconds: Int`, `isGif: Boolean`, `stickerPath: String?`, `stickerIsAnimated: Boolean` — consumed by Task 4/5's rendering code.

- [ ] **Step 1: Extend the `Message` data class**

In `CoreProcess.kt`, replace the class (`:23-45`):

```kotlin
// "text", "image", "audio", "video", "gif", and "sticker" show up here (see
// core/main.go's extractMessage) — every other WhatsApp message type is
// dropped before it reaches the app.
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
    // Path relative to the process's working dir (context.filesDir) — null until
    // core finishes downloading it, for image messages.
    val imagePath: String?,
    // Same deal as imagePath, but for audio messages; audioSeconds is known
    // up front (from the sender's protobuf) even while the file itself is
    // still downloading.
    val audioPath: String?,
    val audioSeconds: Int,
    // Same deal again, but for video/gif messages ("gif" is WhatsApp's
    // GifPlayback-flagged video encoding — see core/main.go's
    // extractMessage). isGif mirrors type == "gif", provided directly so
    // rendering code doesn't need to re-derive it from a string.
    val videoPath: String?,
    val videoSeconds: Int,
    val isGif: Boolean,
    // Same deal again, but for sticker messages. Lottie (vector) stickers
    // never reach here — core/main.go treats them as unsupported.
    val stickerPath: String?,
    val stickerIsAnimated: Boolean,
)
```

- [ ] **Step 2: Extend `parseMessages`**

In `CoreProcess.kt`, replace the `Message(...)` constructor call inside `parseMessages` (`:202-213`):

```kotlin
            Message(
                id = o.getString("id"),
                timestamp = o.optLong("timestamp", 0L),
                fromMe = o.optBoolean("from_me", false),
                status = o.optString("status").ifBlank { null },
                senderName = o.optString("sender_name").ifBlank { null },
                type = o.optString("type", "text"),
                text = o.optString("text"),
                imagePath = o.optString("image_path").ifBlank { null },
                audioPath = o.optString("audio_path").ifBlank { null },
                audioSeconds = o.optInt("audio_seconds", 0),
                videoPath = o.optString("video_path").ifBlank { null },
                videoSeconds = o.optInt("video_seconds", 0),
                isGif = o.optBoolean("is_gif", false),
                stickerPath = o.optString("sticker_path").ifBlank { null },
                stickerIsAnimated = o.optBoolean("sticker_is_animated", false),
            )
```

- [ ] **Step 3: Build**

Run: `cd "app" && ../gradlew :app:compileDebugKotlin` (or from repo root: `./gradlew :app:compileDebugKotlin`)
Expected: BUILD SUCCESSFUL. (No behavior change yet — `MainActivity.kt` doesn't reference the new fields until Task 4/5, so this just validates the model/parsing compiles.)

- [ ] **Step 4: Commit**

```bash
git add "app/src/main/kotlin/com/matheusmortatti/lightwhatsapp/CoreProcess.kt"
git commit -m "app: parse video/gif/sticker fields from core's message events

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>"
```

---

### Task 4: Kotlin — render stickers (reuse the image path)

**Files:**
- Modify: `app/src/main/kotlin/com/matheusmortatti/lightwhatsapp/MainActivity.kt:685-725` (`MessageRow`'s `when (message.type)` block)

**Interfaces:**
- Consumes: `Message.stickerPath` (Task 3), `rememberDecodedImage(relativePath: String): State<ImageBitmap?>` (existing, `:946`).
- Produces: nothing new consumed by later tasks — self-contained.

- [ ] **Step 1: Add the `"sticker"` branch**

In `MainActivity.kt`, in `MessageRow`'s `when (message.type)` block, insert a new case right after the existing `"image"` case (after the closing `}` at `:707`, before `"audio"` at `:709`):

```kotlin
            "sticker" -> {
                val path = message.stickerPath
                if (path != null) {
                    val bitmap by rememberDecodedImage(path)
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
```

- [ ] **Step 2: Add the `STICKER_SIZE_DP` constant**

In `MainActivity.kt`, add near the other message-rendering constants (e.g. right before `RECORDING_TIMER_TICK_MS` at `:552`):

```kotlin
// Stickers are always small/square — a fixed size well under images' 200dp.
private val STICKER_SIZE_DP = 120.dp
```

- [ ] **Step 3: Build**

Run: `./gradlew :app:compileDebugKotlin`
Expected: BUILD SUCCESSFUL.

- [ ] **Step 4: Commit**

```bash
git add "app/src/main/kotlin/com/matheusmortatti/lightwhatsapp/MainActivity.kt"
git commit -m "app: render sticker messages via the existing image decode path

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>"
```

---

### Task 5: Kotlin — video/GIF thumbnail in the chat bubble

**Files:**
- Modify: `app/src/main/kotlin/com/matheusmortatti/lightwhatsapp/MainActivity.kt` (`MessageRow`'s `when` block; new composable near `rememberDecodedImage`)

**Interfaces:**
- Consumes: `Message.videoPath`/`isGif` (Task 3).
- Produces: `rememberDecodedVideoThumbnail(relativePath: String): State<ImageBitmap?>` and a tap-target that Task 6's player screen wires up (this task stubs the tap as a `TODO`-free no-op callback parameter, filled in by Task 6 — see Step 1's `onPlayVideo` parameter).

Note: this task changes `MessageRow`'s signature (adds an `onPlayVideo` callback), which ripples to its one call site in `ChatDetailScreen`. Task 6 is the one that gives that callback a real implementation (opens the player screen); this task wires the plumbing through with a callback that Task 6 will supply.

- [ ] **Step 1: Add an `onPlayVideo` parameter to `MessageRow`**

In `MainActivity.kt`, `MessageRow`'s signature (`:652-658`):

```kotlin
private fun MessageRow(
    message: Message,
    isGroup: Boolean,
    chatName: String,
    showHeader: Boolean,
    showStatus: Boolean,
    onPlayVideo: (path: String, loop: Boolean) -> Unit,
    modifier: Modifier = Modifier,
```

- [ ] **Step 2: Add the `"video"`/`"gif"` branch**

In `MessageRow`'s `when (message.type)` block, insert after the `"sticker"` case added in Task 4 (before `"audio"`):

```kotlin
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
```

- [ ] **Step 3: Add the `MediaMetadataRetriever` import**

In `MainActivity.kt`, add alongside the existing `android.media.MediaPlayer` import (`:6`):

```kotlin
import android.media.MediaMetadataRetriever
```

- [ ] **Step 4: Add `rememberDecodedVideoThumbnail`**

In `MainActivity.kt`, add directly after `rememberDecodedImage` (`:946-953`):

```kotlin
// Decodes a video/gif message's first frame off the main thread, the same
// way rememberDecodedImage does for images — core writes the file relative
// to context.filesDir (see CoreProcess.kt).
@Composable
private fun rememberDecodedVideoThumbnail(relativePath: String): State<ImageBitmap?> {
    val context = LocalContext.current
    return produceState<ImageBitmap?>(initialValue = null, relativePath) {
        value = withContext(Dispatchers.IO) {
            val retriever = MediaMetadataRetriever()
            try {
                retriever.setDataSource(File(context.filesDir, relativePath).absolutePath)
                retriever.frameAtTime?.asImageBitmap()
            } catch (e: Exception) {
                Log.w("MainActivity", "failed to decode video thumbnail $relativePath", e)
                null
            } finally {
                retriever.release()
            }
        }
    }
}
```

- [ ] **Step 5: Update `MessageRow`'s call site to pass a placeholder `onPlayVideo`**

In `MainActivity.kt`, `ChatDetailScreen`'s call to `MessageRow` (`:501-516`), add the new parameter — temporarily a no-op, replaced with the real implementation in Task 6:

```kotlin
                        MessageRow(
                            message = message,
                            isGroup = chat.isGroup,
                            chatName = chat.name,
                            showHeader = showHeader || showStatus,
                            showStatus = showStatus,
                            onPlayVideo = { _, _ -> },
                            modifier = Modifier.padding(
```

- [ ] **Step 6: Build**

Run: `./gradlew :app:compileDebugKotlin`
Expected: BUILD SUCCESSFUL.

- [ ] **Step 7: Commit**

```bash
git add "app/src/main/kotlin/com/matheusmortatti/lightwhatsapp/MainActivity.kt"
git commit -m "app: show a tap-to-play thumbnail for video/gif messages

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>"
```

---

### Task 6: Kotlin — full-screen video/GIF player

**Files:**
- Modify: `app/src/main/kotlin/com/matheusmortatti/lightwhatsapp/MainActivity.kt` (`ChatDetailScreen`; new `VideoPlayerScreen` composable near `RecordingScreen`)

**Interfaces:**
- Consumes: `onPlayVideo` callback wiring from Task 5.
- Produces: nothing consumed by later tasks — this completes the feature's UI.

- [ ] **Step 1: Add player-open state to `ChatDetailScreen` and wire `BackHandler`**

In `MainActivity.kt`, `ChatDetailScreen`, add state right after the existing `recording`-related state (`:317-320`):

```kotlin
    // Path (relative to context.filesDir) + loop flag of the video/gif
    // currently open in the full-screen player, or null if none. Not
    // rememberSaveable — re-opening from the thumbnail on process restart
    // is cheap and there's no live player state worth restoring.
    var playingVideo by remember(chat.jid) { mutableStateOf<Pair<String, Boolean>?>(null) }
```

Then update `BackHandler` (`:342-348`) to close the player first, same precedence pattern as `composing`/`recording`:

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

- [ ] **Step 2: Add the early-return branch for the player screen**

In `MainActivity.kt`, `ChatDetailScreen`, add a new early-return block right after the `recording` block (`:416-437`, before the main `Column` at `:439`):

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
```

- [ ] **Step 3: Wire the real `onPlayVideo` callback**

In `MainActivity.kt`, `ChatDetailScreen`'s `MessageRow` call site (updated in Task 5, Step 4), replace the placeholder:

```kotlin
                            onPlayVideo = { path, loop -> playingVideo = path to loop },
```

- [ ] **Step 4: Add the `AndroidView`/`VideoView` imports**

In `MainActivity.kt`, add alongside the other `androidx.compose.ui.*`/`android.*` imports:

```kotlin
import android.widget.VideoView
import androidx.compose.ui.viewinterop.AndroidView
```

- [ ] **Step 5: Add the `VideoPlayerScreen` composable**

In `MainActivity.kt`, add directly after `RecordingScreen` (`:611-649`):

```kotlin
@Composable
private fun VideoPlayerScreen(
    chatName: String,
    relativePath: String,
    loop: Boolean,
    onClose: () -> Unit,
) {
    val context = LocalContext.current
    Column(
        modifier = Modifier
            .fillMaxSize()
            .background(LightThemeTokens.colors.background),
    ) {
        LightTopBar(
            leftButton = LightBarButton.LightIcon(icon = LightIcons.CLOSE, onClick = onClose),
            center = LightTopBarCenter.Text(chatName),
        )
        Box(
            modifier = Modifier
                .weight(1f)
                .fillMaxWidth(),
            contentAlignment = Alignment.Center,
        ) {
            AndroidView(
                factory = { ctx ->
                    VideoView(ctx).apply {
                        setVideoPath(File(context.filesDir, relativePath).absolutePath)
                        setOnPreparedListener { mp ->
                            mp.isLooping = loop
                            start()
                        }
                        setOnErrorListener { _, what, extra ->
                            Log.w("MainActivity", "video playback error: what=$what extra=$extra for $relativePath")
                            true
                        }
                    }
                },
                modifier = Modifier.fillMaxSize(),
            )
        }
    }
}
```

- [ ] **Step 6: Build**

Run: `./gradlew :app:compileDebugKotlin`
Expected: BUILD SUCCESSFUL.

- [ ] **Step 7: Commit**

```bash
git add "app/src/main/kotlin/com/matheusmortatti/lightwhatsapp/MainActivity.kt"
git commit -m "app: play video/gif messages full-screen on tap

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>"
```

---

### Task 7: Build, deploy to real LP3, and verify end-to-end via self-chat

**Files:** none (verification only).

**Interfaces:** none — this task exercises everything built in Tasks 1-6 together.

- [ ] **Step 1: Cross-compile `core/` for the device and build the full app**

Follow `core/build_android.sh` (cross-compiles the Go binary into
`app/src/main/jniLibs/arm64-v8a/libwhatsmeowcore.so`), then `./gradlew
:app:assembleDebug`. If unfamiliar with the device deploy flow, use the
`lp3-tools:deploy-debug` skill (already available in this environment,
covers building/sideloading/launching/log-checking on a real LP3 over adb)
rather than re-deriving adb commands by hand.

- [ ] **Step 2: Install and launch on the real LP3, confirm connection survives**

Sideload the built APK, launch it, confirm it reconnects to the existing
linked WhatsApp session (no fresh QR scan needed — this device should
already be linked per prior sessions' work) and the chat list loads.

- [ ] **Step 3: Send a video to your own chat-with-self**

From another WhatsApp client (e.g. your phone), open your own
chat-with-self and send a short video clip. On the LP3, open that chat (or
wait for the live event if already open) and confirm:
- The message renders a decoded thumbnail (not a `[Video]` placeholder) once download completes.
- Tapping it opens the full-screen player and it plays with audio.
- Closing (back button) returns cleanly to the chat thread.

- [ ] **Step 4: Send a GIF to your own chat-with-self**

Same as Step 3, but with a GIF (or a video explicitly sent "as a GIF" from
the sending client, so it carries `GifPlayback`). Confirm:
- It renders as a thumbnail with a play icon, same as video.
- Tapping it opens the player and it loops automatically (no manual replay needed).

- [ ] **Step 5: Send a sticker to your own chat-with-self**

Send a WhatsApp sticker (most default/first-party sticker packs are static
or simple-animated WebP, not Lottie — if the first one you try renders as
`[Unsupported message: lottie sticker]`, try a different pack). Confirm it
renders as a small square image in the chat bubble.

- [ ] **Step 6: Verify the reconnect/restart path doesn't strand anything**

Force-stop and relaunch the app (or `adb shell am force-stop
<package>` then relaunch) after Steps 3-5, reopen the chat, and confirm all
three messages still render correctly (not stuck as undownloaded
placeholders) — this is the same class of bug the image pipeline hit and
fixed early on (see `PROJECT.md`'s "Image-download bug found and fixed the
same day"), worth explicitly re-checking here since three new download
paths were just added.

- [ ] **Step 7: Update `PROJECT.md`'s "Next steps" section**

Add a dated note (mirroring the existing image-support entries) recording
what was verified on real hardware and any gaps found during Steps 3-6 that
weren't worth blocking on (e.g. a specific sticker pack that turned out to
be Lottie-only, or a video mimetype that needed a fallback extension not
covered by `videoExtension`).

- [ ] **Step 8: Commit**

```bash
git add PROJECT.md
git commit -m "docs: record video/gif/sticker support verified on real LP3

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>"
```
