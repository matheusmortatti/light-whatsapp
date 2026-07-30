# core

Go subprocess that speaks WhatsApp's multi-device protocol on behalf of the
Light Phone app (`../app` — a standalone Android app, deliberately *not* the
`light-sdk` `tool/` module; see `PROJECT.md`'s decision log for why). Built on
[whatsmeow](https://github.com/tulir/whatsmeow) (MPL-2.0) — the same library
`mautrix-whatsapp` / Beeper's own WhatsApp bridge use server-side.

## Why a subprocess instead of native Kotlin code

whatsmeow is Go, not Kotlin/Java, and reimplementing WhatsApp's Signal-based
E2EE + multi-device protocol by hand is out of scope. [Photon](https://github.com/joshkarlin/photon)
(MIT, an existing WhatsApp/Signal/SMS client built specifically for the Light
Phone III) validated this exact pattern on real hardware: cross-compile a
pure-Go binary (`CGO_ENABLED=0`, so no NDK needed) for `android/arm64`, run it
as a subprocess from a Kotlin foreground service, talk to it over a local
socket. Read Photon's implementation before building the IPC layer here —
no need to re-derive it from scratch.

## State

QR-login works, both standalone (`go run .`) and as an on-device subprocess
launched by `../app` — confirmed end-to-end on the LightOS emulator
(2026-07-29): launched via LightOS's own tool switcher, subprocess spawned,
connected to WhatsApp's servers, real QR rendered on-screen. No chat/message
logic yet.

## Android DNS

Android has no usable `/etc/resolv.conf`, so Go's pure-Go DNS resolver (the
only option without cgo/NDK) fails every lookup with something like
`dial tcp: lookup web.whatsapp.com on [::1]:53: connection refused`. Fixed in
`main.go`'s `init()` by pointing `net.DefaultResolver` at a public DNS server
(`8.8.8.8:53`) directly, bypassing system resolv.conf discovery entirely.
Found via on-device testing, not something desktop `go run .` would ever
surface — real LP3 hardware needs this too, not just the emulator.

## stdout protocol

`main.go` writes one JSON object per line to stdout — this is the entire IPC
contract for the QR-login phase (see `../app/src/main/kotlin/.../CoreProcess.kt`
for the consumer):

```json
{"type":"qr","code":"..."}
{"type":"connected","jid":"..."}
{"type":"logged_out"}
{"type":"error","message":"..."}
```

All human-readable logging goes to stderr instead (see `stderrLogger` in
`main.go`) so it never interleaves with the stdout event stream. The Android
side sets the subprocess's working directory to its app-private files dir
(`ProcessBuilder.directory(...)`), so `main.go`'s relative `whatsapp.db` path
needs no changes to land in the right place.

This one-directional protocol is intentionally minimal — enough for a login
flow, not enough for chat. Extending it (or replacing it with a bidirectional
local socket, Photon's choice) is chat-UI-phase work.

## Building for Android

`./build_android.sh` cross-compiles and drops the binary at
`../app/src/main/jniLibs/arm64-v8a/libwhatsmeowcore.so`. The `lib*.so` name
and `jniLibs` location are load-bearing: Android extracts and chmods files
under `jniLibs` as executable native libs, which is how a plain-Go binary
(no JNI involved) gets to run as a real subprocess despite Android's
app-private-storage `noexec` restriction. `../app/build.gradle.kts` also sets
`packaging.jniLibs.useLegacyPackaging = true` so the file is actually
extracted to disk at install time rather than left zipped in the APK.

## Next steps (see also ../PROJECT.md)

1. On-device verification: confirm the subprocess actually runs under
   `../app` on the LightOS emulator / real device (cross-compile output has
   been confirmed to be a valid ARM aarch64 ELF, but not yet exercised
   on-device) — Photon's docs note pending hardware-verification gaps,
   don't assume parity with desktop Go.
2. Chat/message events: extend the stdout protocol (or move to a
   bidirectional local socket — Photon's choice) once `../app` needs to
   receive/send actual messages, not just complete login.
3. Persistent connection: `main.go` currently exits when its stdin/parent
   process goes away, matching `../app`'s Activity-scoped subprocess
   lifetime. Real-time delivery needs this paired with a foreground service
   on the Android side (see `../PROJECT.md` next steps).

## Known risks specific to this module

- **Ban risk**: whatsmeow-based unofficial clients have been caught in
  WhatsApp/Meta ban waves before. Expect occasional relink, not a one-time
  solved problem.
- **No push gateway**: unlike Matrix, there's no open push mechanism here —
  real-time delivery requires keeping the websocket connection open via a
  foreground service the whole time the tool is "active."
- **Linked-device cap**: WhatsApp multi-device historically caps companion
  devices per account (~4) and some sync edge cases still depend on the
  primary phone having been online.
