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

## Architecture

```
tool/ (Kotlin, Compose, light-sdk)  <-- localhost socket -->  core/ (Go, whatsmeow)
     LightScreen-based UI                                       WhatsApp multi-device
     foreground service supervises                              protocol + E2EE
     the core/ subprocess                                       SQLite session store
```

- **`core/`**: Go module, whatsmeow-based, cross-compiled `CGO_ENABLED=0` for
  `android/arm64`, run as a subprocess. See `core/README.md`.
- **`tool/`**: the light-sdk Android tool module — currently still Light's
  sample scaffold (`HomeScreen`/`DetailScreen`/`ToolEntryPoint`). This is
  where the chat-list/thread UI and the foreground service that owns the
  `core/` subprocess will go.
- IPC contract between the two is not yet designed — reference Photon's
  implementation (same problem, same device, MIT licensed) before
  inventing one from scratch.

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
  via ADB sideload.
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

1. Get the light-sdk emulator running locally, confirm the unmodified
   sample `tool/` builds and launches (validates the whole toolchain before
   touching any WhatsApp code).
2. In `core/`, prototype whatsmeow QR-login + send/receive standalone on
   desktop Go, against a real WhatsApp account — validate the library
   works for your account before involving Android at all.
3. Cross-compile `core/` for `android/arm64`, confirm it actually runs via
   `adb shell` on the emulator/device.
4. Design the IPC contract between `tool/` and `core/` (reference Photon).
5. Build the foreground service in `tool/` that launches and supervises the
   `core/` subprocess.
6. Repurpose `HomeScreen`/`DetailScreen` into chat-list/thread screens.
7. Media handling, then groups, then polish — in that order.
