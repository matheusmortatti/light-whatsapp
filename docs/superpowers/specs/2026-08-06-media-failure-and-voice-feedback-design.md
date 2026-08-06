# Design: media permanent-failure state + voice-send failure feedback

Closes two of the three gaps documented in
`docs/superpowers/reviews/2026-08-06-stress-test-findings.md` ("Known gaps —
documented, not fixed"). Two independent pieces, bundled in one spec because
both are small and both close gaps from the same review pass.

## 1. Media permanent-failure state

### Problem

`downloadMedia` (`core/main.go:768`) can't tell a permanent failure (expired
media URL — 403, common for messages cached from before this device paired)
from a transient one (network blip, timeout). Nothing marks the message as
permanently failed, so `handleOpenChat` (`core/main.go:1159`) queues a retry
download for it on every chat open, forever, and the app has no way to show
"this media is gone" instead of "still downloading."

### Classification

whatsmeow exposes distinct sentinel errors for HTTP failures during media
download (`go.mau.fi/whatsmeow`'s `errors.go`):
`ErrMediaDownloadFailedWith403`, `...With404`, `...With410`, each a
`DownloadHTTPError{Response: &http.Response{StatusCode: N}}`.

`downloadMedia` classifies the error from
`client.DownloadMediaWithPathToFile` via `errors.As(err, &httpErr)` and
checks `httpErr.Response.StatusCode` against 403/404/410 → **permanent**.
Everything else (network errors, timeouts, decrypt/hash failures, unknown
errors) stays **transient** — same retry-forever-on-chat-open behavior as
today. This mirrors the failure modes actually seen on-device (expired
media links) without trying to guess at every other whatsmeow error's
retryability.

### Data model

Add one bool field per media type to `chatMessage` (`core/main.go:106`),
next to that type's existing `*Path`/`*DirectPath` fields:

```go
ImageFailed   bool `json:"image_failed,omitempty"`
AudioFailed   bool `json:"audio_failed,omitempty"`
VideoFailed   bool `json:"video_failed,omitempty"`
StickerFailed bool `json:"sticker_failed,omitempty"`
```

### Core changes (`core/main.go`)

- `downloadMedia`'s `apply` callback contract stays the same shape but gets
  a second failure path: on a classified-permanent error, instead of just
  logging and returning, it calls a new `applyFailure func(cm *chatMessage)`
  (mirroring `apply`) that sets the type's `*Failed = true` and clears
  `*DirectPath` (so `handleOpenChat`'s `m.Type == "image" && m.ImagePath ==
  "" && m.ImageDirectPath != ""` check stops matching it), then persists via
  `saveMessages` and emits `message_update` — same bookkeeping the success
  path already does, just setting a different field.
- Transient-error path (the current `logger.Warnf` + `return`) is unchanged.
- `downloadImage`/`downloadAudio`/`downloadVideo`/`downloadSticker`
  (`core/main.go:841-948`) each pass their type's `applyFailure` closure the
  same way they already pass `apply`.
- `handleOpenChat`'s undownloaded-item scan (`core/main.go:1172-1184`)
  already only requeues when `*DirectPath != ""`, so clearing it on perma-fail
  is sufficient — no separate `*Failed` check needed there.
- Remove the "Known gap" comment block at `core/main.go:794-800` (superseded
  by this implementation).

### App changes

- `CoreProcess.kt`: add `imageFailed`/`audioFailed`/`videoFailed`/
  `stickerFailed` (default `false`) to the message parsing near line 264-270,
  reading `o.optBoolean("image_failed", false)` etc.
- `MainActivity.kt` renderer (~line 921-1008): for each of the four media
  branches, when `path == null`, check the type's `*Failed` flag:
  - `true` → `"[Photo unavailable]"` / `"[Voice message unavailable]"` /
    `"[Video unavailable]"` / `"[Sticker unavailable]"` (still
    `lighten = true`, same styling as the current placeholder).
  - `false` → unchanged current placeholder text (still downloading or not
    yet attempted).
  - No tap handler on the failed state — this is a terminal, non-interactive
    label.

### Documented gap (not built now)

Add a code comment at the perma-fail branch in `downloadMedia` noting a
future manual tap-to-retry would need: a new app→core IPC command (e.g.
`retry_media`) carrying jid+messageID+type, a core handler that re-derives
`*DirectPath` isn't actually recoverable from a 403/404/410 alone (the
original message's attachment metadata would need to be re-fetched or the
message re-sent) — so tap-to-retry is a bigger redesign than a simple re-arm,
worth scoping separately if it turns out real-world 404/410s are often
transient-in-disguise (e.g. media re-sent under a new ID).

### Testing

- Unit-style check in `core/` (if a test file/pattern exists for
  `downloadMedia`-adjacent logic) or manual: feed `downloadMedia` a
  `DownloadHTTPError{StatusCode: 403}` and confirm `*Failed` gets set,
  `*DirectPath` cleared, `message_update` emitted once, and no further
  download goroutine is queued on a subsequent `handleOpenChat` for that
  message.
- On-device (real LP3, self-chat only per project convention): the existing
  self-chat has a cached pre-pairing image that already 403s per the review
  doc — reuse it to verify the placeholder flips to "[Photo unavailable]"
  and stays that way across repeated chat opens (no more retry log spam).

## 2. Voice-send failure feedback

### Problem

`RecordingScreen`'s `onSend` (`MainActivity.kt:461-473`) calls
`voiceRecorder.stop()`; when it returns `null` (the `MediaRecorder.stop()`
`RuntimeException` case, logged since commit `57b06eb`), the screen just
closes back to the composer with nothing sent and no user-facing signal.
The user can't tell "it didn't send" from "it sent and I don't see it yet."

The real failure reason lives only in the `Log.w` inside
`VoiceRecorder.stop()` (`VoiceRecorder.kt:71`) — the exception itself isn't
returned to the caller, only `null` is. This design doesn't change that;
the user-facing message is necessarily generic.

### UI approach

No toast/snackbar component exists anywhere in this app or in the LightOS
SDK surface this app depends on. The one existing precedent for a
user-facing error in this app is `LoginState.Error`'s full-screen `LightText`
(`MainActivity.kt:175`), shown during QR-login failures. This design reuses
that same pattern rather than inventing a new transient-banner widget.

### Changes (`MainActivity.kt`)

- New local state alongside the existing `recording` boolean, e.g.
  `var recordingFailed by remember { mutableStateOf(false) }`.
- In `RecordingScreen`'s `onSend` (line 461-473): when `voiceRecorder.stop()`
  returns `null`, set `recordingFailed = true` instead of doing nothing
  further; when it returns non-null, behavior is unchanged (send + close).
- New branch, checked before the `recording` branch's early return: when
  `recordingFailed` is true, render a full-screen `LightText` — "Voice
  message wasn't sent" — styled like the existing `LoginState.Error` screen,
  with a dismiss action (tap or back) that sets `recordingFailed = false`
  and returns to the normal composer.
- Remove the "Known gap" comment at `MainActivity.kt:462-466` (superseded).

### Testing

- On-device (real LP3, self-chat only): trigger a very-short recording
  (tap record, release almost immediately) — the documented repro for
  `MediaRecorder.stop()`'s `RuntimeException` — and confirm the error screen
  appears, dismisses back to the composer on tap, and a normal-length
  recording still sends successfully afterward (no regression to the happy
  path).
