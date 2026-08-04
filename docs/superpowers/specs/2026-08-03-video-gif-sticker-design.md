# Video, GIF, and sticker message support (receive-only)

## Context

`core/main.go`'s `extractMessage` currently renders two content types
(`"text"`, `"image"`) plus `"audio"`; everything else — including video,
GIFs, and stickers — falls through to `unsupportedMessageLabel` and shows
as `[Unsupported message: video]` etc. (see `PROJECT.md`'s "Groups are
already handled..." note). Image and audio both already have a full,
working pipeline: extract → persist a download reference → lazily download
on `open_chat`/live arrival → render. This spec extends that same pipeline
to three more content types.

WhatsApp has no separate "GIF" message type — a GIF is a `VideoMessage`
with `GifPlayback` set (already noted in a comment at `main.go:340`,
written when this gap was first spotted). Stickers are a distinct
`StickerMessage` type, almost always a (possibly animated) WebP image,
except AI/Lottie stickers (`IsLottie`), which are a vector format outside
what our image decoder can touch.

## Goal

View incoming video, GIF, and sticker messages in a chat thread — parity
with how images already work — verified end-to-end on real LP3 hardware by
sending each media type from another WhatsApp client (e.g. your phone) to
your own chat-with-self and confirming it renders on the LP3.

## Non-goals (this pass)

- **Sending** video, GIF, or stickers from the LP3. No send-media UI exists
  yet for *any* media type, including images (see `PROJECT.md` — image
  support has always been receive-only). Adding a picker/send path is a
  separable, materially larger feature; out of scope here.
- **Lottie/animated-vector stickers** (`StickerMessage.IsLottie`). Not a
  raster format — falls through to the existing `"unsupported"` path
  (label: "lottie sticker") rather than a half-working render.
- **Video/GIF captions beyond what `VideoMessage.Caption` already carries**
  — rendered the same way an image's caption is today, no new behavior.
- **Autoplay / inline-in-list playback.** Both video and GIF play via a
  tap-to-open full-screen player, not inline in the scrolling chat list —
  see Design section for why.
- **Streaming/partial download or a size cap.** Matches images today (no
  cap); videos can be larger, but this is out of scope for a first cut and
  noted as a known risk below.

## Design

### `core/main.go`

**`extractMessage`** (currently returns `text, msgType string, img
*waE2E.ImageMessage, audio *waE2E.AudioMessage, ok bool`) gains two more
out-params, `video *waE2E.VideoMessage` and `sticker
*waE2E.StickerMessage`, and two new cases:

- `m.GetVideoMessage() != nil` → `msgType` is `"gif"` if
  `vm.GetGifPlayback()`, else `"video"`; caption comes from
  `vm.GetCaption()`.
- `m.GetStickerMessage() != nil && !sm.GetIsLottie()` → `msgType =
  "sticker"`, no caption (stickers don't carry one). If `IsLottie` is true,
  fall through to the existing `unsupportedMessageLabel` path instead (label
  "lottie sticker" — `unsupportedMessageLabel`'s reflection-based field walk
  already produces this automatically once the sticker case only matches
  non-Lottie).

**`chatMessage` struct** gains two new field groups, following the
`Image*`/`Audio*` pattern exactly (persisted download reference, cleared
once downloaded):

```go
VideoPath          string `json:"video_path,omitempty"`
VideoSeconds       uint32 `json:"video_seconds,omitempty"`
IsGif              bool   `json:"is_gif,omitempty"`
VideoDirectPath    string `json:"video_direct_path,omitempty"`
VideoMediaKey      []byte `json:"video_media_key,omitempty"`
VideoFileSHA256    []byte `json:"video_file_sha256,omitempty"`
VideoFileEncSHA256 []byte `json:"video_file_enc_sha256,omitempty"`
VideoMimetype      string `json:"video_mimetype,omitempty"`

StickerPath          string `json:"sticker_path,omitempty"`
StickerIsAnimated    bool   `json:"sticker_is_animated,omitempty"`
StickerDirectPath    string `json:"sticker_direct_path,omitempty"`
StickerMediaKey      []byte `json:"sticker_media_key,omitempty"`
StickerFileSHA256    []byte `json:"sticker_file_sha256,omitempty"`
StickerFileEncSHA256 []byte `json:"sticker_file_enc_sha256,omitempty"`
StickerMimetype      string `json:"sticker_mimetype,omitempty"`
```

**New functions**, each a direct adaptation of `setImageFields`/
`downloadImage`:

- `setVideoFields(cm *chatMessage, v *waE2E.VideoMessage)` /
  `setStickerFields(cm *chatMessage, s *waE2E.StickerMessage)` — copy the
  download reference fields, same shape as `setImageFields`.
- `videoExtension`/`videoPath`, `stickerExtension`/`stickerPath` — same
  shape as `imageExtension`/`imagePath` (video: default `mp4`; sticker:
  default `webp`).
- `downloadVideo(ctx, client, logger, messages, jid, m)` — calls
  `client.DownloadMediaWithPath(..., whatsmeow.MediaVideo, "", false)`,
  writes to `media/<jid>/<id>.<ext>`, updates the cache, re-emits.
- `downloadSticker(ctx, client, logger, messages, jid, m)` — same shape,
  `whatsmeow.MediaImage` (confirmed via whatsmeow's `classToMediaType` map
  in `download.go`: `StickerMessage` downloads as `MediaImage`, there's no
  separate sticker media type).

**Wiring** — three call sites, matching image/audio's existing pattern
exactly, no protocol/command changes needed:

1. `extractHistoryMessage`: on `msgType == "video" || "gif"`, call
   `setVideoFields`; on `"sticker"`, call `setStickerFields`. Media stays
   undownloaded until `open_chat`, same as images.
2. `handleMessage` (live messages): same two branches.
3. `handleOpenChat`: extend the existing "what needs downloading" scan with
   two more slices (`toDownloadVideo`, `toDownloadSticker`), same
   `Path == "" && DirectPath != ""` check as images/audio, each dispatched
   via `go downloadVideo(...)` / `go downloadSticker(...)`.

### `app/`

**`CoreProcess.kt`**'s `ChatMessage` parsing gains the new fields
(`videoPath`, `videoSeconds`, `isGif`, `stickerPath`, `stickerIsAnimated`),
same `o.optString("video_path").ifBlank { null }` pattern as
`imagePath`/`audioPath`.

**`MainActivity.kt`**, in the `when (message.type)` block (`:686`):

- **`"sticker"`**: renders through the *same* branch as `"image"` (WebP —
  static or animated — decodes fine via `BitmapFactory`, which returns the
  first frame for animated WebP; full animation playback is not attempted
  in this pass), just at a smaller fixed size (~120dp vs. images' 200dp,
  since stickers are always small/square).
- **`"video"` / `"gif"`**: new branch. The chat bubble shows a static
  thumbnail — first frame decoded via
  `MediaMetadataRetriever.getFrameAtTime(path)` off the main thread, same
  `remember`/coroutine shape `rememberDecodedImage` already uses for
  images — with a small play-icon overlay. Tapping it navigates to a new
  full-screen player screen (added as a third branch alongside
  `selectedChat != null` / `else` at `MainActivity.kt:108`, structurally
  the same kind of screen-state addition as `RecordingScreen`), which plays
  the file via `VideoView` (or `MediaPlayer` + `AndroidView`-wrapped
  `SurfaceView`/`TextureView`) — built-in Android APIs, no new Gradle
  dependency, consistent with this project's existing preference for
  hand-rolled media handling over SDK deps (own `VoiceRecorder` instead of
  `sdk:client`). GIFs use the identical player, defaulting to loop-on-end;
  regular videos stop at the end. Back/tap-outside closes the player and
  returns to the chat thread.

  Full-screen tap-to-play (rather than inline autoplay in the scrolling
  list) is deliberate: a `Surface`/`MediaPlayer` embedded directly in a
  `LazyColumn` item risks lifecycle bugs when that item scrolls off-screen
  mid-playback, especially given the list is already
  `LightLazyScrollView`-backed with its own recycling behavior. A
  dedicated player screen sidesteps that entirely.

## Known risks (carried, not solved, this pass)

- **Video file size**: WhatsApp videos can be tens of MB; the existing
  lazy-download-on-`open_chat` behavior (same as images) avoids downloading
  a whole history's worth of video eagerly, but a chat with several videos
  could still mean a slow/expensive `open_chat`. No cap or progress UI is
  added in this pass — matches images' existing behavior, revisit if it
  proves painful in practice.
- **Animated WebP stickers**: shown as a static first frame. Full animation
  would need `ImageDecoder` (API 28+, available on LP3's API 34 target)
  instead of `BitmapFactory`; deferred since it's a polish item, not
  required for stickers to be visibly usable.

## Testing

- Go: extend `main_test.go`'s existing `extractMessage`/label-fallback
  coverage with cases for a plain `VideoMessage`, a `GifPlayback` one, a
  non-Lottie `StickerMessage`, and a Lottie one (falls through to
  `"unsupported"`).
- Manual end-to-end on real LP3 hardware (`chat with myself`): from another
  WhatsApp client (phone), send a short video, a GIF, and a sticker to your
  own account. Confirm all three arrive in the LP3's self-chat, render a
  thumbnail/sticker image, and — for video/GIF — play back correctly from
  the full-screen player. Also confirm history-sync/reconnect doesn't
  strand any of them as an undownloaded placeholder (same check the image
  pipeline already passed).
