# Security & performance audit — 2026-08-04

Whole-codebase read of the project's own code (`core/`, `app/`) against a
messaging-app threat model. Nothing in this pass was changed; each finding is
written to be actionable on its own so they can be picked off one at a time.

**Scope.** `core/main.go`, `app/src/main/kotlin/**`, `app/AndroidManifest.xml`,
`app/build.gradle.kts`, `core/build_android.sh`, `.gitignore`. Upstream
light-sdk scaffolding (`sdk/`, `plugin/`, `examples/`, `builder/`,
`lint-rules/`) was **not** audited — it tracks upstream and we don't edit it —
except where `app/` depends on it (`sdk/keys/`, `sdk/ui`). Third-party
dependency CVEs (whatsmeow, modernc.org/sqlite, zxing) are out of scope; they
are tracked by version bumps, not by this document.

**Threat model assumed.** The realistic adversaries, in rough order of
likelihood:

1. **A remote WhatsApp peer** — anyone who can send this account a message,
   including a stranger in a group. Controls message content, message IDs,
   push names, media metadata. This is the only fully-remote attacker.
2. **Someone with physical/ADB access** to an unlocked Light Phone III. Real
   here because the LP3 runs this app in developer-mode sideload by design
   (see `PROJECT.md` decision log).
3. **A malicious app on the same device.** Weak — Android's uid sandbox does
   most of the work, and this app exports nothing but the launcher activity
   and an inert marker receiver.
4. **An on-path network observer.** WhatsApp traffic is Noise/E2EE, so this
   is a metadata concern, not a content one.

**Not** in the model: the WhatsApp servers themselves (trusting them is
inherent to being a WhatsApp client), and Meta's ban risk (an operational
risk, already documented in `PROJECT.md`).

---

## Index

| # | Area | Severity | Summary |
|---|------|----------|---------|
| S1 | Signing | **High** | Release builds signed with the in-repo dev keystore (password in the build file) |
| S2 | Data at rest | **Medium-High** | Message history, session keys, and pending media keys stored unencrypted |
| S3 | Filesystem | **Medium** | Path traversal via sender-controlled WhatsApp message ID |
| S4 | Spoofing | **Medium** | Live group messages trust unauthenticated `PushName` for the sender label |
| S5 | Privacy | **Medium** | All DNS hard-forced to plaintext `8.8.8.8`, ignoring system/VPN settings |
| S6 | Disclosure | **Medium** | Message cache, media, and recordings are not gitignored |
| S7 | Spoofing | Low | Mention rewriting lets a sender render an arbitrary contact's real name |
| S8 | IPC | Low | `core` trusts the stdin command channel completely (unvalidated JIDs/paths) |
| S9 | Integrity | Low | Non-atomic cache writes silently discard a whole chat's history on truncation |
| S10 | Disclosure | Low | Raw internal error strings rendered in the UI |
| P1 | Rendering | **High** | Full-resolution bitmap decode for every image/sticker, no downsampling, no cache |
| P2 | I/O | **High** | Whole chat list re-serialized and written to disk on every single message |
| P3 | IPC | **High** | Every media download and status receipt re-emits the chat's entire message list |
| P4 | DB | Medium-High | Mention resolution hits a 1-connection SQLite pool on every emit |
| P5 | Memory | Medium | Unbounded concurrent media downloads, each fully buffered in RAM |
| P6 | Media | Medium | A `MediaPlayer` prepared per visible audio row; thumbnails re-extracted every scroll |
| P7 | Memory | Medium | In-memory message map never evicted; full re-sort + rewrite per message |
| P8 | UI | Medium | QR bitmap built with 262,144 individual `setPixel` calls on the main thread |
| P9 | IPC | Medium | `trySend` silently drops core events when the flow buffer fills |
| P10 | UI | Low-Med | Scroll-chase effect reacts to every scroll frame and re-issues `scrollToItem` |
| P11 | DB | Low | SQLite opened without WAL and capped at one connection |
| P12 | Startup | Low | Every cached chat file re-read and re-parsed on each process start |

---

# Security

## S1 — Release builds are signed with the repo's public dev keystore

**Severity: High.** `app/build.gradle.kts:17-26`, `:38`, `:44`.

```kotlin
signingConfigs {
    create("lightsdkDev") {
        storeFile = file("../sdk/keys/lightsdk-dev.jks")
        storePassword = "android"
        keyAlias = "lightsdk-dev"
        keyPassword = "android"
```

`sdk/keys/lightsdk-dev.jks` is tracked in git (`git ls-files sdk/keys/`
confirms it), the store and key passwords are literals in the build file, and
**both** `debug` and `release` build types use it.

This is upstream light-sdk's shared sample key — it is in every fork of the
SDK, and it is the signing identity of a real WhatsApp client holding a live
account session. Android's package manager allows an in-place upgrade of an
installed app by any APK signed with the same key, and an upgrade inherits the
app's private data directory. So anyone who can get one APK installed on the
device (sideload, "update" prompt, ADB) can silently replace this app with
their own build and read `whatsapp.db` (Signal session and identity keys),
`chats.json`, `messages/*.json`, and the entire media cache. The key
additionally can't be rotated later without uninstall/reinstall, which means
re-linking the WhatsApp account.

**Fix.** Generate a project-specific release keystore, keep it out of the repo,
and feed it via `local.properties` / env vars (the repo already has this
pattern for `gpr.user`/`gpr.key` in `settings.gradle.kts`). Leave the shared
dev key on `debug` only, and make `release` fail loudly if the real keystore
isn't configured rather than silently falling back.

---

## S2 — Message history, session keys, and pending media keys are stored unencrypted

**Severity: Medium-High.** `core/main.go:249`, `:286-293`, `:116-151`,
`:1731`.

Three separate things sit in cleartext in the app's private files dir:

- `whatsapp.db` — whatsmeow's store: the device's Signal identity key, session
  state, and prekeys. Compromise means the account's linked device can be
  impersonated.
- `messages/<jid>.json` — full plaintext message bodies, sender JIDs, and
  timestamps, up to 500 per chat (`maxMessagesPerChat`, `:268`).
- `chats.json` — every contact/group name and last-activity time. A complete
  social graph.

Plus, for media that has been *announced but not yet downloaded*, the
`*MediaKey` / `*FileEncSHA256` fields (`:116-151`) are persisted alongside the
message — i.e. live decryption keys for content on WhatsApp's CDN.

There is real mitigation already in place and it should be credited: files are
`0600` in `0700` dirs, `allowBackup="false"` is set
(`AndroidManifest.xml:9`), and the media keys are **cleared** once the media
actually downloads (`:648-652` and its audio/video/sticker counterparts) —
so the key exposure window is bounded, not permanent.

What's missing is encryption at rest. On a device this app is *designed* to be
sideloaded onto in developer mode, `adb run-as` against a debuggable build
reads all of the above without root. Nothing here is bound to the screen lock.

**Fix.** In rough cost order: (a) encrypt `chats.json` / `messages/*.json` with
an Android-Keystore-held key handed to `core` over stdin at startup, or via
`EncryptedFile` on the Kotlin side with `core` writing through a pipe; (b) do
the same for `whatsapp.db` (needs a SQLCipher-equivalent that works pure-Go —
non-trivial, `modernc.org/sqlite` has no encryption); (c) at minimum, shrink
what's persisted — media keys only need to survive as long as the download is
pending. Also worth adding an explicit "clear all local data" path for the
`logged_out` event (`:1772-1773` only logs and emits today; the caches stay).

---

## S3 — Path traversal via a sender-controlled WhatsApp message ID

**Severity: Medium.** `core/main.go:614-616`, `:684-686`, `:745-747`,
`:826-828`.

```go
func imagePath(jid, msgID, mimetype string) string {
	return filepath.Join("media", jid, msgID+"."+imageExtension(mimetype))
}
```

`msgID` is `evt.Info.ID` (live) or `key.GetID()` (history sync). Neither is
validated. whatsmeow parses it straight off the wire —
`whatsmeow/message.go:231`: `info.ID = types.MessageID(ag.String("id"))` — with
no charset restriction, and it is chosen by the *sending* client, not the
server. The history-sync path is a plain protobuf `string`, equally free-form.

`filepath.Join` cleans `..` segments, so a message ID like
`../../../../foo` resolves out of `media/<jid>/` and the subsequent
`os.WriteFile(path, data, 0o600)` writes attacker-supplied bytes at an
attacker-chosen location, with only the extension fixed. The four download
functions all `os.MkdirAll(filepath.Dir(path))` first, so intermediate
directories get created too.

Impact is bounded — the app has no external-storage permission, so the write
stays inside the app's own uid-owned tree, and the fixed `.jpg`/`.mp4`/`.webp`
/`.m4a` suffix blocks clobbering `whatsapp.db` or `chats.json` exactly. That
keeps this out of RCE territory. But it is still a **fully remote,
zero-interaction arbitrary-write primitive inside the app sandbox**, reachable
by anyone who can put a message in front of this account, and the resulting
path is then handed to `VideoView.setVideoPath` (`MainActivity.kt:709`),
`MediaPlayer.setDataSource` (`:966`), `MediaMetadataRetriever.setDataSource`
(`:1102`), and `BitmapFactory.decodeFile` (`:1087`) with no validation on the
Kotlin side either.

Note the `jid` component of the path is safe — it always comes from
`types.ParseJID` before reaching these helpers.

**Fix.** Two layers:
- In `core`, don't put a remote string in a filename. Either reject IDs not
  matching `^[A-Za-z0-9_-]{1,64}$`, or (better, no rejection needed) use
  `hex.EncodeToString(sha256.Sum256([]byte(msgID)))` as the on-disk name. The
  ID stays intact in the JSON; only the filename is derived.
- In `app`, resolve `File(context.filesDir, relativePath).canonicalPath` and
  refuse anything not under `filesDir.canonicalPath` before opening it.

---

## S4 — Live group messages trust `PushName` for the sender label

**Severity: Medium.** `core/main.go:1572-1575` vs `:583-592`.

The history-sync path does the right thing — it looks the participant up in the
local contact store and prefers `FullName`, then `PushName`:

```go
if contact, err := client.Store.Contacts.GetContact(ctx, pjid); err == nil && contact.Found {
	switch {
	case contact.FullName != "":
		cm.SenderName = contact.FullName
```

The live path does not:

```go
if evt.Info.IsGroup && !evt.Info.IsFromMe {
	cm.Sender = evt.Info.Sender.String()
	cm.SenderName = evt.Info.PushName
}
```

`PushName` is an arbitrary self-chosen string from the sender, unverified by
anyone. In a group, any member can set theirs to another member's exact
address-book name and every message they send afterward renders under that
name (`MainActivity.kt:751-755`). Because the address-book name is the *trusted*
label everywhere else in this UI, there is no cue that this one isn't.

The same inconsistency means the same sender can appear under two different
names depending on whether you're reading their message live or after a
history sync — a correctness bug on top of the spoofing one.

**Fix.** Make the live path use `contactName(ctx, client, evt.Info.Sender)`
first and fall back to `PushName` only when the contact store has nothing,
matching `extractHistoryMessage`. Optionally mark push-name-only senders
visually (e.g. `~Name`, which is what WhatsApp's own clients do).

---

## S5 — All DNS is hard-forced to plaintext `8.8.8.8`

**Severity: Medium (privacy).** `core/main.go:53-61`.

```go
net.DefaultResolver = &net.Resolver{
	PreferGo: true,
	Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "udp", "8.8.8.8:53")
	},
}
```

The workaround itself is legitimate and well-documented (`core/README.md`,
Android has no usable `/etc/resolv.conf` and there's no cgo resolver). The
problem is what it commits to:

- Every WhatsApp host and media-CDN lookup goes to Google, over unencrypted
  UDP, forever. That is a continuous stream of "this device is running a
  WhatsApp client, and is active right now" to a third party the user never
  chose, on a phone whose entire selling point is minimalism.
- It overrides the user's system DNS, Private DNS (DoT), and any VPN-provided
  resolver. A user who set those up specifically to avoid this gets silently
  bypassed.
- Any on-path observer sees the same metadata in cleartext.
- Functionally: networks that block or hijack `8.8.8.8` (captive portals,
  some corporate/hotel Wi-Fi, several national networks) break the app
  outright with no fallback.

Content is safe — whatsmeow's Noise handshake authenticates the server
independently, so DNS spoofing gets an attacker a failed connection, not a
MITM. This is a metadata and availability finding, not a confidentiality one.

**Fix.** Read Android's actual resolvers first (`getprop net.dns1`/`net.dns2`,
or have `app` pass `LinkProperties.getDnsServers()` down over stdin at
startup) and only fall back to a public resolver if that yields nothing. If a
hardcoded fallback stays, make it DNS-over-TLS or DoH rather than plaintext
:53, and make which resolver is used a visible choice rather than a buried
constant.

---

## S6 — The message cache, media, and voice recordings are not gitignored

**Severity: Medium (accidental disclosure).** `.gitignore`.

The Go section covers exactly:

```
core/core
core/*.db
core/*.db-shm
core/*.db-wal
```

Running `core` standalone on a desktop (`go run .` — the documented dev flow
in `core/README.md`) writes `chats.json`, `messages/<jid>.json`, `media/<jid>/`
and, after a send, moved voice recordings into the **same directory**, all
relative to the working dir. None of those are ignored. `git status` currently
shows the tree clean only because this checkout hasn't run the standalone path
since those features landed.

One `git add -A` on a public repo publishes real message bodies, contact names,
phone-number JIDs, downloaded media, and — for pending media — live CDN
decryption keys. This is a one-line fix with a very bad failure mode, which is
why it's rated above "housekeeping".

**Fix.** Add to `.gitignore`:

```
core/chats.json
core/messages/
core/media/
core/voice_tmp/
```

---

## S7 — Mention rewriting lets a sender render an arbitrary contact's real name

**Severity: Low.** `core/main.go:429-460`.

`mentionPattern` matches any `@` followed by 5-15 digits **anywhere in the
message text**, and rewrites it to that JID's address-book display name. The
source of truth is deliberately the text itself, not the protobuf's mention
list (the comment at `:422-428` explains why: it survives cache upgrades).

The consequence is that a sender can write `@15551234567` in ordinary prose and
have it render as `@Mum` — the recipient's own trusted, private address-book
name for that person. It reads as a genuine mention of someone the attacker
may not even know the name of. That's a meaningful phishing primitive in a
group chat, and it also leaks: the attacker can confirm whether a given number
is in the victim's address book by whether the number renders as a name.

**Fix.** Resolve only numbers that appear in the message's
`ContextInfo.MentionedJID`. To keep the cache-upgrade property that motivated
the current design, persist the mention list on the `chatMessage` and fall back
to the current text-scan behavior only for messages cached before that field
existed.

---

## S8 — `core` fully trusts its stdin command channel

**Severity: Low (defense-in-depth).** `core/main.go:1272-1304`, `:1073`,
`:1186-1198`, `:270-272`.

`readCommands` accepts four commands and validates only that the relevant
strings are non-empty:

- `open_chat` → `handleOpenChat(jid)` → `loadCachedMessages(jid)` →
  `filepath.Join("messages", jid+".json")`. The JID is never `ParseJID`'d, so
  a `../`-bearing value reads and later writes outside `messages/`.
- `send_audio` → `os.ReadFile(cmd.AudioPath)` with an arbitrary relative path,
  then uploads the contents to WhatsApp and `os.Rename`s the file. That's an
  arbitrary-file-read-and-exfiltrate primitive if the channel is ever reachable
  by anything but the app.

Today the pipe is private to the parent process, so this is not exploitable —
it's rated Low for that reason. It matters because `PROJECT.md` explicitly
plans to move this IPC to a local socket (Photon-style) once there's a
foreground service, and a local socket on Android is reachable by other apps
unless it's abstract-namespace-plus-peer-credential-checked. The validation
should exist before that move, not after.

**Fix.** `types.ParseJID` every incoming JID and use the parsed `.String()`
downstream (this also fixes the S3 sibling on the read path). Constrain
`audio_path` to the expected shape — `voice_tmp/<uuid>.m4a` — rather than
accepting any path. When the socket migration happens, add peer-uid checking.

---

## S9 — Non-atomic cache writes silently discard a whole chat's history

**Severity: Low (integrity/availability).** `core/main.go:249`, `:291`,
`:274-284`.

`saveChats` and `saveMessages` both use `os.WriteFile`, which truncates then
writes. A kill mid-write (the subprocess is Activity-scoped, so
`process.destroy()` fires on every Activity teardown — `CoreProcess.kt:112-117`)
leaves a truncated file. `loadCachedMessages` then fails `json.Unmarshal` and
**returns `nil` with no error surfaced** (`:280-282`), which is
indistinguishable from "this chat has no cached messages". Given WhatsApp's
history sync is one-time (documented at length in `PROJECT.md`), that history
is gone permanently.

**Fix.** Write to `<path>.tmp` then `os.Rename` (atomic on the same
filesystem). Log loudly on unmarshal failure instead of returning `nil`
silently, and consider keeping the corrupt file as `.bad` rather than letting
the next write overwrite the evidence.

---

## S10 — Raw internal error strings rendered in the UI

**Severity: Low.** `core/main.go:1136`, `:1196`, `:1203`, `:1223`, `:1794`,
`:1799`, `:1812`; rendered at `MainActivity.kt:167-170`.

`emit(event{Type: "error", Message: fmt.Sprintf("failed to send message: %v", err)})`
passes whatsmeow's error text straight to the screen. Those errors routinely
carry internal hostnames, JIDs, and protocol detail. Low impact on a personal
device; worth a mapping layer to user-facing strings, with the raw text kept on
stderr where it already goes.

---

# Performance

Context for severity: the target device is a Light Phone III — a low-power
e-ink-adjacent handset. Allocation churn and codec pressure hurt
disproportionately, and the subprocess is restarted on every Activity
recreation, so startup cost is paid often.

## P1 — Full-resolution bitmap decode for every image and sticker

**Severity: High.** `MainActivity.kt:1083-1090`, used at `:770`, `:793`.

```kotlin
value = withContext(Dispatchers.IO) {
    BitmapFactory.decodeFile(File(context.filesDir, relativePath).absolutePath)?.asImageBitmap()
}
```

No `BitmapFactory.Options`, so no `inSampleSize` and no `inJustDecodeBounds`
pre-pass. A 12-megapixel WhatsApp photo decodes to roughly 48 MB as
`ARGB_8888` — and is then drawn into a **200 dp** box (`:776`). Stickers get the
same treatment into a 120 dp box (`:799`). Several image messages visible at
once is enough to OOM or, at best, thrash GC hard.

There's also no cache: `produceState` is keyed on `relativePath`, so scrolling
an image off-screen and back re-runs the full decode from disk, every time.
P3's whole-list re-emissions make this worse — each re-emit rebuilds the
`Message` objects and can re-trigger decodes.

**Fix.** Two-pass decode: `inJustDecodeBounds = true` to read dimensions,
compute `inSampleSize` for the actual target size, then decode. Add a small
`LruCache<String, ImageBitmap>` sized against `Runtime.maxMemory()`, shared
across rows. Consider `RGB_565` for photos (no alpha needed) to halve the
footprint again.

## P2 — The whole chat list is re-serialized and written to disk on every message

**Severity: High.** `core/main.go:1532`, and callers at `:939`, `:972`,
`:1148`, `:1235`, `:1667`, `:1485`.

In `handleMessage`:

```go
changed := bumped || unread || !ok
if changed {
	chats[jid.String()] = c
}
list := saveChats(chats)   // <-- outside the guard
chatsMu.Unlock()
```

`saveChats` (`:242-252`) allocates a slice of all chats, sorts it, marshals it,
and writes the file. With the real dataset — 274 chats — that runs on **every
single incoming message**, including the ones where `changed` is false and
nothing needs persisting. It also runs *while holding `chatsMu`*, which
blocks the history-sync goroutine and the connected-event goroutine behind a
synchronous filesystem write.

**Fix.** Move the `saveChats` call inside `if changed`. Then decouple
persistence from emission: keep a dirty flag and flush on a ~1s debounce
(plus on shutdown), and do the marshal/write outside the mutex — snapshot the
slice under the lock, write after releasing it.

## P3 — Every media download and status receipt re-emits the entire message list

**Severity: High.** `core/main.go:662`, `:729`, `:811`, `:870`, `:1062`,
`:1103`, `:1169`, `:1266`, `:1581`.

Each of those is `emit(event{Type: "messages", JID: jid, Messages: resolveMentionsInList(ctx, client, list)})`
— the chat's **full** list, up to 500 messages, each carrying its
base64-encoded media-key blobs.

Open a chat with 40 backlogged images and `handleOpenChat` fires 40 concurrent
downloads (P5), each of which, on completion, serializes and pipes 500
messages to the app. The app then re-parses all 500 into fresh `Message`
objects (`CoreProcess.kt:209-231`), replaces the `StateFlow` value, and
recomposes the entire `LazyColumn` — 40 times, for 40 single-field changes.
Every one of those also pays P4's cost.

**Fix.** Add an incremental event — `{"type":"message_update","jid":...,"message":{...}}`
— for the single-message cases (download completion, status receipt). Keep the
full `messages` event for the initial `open_chat` reply only. On the Kotlin
side, apply updates in place against the existing list rather than replacing
it, so Compose's `key`-based diffing only invalidates the changed row.
Coalescing bursts (emit at most every ~200 ms) is a cheaper partial fix if the
protocol change is too large for one pass.

## P4 — Mention resolution hits a one-connection SQLite pool on every emit

**Severity: Medium-High.** `core/main.go:467-477`, `:443-460`, `:1736`.

`resolveMentionsInList` runs on every `messages` emit. Per message it runs the
regex, and per matched mention it does up to **two** `Store.Contacts.GetContact`
calls (LID server, then phone-number server) — each a SQLite query. There is
no cache; results are recomputed from scratch every time, by design
(`:462-466` explains the reasoning, which is sound — the issue is the cost,
not the choice).

Meanwhile `db.SetMaxOpenConns(1)` (`:1736`) forces every one of those queries
to serialize against whatsmeow's own session-store writes — Signal ratchet
updates, prekey uploads, app-state sync. Under P3's re-emit storm this is a
contention pile-up on the one thing that must stay responsive: the crypto
session store.

The `strings.Contains(text, "@")` early-out at `:444` helps, but only for
messages with no `@` at all.

**Fix.** Memoize mention resolution in a `map[string]string` guarded by its own
mutex, invalidated on `*events.Contact` / `*events.PushName` (both already have
handlers at `:1781-1784`). Combine with P11 (WAL) so lookups stop serializing
behind writes.

## P5 — Unbounded concurrent media downloads, each fully buffered in RAM

**Severity: Medium.** `core/main.go:1107-1118`, `:181`, `:627`, `:694`, `:835`.

```go
for _, m := range toDownload        { go downloadImage(...) }
for _, m := range toDownloadAudio   { go downloadAudio(...) }
for _, m := range toDownloadVideo   { go downloadVideo(...) }   // capped at 2
for _, m := range toDownloadSticker { go downloadSticker(...) }
```

Only video is bounded (`videoDownloadSem`, capacity 2) and only video streams
to disk (`DownloadMediaWithPathToFile`). Images, audio, and stickers each get
one unbounded goroutine and buffer the **entire decrypted file in memory**
via `DownloadMediaWithPath` before writing. A chat with 100 backlogged images
opens 100 simultaneous HTTPS downloads and holds 100 full image buffers at
once, on the LP3.

The comment at `:175-181` correctly identifies this risk — it just applies the
fix to only one of the four paths. (This matches the "4-copy media-download
duplication" already flagged in project notes.)

**Fix.** One shared semaphore across all four media types (a single
`mediaDownloadSem` with capacity ~3), and switch image/audio/sticker to
`DownloadMediaWithPathToFile` so nothing large is held in memory. That also
collapses the four near-identical download functions into one parameterized
helper, which is where the duplication was heading anyway.

## P6 — A `MediaPlayer` is prepared per visible audio row; thumbnails re-extracted on every scroll

**Severity: Medium.** `MainActivity.kt:949-994`, `:1096-1112`.

`AudioMessageRow` constructs a `MediaPlayer` and calls `prepare()` as soon as
the row composes — before the user has touched play. `MediaPlayer` holds a
hardware/codec handle; devices have a small fixed pool of them. A chat with
several voice messages on screen allocates and prepares one per row and holds
them until disposal.

`rememberDecodedVideoThumbnail` opens a `MediaMetadataRetriever` per video row
and pulls `frameAtTime` — a full demux + decode of the first frame — every time
the row composes. Scroll away and back and it runs again. Nothing is cached.

**Fix.** For audio: keep only the duration up front (already available as
`audioSeconds` from the protobuf, `:957`), construct and prepare the
`MediaPlayer` lazily on the first play tap, and keep at most one alive across
the whole screen. For video: extract the thumbnail once and cache it to disk
next to the video (`media/<jid>/<id>.thumb.jpg`), so the retriever runs once
per video ever, not once per scroll.

## P7 — In-memory message map never evicted; full re-sort and rewrite per message

**Severity: Medium.** `core/main.go:1751`, `:300-323`.

`messages := make(map[string][]chatMessage)` grows for the process's lifetime.
Every chat opened or receiving a message stays resident — worst case 274 chats
× 500 messages, with text bodies. Nothing is ever removed.

`upsertMessage` additionally, per message: linear-scans the list for the ID,
appends, `sort.Slice`s the **whole** list, trims, and then rewrites the
**entire** file. For a chat at the 500-message cap that's a 500-element sort
and a full JSON marshal-and-write per arriving message, all under `messagesMu`.

**Fix.** LRU-evict chats not touched recently (the on-disk cache is the source
of truth, so eviction is free). Replace the sort with an insertion at the
correct index — the list is already sorted and arrivals are almost always
newest, so this is O(1) amortized. Debounce the file write the same way as P2.

## P8 — QR bitmap built with 262,144 individual `setPixel` calls on the main thread

**Severity: Medium.** `QrLoginViewModel.kt:106-115`.

```kotlin
for (x in 0 until size) {
    for (y in 0 until size) {
        bitmap.setPixel(x, y, if (matrix[x, y]) Color.BLACK else Color.WHITE)
```

512×512 = 262,144 individual `Bitmap.setPixel` calls, each one a JNI crossing
with bounds checks. This runs inside the event collector on the main
dispatcher (`init` → `viewModelScope.launch` → `collect`, `:53-56`), so it
blocks the UI — and it re-runs on every `qr` event, which WhatsApp re-issues
periodically while the code is unscanned.

**Fix.** Fill an `IntArray(size * size)` in the loop and call
`bitmap.setPixels(...)` once — typically 50-100× faster. Move the whole encode
to `Dispatchers.Default`. Rendering the QR at its natural module size (~25-45 px)
and letting Compose scale it with nearest-neighbour filtering is cheaper still.

## P9 — `trySend` silently drops core events when the buffer fills

**Severity: Medium (correctness under load).** `CoreProcess.kt:99-110`.

```kotlin
process.inputStream.bufferedReader().forEachLine { line ->
    parseEvent(line)?.let { trySend(it) }
}
```

`callbackFlow`'s channel defaults to a 64-element buffer. `trySend` returns
`false` — **ignored here** — when it's full, and the event is dropped with no
log and no signal. During a history sync, `core` emits a full `chats` event per
sync chunk plus one per message (P2/P3), which is exactly the burst pattern
that overruns a 64-slot buffer while the collector is busy recomposing 274
rows. A dropped `chats`/`messages` event means stale UI until the next one
happens to arrive; a dropped `connected` or `logged_out` means a wedged screen.

**Fix.** At minimum, log when `trySend` fails. Better: `callbackFlow(...)
.buffer(Channel.UNLIMITED)` for lifecycle events, and conflate
`chats`/`messages` per JID so a burst collapses to the latest state rather than
queueing or dropping. Fixing P3 shrinks the burst that causes this.

## P10 — Scroll-chase effect reacts to every scroll frame

**Severity: Low-Medium.** `MainActivity.kt:391-405`.

The `snapshotFlow { Triple(firstVisibleItemIndex, firstVisibleItemScrollOffset, canScrollForward) }`
collector emits on every scroll position change — i.e. every frame during a
fling — and when its condition holds it calls `listState.scrollToItem(...)`,
which itself changes the position that triggers the flow. The anchor-comparison
guard (`anchorUnchanged`) is what keeps this from looping, and the reasoning in
the comment is careful, but it's a feedback loop held open by a single
equality check, running per frame, on the device's hot path.

**Fix.** Debounce the flow (~100 ms) so it can't fire mid-fling, and prefer
driving off item-size changes (`layoutInfo.visibleItemsInfo` sizes) rather than
scroll position, since the actual trigger being handled is "a row grew after
its image decoded". Cheaper still: reserve the final image height up front
(the protobuf carries dimensions) so rows don't grow at all.

## P11 — SQLite opened without WAL, capped at one connection

**Severity: Low.** `core/main.go:1731-1736`.

```go
db, err := sql.Open("sqlite", "file:whatsapp.db?_foreign_keys=on")
db.SetMaxOpenConns(1)
```

The comment explains the cap: rollback-journal mode allows one writer and
`modernc.org/sqlite` returns `SQLITE_BUSY` rather than queuing, so the pool cap
serializes writes in `database/sql` instead. That's a correct fix for the
symptom, but it also serializes every *read* — including P4's per-mention
contact lookups — behind unrelated Signal session writes.

**Fix.** Enable WAL and a busy timeout
(`?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_foreign_keys=on`),
which permits concurrent readers alongside one writer, then raise
`SetMaxOpenConns` to something like 4. Note the `.db-wal`/`.db-shm` files are
already gitignored, so no cleanup is needed there. Verify on-device — WAL on
some Android filesystems has its own quirks.

## P12 — Every cached chat file is re-read and re-parsed on each process start

**Severity: Low.** `core/main.go:1372-1414`, called at `:1755`.

`sanitizeMessageCache` walks all of `messages/`, and for each file that
canonicalizes to a different JID, loads both sides, merges, sorts, and writes.
It runs on every process start — and because the subprocess is Activity-scoped
(`CoreProcess.kt:112-117`), that's every Activity recreation, not just every
app launch. With 274 chats it's 274 `ReadDir` entries plus a `ParseJID` and a
`GetPNForLID` DB round-trip each, on the startup path before the first emit.

**Fix.** This is a one-time migration. Write a marker file (or a version key in
`chats.json`) once it completes and skip it thereafter. If it must stay
repeatable, make it lazy — do the canonicalization check per-JID inside
`loadCachedMessages` instead of eagerly for all chats up front.

---

# Verified sound — not findings

Called out so a later pass doesn't re-litigate them:

- **E2EE is not hand-rolled.** All Signal/Noise protocol work is whatsmeow's.
  The right call, and the reasoning is documented in `core/README.md`.
- **`allowBackup="false"`** (`AndroidManifest.xml:9`) — blocks `adb backup` and
  cloud backup of the session store.
- **Media keys are cleared after download** (`core/main.go:648-652` and the
  audio/video/sticker equivalents) — bounds the S2 key-exposure window.
- **File modes** are consistently `0600` for files and `0700` for directories
  across every write path.
- **Attack surface is minimal**: two permissions (`INTERNET`, `RECORD_AUDIO`),
  one exported activity (the launcher, required), one exported receiver
  (`SdkMarkerReceiver` — genuinely inert, no `onReceive` logic). No
  `WebView`, no JavaScript, no dynamic code loading, no `ContentProvider`, no
  deep links.
- **The subprocess binary is not a privilege boundary.** It ships in
  `nativeLibraryDir`, which is world-readable, but another app executing it
  runs it under *its own* uid with its own working directory — it gets an
  empty database, not this account's session.
- **stdout/stderr are correctly split** so human-readable logs can never
  corrupt the JSON event stream — and the logger never prints message bodies
  or key material.
- **`emitMu` / `chatsMu` / `messagesMu` / `openChatMu`** each guard what their
  comments claim, and the lock ordering is consistent (no path takes two of
  them nested).
- **`local.properties`** (holding the GitHub Packages token) is gitignored, and
  `settings.gradle.kts` falls back to env vars rather than embedding anything.

---

# Suggested order

Ordered by (impact × confidence) ÷ effort, not by severity alone.

**First — cheap and high-value:**
1. **S6** — four lines in `.gitignore`. Worst consequence-to-effort ratio in
   the document.
2. **P2** — move one `saveChats` call inside an existing `if`. Removes a full
   disk write per message.
3. **S4** — reuse the `contactName` call the history path already makes. Fixes
   a spoof and a name-inconsistency bug together.
4. **P8** — `setPixels` instead of 262k `setPixel`. Contained, visible win.

**Second — real work, clear payoff:**
5. **S1** — separate release keystore. Blocks anything that could be called a
   release build until it's done.
6. **P1** — downsample + cache bitmaps. The single biggest on-device
   performance and stability win.
7. **S3** — hash message IDs for filenames, canonical-path check on the Kotlin
   side. Closes the only remote write primitive.
8. **P5** — one shared download semaphore, stream to file. Pairs naturally with
   collapsing the four duplicated download functions.

**Third — protocol and structural:**
9. **P3 + P9 together** — incremental `message_update` event plus proper
   channel buffering. They're the same problem seen from both ends of the pipe.
10. **P4 + P11 together** — mention cache plus WAL. Same contention.
11. **S9** — atomic writes. Small, and it's guarding history that cannot be
    re-synced.
12. **P6, P7, P12** — media resource lifecycle, eviction, startup cost.

**Deliberately last:**
13. **S2** — encryption at rest. The largest job (Keystore integration, and
    `whatsapp.db` has no pure-Go encrypted driver), and S1 is a prerequisite
    for it to mean anything.
14. **S5** — DNS. Needs a real fix (system resolvers over stdin, or DoT), not
    a different hardcoded IP.
15. **S7, S8, S10, P10** — low-impact hardening; fold into whatever adjacent
    work touches them. S8 becomes mandatory *before* the planned local-socket
    IPC migration, not after.
