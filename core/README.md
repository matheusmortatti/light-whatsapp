# core

Go subprocess that speaks WhatsApp's multi-device protocol on behalf of the
Light Phone tool (`../tool`). Built on [whatsmeow](https://github.com/tulir/whatsmeow)
(MPL-2.0) — the same library `mautrix-whatsapp` / Beeper's own WhatsApp bridge
use server-side.

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

Stub only. `main.go` just proves the dependency graph (whatsmeow +
`modernc.org/sqlite`, pure-Go, no CGO) compiles and cross-compiles cleanly.
No WhatsApp logic yet.

## Next steps (see also ../PROJECT.md)

1. Prototype QR-login + receiving messages standalone on desktop first
   (`whatsmeow`'s [package example](https://pkg.go.dev/go.mau.fi/whatsmeow#example-package)
   is a complete minimal client) — validate against a real WhatsApp account
   before touching Android at all.
2. Cross-compile for `android/arm64`:
   ```
   CGO_ENABLED=0 GOOS=android GOARCH=arm64 go build -o whatsmeow-core .
   ```
   Confirm the resulting binary actually runs under `adb shell` on the
   LightOS emulator / real device — Photon's docs note pending
   hardware-verification gaps, don't assume parity with desktop Go.
3. Design the IPC contract with `../tool` (message schema for
   incoming/outgoing chat events, QR pairing hand-off, connection state).
   Localhost websocket (Photon's choice) is the path of least resistance.
4. Session/key store: SQLite via `modernc.org/sqlite` (already wired up),
   same as Photon — avoids CGO/NDK entirely.

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
