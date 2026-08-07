# On-device stress test findings — 2026-08-06

Full-app pass on real LP3 hardware (serial `LP3LHMA551300893`), driven via
`adb`/`uiautomator` rather than by hand. Goal: find unhandled errors and
regressions, and add logging anywhere it was missing so future issues are
diagnosable from `adb logcat` alone. Self-chat only for any sends, per the
device-testing convention (see `PROJECT.md`/session notes); everything else
was read-only browsing of real chats.

**Scope covered.** Chat list, search, self-chat text send/receive round-trip,
voice-message record/send/playback, standard-6 reaction send, group chat
render/scroll, image message rendering, back-navigation/exit, backgrounding.
Not covered this pass: sending video/GIF/stickers (no send UI exists),
QR login/pairing flow (already paired), group chat sends (self-chat-only
convention).

## Bugs found and fixed

### 1. Stale "[Unsupported message: protocol/reaction]" placeholders never clear

`core/main.go`'s `extractMessage` used to fall through to a generic
reflection-based `"unsupported"` label for protocol messages and reactions,
before each got dedicated handling (`7627824` dropped protocol messages
outright; the 2026-08-05 reactions feature routes reactions to
`handleReaction` instead). Both fixes only stopped *new* messages of that
kind from being mislabeled — neither cleaned up what earlier builds had
already written to `messages/<jid>.json`. Any chat that received one of
these before the relevant fix landed kept showing the stale placeholder
text forever, since the cached message is loaded and rendered as-is on
every chat open.

Found by opening a real self-chat and a real group chat and seeing the
stale text still rendered, despite both fixes being live in `main`.

**Fix.** `loadCachedMessages` (`core/main.go:343`) now filters out any
cached message with `Type == "unsupported"` and `Text` in
`staleUnsupportedLabels` (`"protocol"`, `"reaction"`) as it loads. Verified
on-device: both chats clean after rebuild + reinstall.

Commit: `4b6565c`.

### 2. No exception boundary around the core-event pipeline

`QrLoginViewModel.kt`'s `init` block collected `coreProcess.events()` with a
bare `when(event)` and no try/catch, and `viewModelScope` had no
`CoroutineExceptionHandler` installed — so any exception thrown by *any*
event-handling branch (or by `encodeQr`) was an uncaught exception that
killed the whole app, surfacing only as a generic `AndroidRuntime` crash
dump with no indication of which event or handler was responsible.

This was compounded by `CoreProcess.kt`'s `events()` calling
`ProcessBuilder(...).start()` completely unguarded — a failure to exec the
native binary (missing lib, bad `jniLibs` packaging, exec-format error)
would throw an `IOException` straight into that same unguarded collector.

Not reproduced on-device (no input was found that actually throws), but a
real latent single point of failure: the entire app's liveness rested on
every branch of one `when` and one subprocess-start call never throwing.
Found via code audit prompted by "add logging where it's lacking," not by
a live crash.

**Fix.** The event-handling `when()` is now wrapped in try/catch that logs
via `Log.e(TAG, "failed to handle core event: $event", e)` and drops the
bad event instead of crashing. `ProcessBuilder.start()` failures are now
caught, logged, surfaced as a `CoreEvent.Error` (so the existing
`LoginState.Error` UI path picks it up), and close the flow gracefully.

Commit: `cb820af`.

### 3. Missing predictive-back manifest opt-in

Logcat printed `W/WindowOnBackDispatcher: OnBackInvokedCallback is not
enabled for the application` on every back-press during normal navigation
testing. The app targets SDK 36 but hadn't set
`android:enableOnBackInvokedCallback="true"`. Compose's `BackHandler`
supports both the legacy and predictive-back dispatch paths, so this was
log noise rather than a functional bug — but it's the documented fix and
free to apply.

**Fix.** Added the manifest attribute. Re-verified the critical back-exit
crash from the 2026-07-30 stress test session still doesn't regress after
this change (triple back-press, process stays alive, backgrounds cleanly).

Commit: `ceff4da`.

### 4. Silent `VoiceRecorder.stop()` failure

The catch block on `MediaRecorder.stop()` (a `RuntimeException`, common for
very short recordings) discarded the recording file and returned `null`
with zero logging. The caller (`MainActivity.kt`'s `RecordingScreen.onSend`)
does nothing but close the recording screen either way, so a failed
send-after-record was indistinguishable from a normal cancel in the logs.

**Fix.** Added `Log.w` in the catch block, and a comment at the call site
cross-referencing the residual gap below.

Commit: `57b06eb`.

## Known gaps — documented, not fixed

Both gaps originally identified in this review (see commit `4b6565c` for
the first, `57b06eb` for the second) were product/UX decisions, not
correctness bugs, and out of scope for this pass at the time. Both have
since been closed — see below. No known gaps remain open from this
review.

### Closed since this review

- **Media downloads retry forever with no permanent-failure state** — was
  the first bullet above (`downloadMedia` in `core/main.go` had no way to
  distinguish a permanently failed download, e.g. a 403 on media cached
  from before this device paired, from a transient one, so
  `handleOpenChat` retried it on every chat open forever). Fixed by the
  `2026-08-06-media-failure-and-voice-feedback` plan: core now classifies
  HTTP 403/404/410 as permanent (`ceedc4b`, `a2c5306`), persists a
  per-type `*_failed` flag and stops re-queuing the download
  (`b677d1b`, `a2c5306`), and the app renders "[Photo unavailable]" /
  "[Voice message unavailable]" / "[Video unavailable]" / "[GIF
  unavailable]" / "[Sticker unavailable]" instead of retrying forever
  (`f7332f6`). Verified live on real LP3 (serial `LP3LHMA551300893`):
  self-chat's pre-pairing image that used to 403 forever now shows
  "[Photo unavailable]", persisted to the on-disk message cache
  (`image_failed: true`, direct path cleared), and `adb logcat` showed the
  403 `Warnf` exactly once, with `open_chat` reporting "0 images to
  download" for that chat across 3 subsequent open/close cycles — no
  retry spam. Other media in the same chat (4 images, 5 audio, 5 gifs, 4
  stickers, 1 video) all still downloaded and rendered normally,
  confirming no regression.

- **A failed voice-recording send gave no user-facing feedback** — was the
  second bullet above (`RecordingScreen.onSend` in `MainActivity.kt` just
  closed the recording screen silently when `voiceRecorder.stop()`
  returned `null`, indistinguishable from a normal cancel without checking
  logcat). Fixed by the `2026-08-06-media-failure-and-voice-feedback`
  plan: a new dismissible `RecordingFailedScreen` now shows "Voice message
  wasn't sent" whenever `stop()` rejects the recording, dismissible via
  either the top-bar CLOSE icon or hardware/predictive back
  (`1d74e09`, `ed56276`). Verified live on real LP3 (serial
  `LP3LHMA551300893`), driven via `adb`/`uiautomator`: a very-short
  recording (mic-tap immediately followed by SEND-tap) reliably reproduced
  `MediaRecorder.stop()`'s `RuntimeException: stop failed.`, `adb logcat`
  showed `VoiceRecorder`'s `Log.w` firing each time, and the "Voice
  message wasn't sent" screen appeared instead of a silent return to the
  composer — confirmed on two separate reproductions, dismissed cleanly
  back to the composer via both the CLOSE icon and the hardware back key
  (chat stayed open, no exit, no crash). No regression to the happy path:
  a normal ~3-second recording sent successfully (ack + delivery receipts
  observed in `adb logcat`) and played back normally in the chat.

## Also audited, found clean

`core/main.go` has roughly a dozen `if err != nil { return/continue }`
branches with no logger call, mostly in functions with no `logger` param
threaded through (`loadCachedChats`, `sanitizeChats`, `contactName`, and
similar). Checked each: all are legitimate best-effort silence (first-run
cache misses, best-effort JID-parse skips on malformed/junk data) rather
than bugs — threading a logger through them for these cases would mostly
add noise, not signal.
