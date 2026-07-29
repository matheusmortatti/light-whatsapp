// core is the WhatsApp connection process for the Light Phone tool.
//
// It runs as a subprocess of the Android tool (see ../tool), speaking
// WhatsApp's multi-device protocol via whatsmeow and exposing a local
// IPC interface (TBD: unix socket vs localhost websocket — see PROJECT.md)
// for the Kotlin UI to consume. Session/key state lives in a local
// SQLite store (pure-Go driver, no CGO, so it cross-compiles cleanly
// for android/arm64).
package main

import (
	"database/sql"
	"log"

	_ "modernc.org/sqlite"
	"go.mau.fi/whatsmeow"
)

func main() {
	log.Println("light-whatsapp core: not yet implemented, see PROJECT.md next steps")
	_ = whatsmeow.Client{}
	var _ *sql.DB
}
