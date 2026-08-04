# light-whatsapp

A WhatsApp-only messaging client for the Light Phone III, built on Light's
[`light-sdk`](https://github.com/lightphone/light-sdk) (this repo is a fork
of it — see "Repo layout" below) and [whatsmeow](https://github.com/tulir/whatsmeow)
for the WhatsApp protocol. Open source, sideload-distributed, not intended
for Light's official tool store.

This file is the orientation doc for whoever (human or agent) picks this up
next. Read it before writing code.

## Intent

Personal, minimal WhatsApp client for the Light Phone III. No other
networks, no Beeper dependency, no bridge service — talks to WhatsApp
directly via its multi-device (WhatsApp Web-style) protocol.

## Decision log

**Why not build on Beeper at all (as a bridge-aggregator for WhatsApp +
Telegram + others)?**
Investigated first. Beeper is a Matrix homeserver (`matrix.beeper.com`) with
server-side bridges — architecturally attractive since a client would only
need to speak Matrix, not each network's protocol. Killed this path after
reading Beeper's own `bridge-manager` CLI source
(`api/beeperapi/login.go`): the only way to obtain a Matrix token for a
Beeper account is Beeper's private login API, and the auth header literally
is the string `"BEEPER-PRIVATE-API-PLEASE-DONT-USE"`. That's the vendor
explicitly telling third parties not to use it, in their own open-source
code — a different and clearer kind of "no" than generic ToS risk or SDK
immaturity. Treated as a hard no-go rather than an engineering obstacle.

**Why WhatsApp-only via whatsmeow instead of Beeper's bridges?**
whatsmeow talks to WhatsApp directly, using WhatsApp's own sanctioned
multi-device linking mechanism (QR-scan, same as WhatsApp Web/Desktop) —
no vendor is telling us not to do this the way Beeper did. It's also the
same library Beeper's own WhatsApp bridge runs server-side, and
[Photon](https://github.com/joshkarlin/photon) already proved the whole
"whatsmeow subprocess + Kotlin light-sdk UI" architecture works on real
Light Phone III hardware.

**Why sideload-only, not targeting Light's official tool tiers?**
A real-time chat client needs a persistent connection. Light's official SDK
tool model does not support this today: `LightWork` background jobs have a
15-minute minimum interval, and UnifiedPush (the only push mechanism)
requires a remote server to originate the push — neither can hold a live
WhatsApp session open. A Light SDK team member confirmed (light-sdk
[issue #128](https://github.com/lightphone/light-sdk/issues/128)) there are
currently no plans for persistent background services in the
Light-approved/SDK-built tiers. This forces the "unrestricted sideload /
Developer Mode" distribution tier, which allows genuine Android foreground
services. Not a blocker for this project (open-source, personal use, not
distributing through Light's store), but it's a deliberate scope choice,
not an oversight.

**Why `app/` instead of building this inside `tool/`?** Originally assumed
`tool/` could host the foreground service supervising `core/`'s subprocess.
Turns out the `light-sdk` Gradle plugin (`plugin/.../LightSdkPlugin.kt`,
`LightToolMetadata.kt`, `ManifestGenerator.kt`) makes this structurally
impossible for any module that applies it: the generated manifest only ever
emits `LightSdkApplication`/`LightActivity`/`LightSdkReceiver` (no `<service>`
slot, and a hand-written manifest is explicitly rejected by
`validateNoUserManifest`); the permission allowlist
(`LightToolPolicy.ALLOWED_PERMISSIONS`) has no `FOREGROUND_SERVICE`; and the
plugin's source scanner blocks `android.app.Service`, `startService(`,
`bindService(`, `getSystemService(` in any `tool/src` file. None of that
machinery applies to a module that doesn't apply the plugin (`sdk/ui` proves
this — no plugin, no restrictions). So `app/` is a plain Android app module
(no `light-sdk` plugin, own manifest, free dependency choices), reusing
`sdk/ui`'s Compose components for visual parity but not built through the
plugin — sideloaded directly instead. `tool/` is left alone as the
upstream-tracking scaffold it already was.

**Discoverability gotcha found afterward:** LightOS doesn't find Tools via
the standard Android LAUNCHER intent-filter — it scans installed packages
for a specific no-op `BroadcastReceiver` marker
(`ACTION_SDK_MARKER`/`SDK_VERSION` metadata, see
`sdk/server/.../LightSdkServer.kt:77-106`), which the light-sdk plugin
normally injects (`LightSdkReceiver`) and which a plain sideloaded APK
therefore lacks — true at every `ClientFilterLevel` including the
permissive "Any tools" tier, since that only relaxes the check *after* the
marker query already ran. Fix: `app/` hand-declares that same marker
(`SdkMarkerReceiver.kt` + the matching `<receiver>` in
`AndroidManifest.xml`) — it's an inert manifest-only contract, doesn't
require adopting the rest of the plugin's restrictions. Without it, the app
still installs and runs fine, just isn't listed in LightOS's own tool
switcher (`adb shell am start` still works as a fallback). Unverified
against real LightOS at time of writing — only checked against the shared
`sdk/server` discovery code and the emulator, not real hardware.

## Architecture

```
app/ (Kotlin, Compose, plain Android app)  <-- stdout JSON events -->  core/ (Go, whatsmeow)
     Activity-scoped subprocess supervision                              WhatsApp multi-device
     (persistent foreground service is chat-phase work)                  protocol + E2EE
     reuses sdk/ui for visual parity, not a light-sdk Tool               SQLite session store
```

- **`core/`**: Go module, whatsmeow-based, cross-compiled `CGO_ENABLED=0` for
  `android/arm64` via `core/build_android.sh`, run as a subprocess. See
  `core/README.md`.
- **`app/`**: standalone Android app module (deliberately *not* a
  `light-sdk` Tool — see decision log above). Launches `core/`'s binary
  (bundled as `app/src/main/jniLibs/arm64-v8a/libwhatsmeowcore.so` so
  Android extracts it to disk as an executable "native lib"), reads its
  stdout JSON-event protocol, and renders a QR-login screen using `sdk/ui`
  Compose components. Once connected, shows a chat list (name, unread
  count) fed by a `"chats"` stdout event; tapping a chat opens a thread view
  (text + images, sender names in groups) fed by a `"messages"` event — no
  sending yet, and subprocess lifetime is still scoped to the Activity, no
  persistent foreground service.
  - **Chat viewing (2026-07-29)**: the stdout-only protocol became
    bidirectional to support this — `app/` writes `{"type":"open_chat",
    "jid":...}` lines to `core/`'s stdin (`readCommands` in `core/main.go`),
    which replies with a `"messages"` event for that chat. Text messages
    (`Conversation`/`ExtendedTextMessage`, unwrapping
    `Ephemeral`/`ViewOnce`/`ViewOnceV2` wrappers first) and image messages
    are extracted from both history-sync conversations and live
    `*events.Message`; everything else (video, audio, documents, stickers,
    polls, location, reactions, ...) is dropped during extraction, per the
    app's scope. Images download lazily — on `open_chat`, or immediately for
    a live incoming message — rather than eagerly for a whole history sync,
    which can carry years of backlog. Verified end-to-end on the LightOS
    emulator (2026-07-29): a fresh QR re-link's history sync produced real
    per-chat message backlog (e.g. 41 messages/9 images in one group),
    rendered correctly with sender names and chronological order; a live
    incoming photo downloaded and rendered full-res, right-aligned as
    from-me.
  - **Image-download bug found and fixed the same day**: the first cut kept
    each undownloaded image's decryption info (direct path, media key,
    hashes) in an in-memory-only map, separate from the persisted message
    cache. Any process restart between seeing an image (history sync or a
    live message) and the user opening that chat — normal, given the
    Activity-scoped subprocess lifetime — silently and permanently
    stranded it as a `[Photo]` placeholder, since the in-memory info was
    gone but the message itself looked like it was just "not downloaded
    yet". Fixed by persisting the download reference on the `chatMessage`
    itself (cleared once the image actually downloads), so a later process
    can retry it.
  - **Known gap, not yet fixed**: WhatsApp's one-time history-sync only
    includes full message backlog for a subset of chats (recently-active
    ones, in this account's case 3 of 274) — others show no messages until
    a live message arrives in them. Same underlying cause as the chat-list
    caveat below (history-sync is one-time, not on-demand).
  - **Chat list caveat**: WhatsApp's multi-device protocol only pushes the
    full conversation history-sync once, right after a device is linked
    (`events.HistorySync` in whatsmeow). `core/` accumulates conversations
    from that sync into `chats.json` (in the same working dir as
    `whatsapp.db`) and re-emits it on every subsequent launch so a
    reconnect still has something to show immediately — but a session that
    was linked *before* this feature existed (or any session already past
    its initial sync) will never receive another history-sync, so the chat
    list stays empty until the device is unlinked and re-linked via a fresh
    QR scan. Confirmed on the LightOS emulator (2026-07-29): reconnect path
    correctly shows the "Downloading chats..." empty state, no crash.
    Full data path verified end-to-end the same day with a real fresh QR
    scan: 274 chats (161 direct, 113 groups) downloaded and rendered. That
    fresh sync exposed two real bugs, both fixed: (1)
    `Conversation.GetConversationTimestamp()`/`GetLastMsgTimestamp()` came
    back 0 for every entry in the `RECENT` sync chunk — fixed by falling
    back to the max per-message timestamp in that conversation's bundled
    `Messages`. (2) `Conversation.Name` is only ever populated for 1:1
    chats, never groups — history sync alone can't produce a group's
    subject. Fixed with one `GetJoinedGroups` bulk IQ call (a single
    request, not one per group) on every `Connected` event, backfilling
    names into already-accumulated chats and re-emitting; resolved
    108/113 group names in testing (the rest are likely groups the
    account has since left, which `GetJoinedGroups` correctly excludes).
  - **Ordering fix (2026-07-29, same day)**: the timestamp fix above turned
    out to be necessary but not sufficient — `handleHistorySync` was
    unconditionally overwriting each JID's `chatSummary` on every chunk it
    saw, so a later chunk with an unset timestamp/name silently threw away
    good data an earlier chunk had already supplied for that same JID. Sync
    chunks for the same JID are now merged (max timestamp, prefer a
    non-empty name/explicit unread count) instead of clobbered. Also added
    a `*events.Message` handler so any new message — sent or received, from
    any linked device — bumps its chat to the top going forward, matching
    WhatsApp's own behavior once the one-time history sync is no longer
    available to lean on. Reverified with a second fresh QR link: all 274
    chats got real (nonzero) timestamps and the list came out correctly
    sorted newest-first.
  - **Known minor gap, not yet fixed**: a handful of 1:1 chat names still
    show as raw phone numbers instead of the contact's name. Likely cause:
    whatsmeow's `PUSH_NAME` chunk processing is async
    (`go doStorage(...)` in `message.go`'s `DownloadHistorySync`) and isn't
    guaranteed to finish writing to `Store.Contacts` before the `RECENT`
    chunk arrives and does its contact lookup — a race, not something a
    resync fixes deterministically. Low priority, not blocking.
- **`tool/`**: still Light's unmodified sample scaffold
  (`HomeScreen`/`DetailScreen`/`ToolEntryPoint`), kept only for tracking
  upstream `light-sdk` changes. Not used by this project's actual app.
- IPC is newline-delimited JSON, both directions, over the subprocess's
  stdin/stdout: `core/main.go` emits events on stdout (`{"type":"qr",...}` /
  `{"type":"connected",...}` / `{"type":"chats",...}` /
  `{"type":"messages",...}`), `app/` sends commands on stdin
  (`{"type":"open_chat","jid":...}`), human-readable logs go to stderr on
  both sides of that split. Still a plain pipe, not a socket — fine for
  one Activity-scoped subprocess with a single client; would need
  revisiting (Photon-style local socket) if `core/` ever has more than one
  consumer at a time.

## Repo layout / remotes

This repo is a fork of `lightphone/light-sdk`, forked because their README
recommends tracking upstream closely ("things are going to change fast").

- `origin` → `https://github.com/matheusmortatti/light-whatsapp.git` (your fork, renamed from `light-sdk`)
- `upstream` → `https://github.com/lightphone/light-sdk.git` (Light's repo)

Pull upstream periodically: `git fetch upstream && git merge upstream/main`
(or rebase, your call). Everything under `sdk/`, `plugin/`, `examples/`,
`docs/`, `builder/`, `lint-rules/` is upstream light-sdk scaffolding/tooling
— leave it alone except to take upstream updates. `core/` and this file are
new, ours, not part of upstream.

## Dependencies / setup

- **Go 1.25+** for `core/`.
- **Android Studio** (or IntelliJ) for `tool/` — standard light-sdk Gradle
  project.
- **GitHub Packages read token**: light-sdk's Gradle build pulls a private
  package from `lightphone/light-keyboard` via GitHub Packages. Needs either
  env vars `GH_PACKAGES_USER` / `GH_PACKAGES_TOKEN`, or `gpr.user` /
  `gpr.key` in a (gitignored) `local.properties`. Get a token with package
  read access to the `lightphone` org.
- **LightOS emulator**: Android emulator, API 34, 1080×1240, no Google Play
  Services — see `docs/system_app` (from upstream) for the "system app"
  setup needed to test push/permissions. Or real Light Phone III hardware
  via ADB sideload. Two gotchas hit setting this up (2026-07-29) that the
  upstream doc doesn't call out, noted here since `docs/` is upstream and we
  don't edit it directly:
  - The `google_apis` system-image tag is signed `dev-keys`, not
    `test-keys` — the platform-signing step needs `default` (pure AOSP,
    no Google APIs) instead, e.g.
    `system-images;android-34;default;arm64-v8a`. Check with
    `adb shell getprop ro.build.description`.
  - The emulator's default "Allowed Tools" setting is "Community Tools",
    which filters out unsigned sideloaded apps regardless of the
    `ACTION_SDK_MARKER` receiver being present. In the emulator app:
    swipe/dpad to Settings → Allowed Tools → "All Tools" to see `app/`
    listed and launchable from LightOS's own tool switcher.
- **whatsmeow** (MPL-2.0) + **modernc.org/sqlite** (pure-Go, no CGO) —
  already wired into `core/go.mod`.

## Open risks (carry these into every design decision)

1. **WhatsApp ban risk** — unofficial whatsmeow-based clients get caught in
   periodic Meta ban waves. Ongoing operational risk, not a one-time fix.
2. **No push gateway** — real-time delivery needs the connection held open
   via foreground service; battery/Doze handling is the main engineering
   grind, not the WhatsApp protocol itself.
3. **Primary-phone dependency** — some sync edge cases still need the
   primary WhatsApp phone to have been online; this is a companion device,
   not a full replacement.
4. **Linked-device cap** (~4 historically) — uses one of a limited pool of
   slots on the account.
5. Both `light-sdk` and LightOS's community-tool support are pre-release
   and changing fast (per upstream README, as of July 2026). Expect
   breaking changes on `git pull upstream`.

## Next steps, in order

1. ~~Get the light-sdk emulator running locally, confirm the unmodified
   sample `tool/` builds and launches~~ — done, see `docs/system_app`.
2. ~~In `core/`, prototype whatsmeow QR-login + send/receive standalone on
   desktop Go, against a real WhatsApp account~~ — done, `core/main.go`.
3. ~~Cross-compile `core/` for `android/arm64`~~ — done,
   `core/build_android.sh`; confirmed on-device (2026-07-29, LightOS
   emulator) that the binary actually executes as a subprocess.
4. ~~Design the IPC contract~~ — done for this phase: stdout JSON-lines
   protocol, see Architecture above. (Bidirectional/socket-based IPC still
   TBD for the chat phase.)
5. ~~`app/` scaffolded... needs on-device verification~~ — done
   (2026-07-29): built, installed on the LightOS emulator, **discovered and
   launched via LightOS's own tool switcher** (not just `adb shell am
   start` — the `ACTION_SDK_MARKER` receiver works), subprocess launched
   and connected to WhatsApp's servers, real QR code rendered on-screen.
   Along the way, found and fixed a real bug: Android has no usable
   `/etc/resolv.conf`, so Go's pure-Go DNS resolver (required — no cgo/NDK)
   failed every lookup; fixed by pointing `net.DefaultResolver` at a public
   DNS server directly (see `core/main.go`'s `init()`). This would have hit
   real LP3 hardware too, not just the emulator. Still unverified: actually
   scanning the QR with a phone and confirming "connected as `<JID>`", and
   confirming reconnect-without-QR on relaunch — needs a human to scan.
6. Persistent connection: replace the Activity-scoped subprocess lifetime
   with a real foreground service (notification channel, Doze/battery
   handling) once real-time message delivery is needed.
7. ~~Chat UI: repurpose/extend `app/`'s screens into a chat-list/thread
   UI~~ — done and verified end-to-end (2026-07-29). Chat list: `core/`
   emits a `"chats"` event built from history-sync conversations, `app/`
   renders it once connected. Thread view: tapping a chat sends
   `open_chat`, `core/` replies with a `"messages"` event (text + images,
   see Architecture above); real message backlog and a live incoming photo
   both confirmed rendering correctly.
8. ~~Sending messages: requires extending the stdout protocol to be
   bidirectional~~ — the bidirectional half is done (`open_chat` over
   stdin, see Architecture above); *sending* messages themselves (a new
   `send_message` command, `core/` calling whatsmeow's send APIs) is still
   open.
9. ~~Groups are already handled in chat viewing (sender names, `is_group`);
   remaining polish: the known per-chat-backlog gap above, and images
   currently being the only non-text media type (video/audio/documents
   still dropped during extraction).~~
10. Video, GIF, and sticker messages (receive-only, same scope as images —
    see `docs/superpowers/specs/2026-08-03-video-gif-sticker-design.md`)
    — done and verified end-to-end on real LP3 hardware (2026-08-04,
    `LP3LHMA551300893`, self-chat). `core/` extracts video/GIF (a
    `VideoMessage` with `GifPlayback` set — WhatsApp has no separate GIF
    type) and sticker messages the same way it already did images/audio;
    downloads lazily on `open_chat`/live arrival. `app/` shows a
    first-frame thumbnail (via `MediaMetadataRetriever`) with a
    tap-to-play full-screen player (`VideoView`, no new dependency) for
    video/GIF, and renders stickers through the existing image-decode path
    (animated WebP shows its first frame only, not animated — accepted
    limitation, not a bug). Confirmed on-device: a video message already
    present in the chat when the app (running this build) was opened
    rendered a thumbnail and played correctly — this shows the extraction/
    thumbnail/playback path works, but doesn't by itself prove the
    in-place-upgrade path specifically (see the cache-upgrade gap noted
    below; it's unconfirmed whether that message had been cached by an
    older build or was freshly extracted by this one). A live GIF sent from
    another WhatsApp client rendered and looped correctly; a live sticker
    rendered correctly; all three survived a force-stop/relaunch without
    getting stranded as an undownloaded placeholder (the same class of bug
    the image pipeline hit early on — re-checked deliberately here).
    Documents — and everything else (polls, locations, contacts, Lottie
    stickers) — remain unsupported at extraction; audio is already fully
    supported (has been since an earlier feature: `extractMessage` has an
    `"audio"` case, `downloadAudio` exists, `AudioMessageRow` renders it).
    Sending any media type (video/GIF/sticker/image alike) remains out of
    scope for this pass — the app has always been receive-only for media.
    Known gap: a video/GIF/sticker message that was already cached (e.g.
    from a history sync under an older build without this feature) stays
    `[Unsupported message: ...]` permanently — `loadCachedMessages` in
    `core/main.go` never re-runs `extractMessage` on already-cached rows in
    `messages/<jid>.json`, so upgrading to this build does not retroactively
    re-extract old entries. Only newly-arriving messages, or chats not yet
    in the cache, get the new rendering. A full fix would need a
    cache-format version marker that forces a re-sync; not implemented in
    this pass, given WhatsApp's history sync is itself one-time (see the
    "Chat list caveat" note earlier in this doc).
