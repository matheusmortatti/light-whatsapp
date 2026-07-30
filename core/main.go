// core is the WhatsApp connection process for the Light Phone app (see
// ../app). It runs as a subprocess launched by the Android app, speaking
// WhatsApp's multi-device protocol via whatsmeow. Session/key state lives
// in a local SQLite store (pure-Go driver, no CGO, so it cross-compiles
// cleanly for android/arm64), written relative to the process's working
// directory — the Android side points that at its app-private files dir.
//
// stdout is a machine-readable protocol: one JSON object per line, see
// the event type below. All human-readable logging goes to stderr so it
// never gets mixed into the stdout event stream. This is the whole IPC
// contract for the QR-login phase; a richer contract (bidirectional,
// probably a local socket — see Photon) is deferred until chat UI work
// begins (see PROJECT.md).
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"

	_ "modernc.org/sqlite"
)

// Android has no usable /etc/resolv.conf, so Go's pure-Go DNS resolver
// (the only option without cgo/NDK — see core/README.md) falls back to
// querying nothing at [::1]:53 and every lookup fails with "connection
// refused". Point net.DefaultResolver at a public DNS server directly to
// bypass system resolv.conf discovery entirely. Confirmed necessary via
// on-device testing (2026-07-29): without this, whatsmeow's websocket
// dial to web.whatsapp.com fails outright.
func init() {
	net.DefaultResolver = &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "udp", "8.8.8.8:53")
		},
	}
}

// event is one line of the stdout protocol. Exactly one of the fields
// relevant to Type is populated.
type event struct {
	Type    string `json:"type"` // "qr" | "connected" | "logged_out" | "error"
	Code    string `json:"code,omitempty"`
	JID     string `json:"jid,omitempty"`
	Message string `json:"message,omitempty"`
}

func emit(e event) {
	// Errors here would mean stdout is gone (pipe closed by the
	// supervising process) — nothing useful to do but ignore it.
	_ = json.NewEncoder(os.Stdout).Encode(e)
}

// stderrLogger is waLog.Stdout's formatting, redirected to stderr so it
// never interleaves with the stdout event protocol.
type stderrLogger struct {
	mod string
}

func (l *stderrLogger) log(level, msg string, args ...any) {
	fmt.Fprintf(os.Stderr, "%s [%s %s] %s\n", time.Now().Format("15:04:05.000"), l.mod, level, fmt.Sprintf(msg, args...))
}
func (l *stderrLogger) Errorf(msg string, args ...any) { l.log("ERROR", msg, args...) }
func (l *stderrLogger) Warnf(msg string, args ...any)  { l.log("WARN", msg, args...) }
func (l *stderrLogger) Infof(msg string, args ...any)  { l.log("INFO", msg, args...) }
func (l *stderrLogger) Debugf(msg string, args ...any) { l.log("DEBUG", msg, args...) }
func (l *stderrLogger) Sub(mod string) waLog.Logger {
	return &stderrLogger{mod: l.mod + "/" + mod}
}

func main() {
	logger := waLog.Logger(&stderrLogger{mod: "light-whatsapp"})
	ctx := context.Background()

	// SQLite only allows one writer at a time, and sqlstore.New leaves the
	// pool unbounded, so concurrent writes (prekey upload, app-state sync)
	// race and modernc.org/sqlite returns SQLITE_BUSY instead of queuing.
	// Capping the pool at 1 connection makes database/sql serialize them.
	db, err := sql.Open("sqlite", "file:whatsapp.db?_foreign_keys=on")
	if err != nil {
		logger.Errorf("failed to open database: %v", err)
		os.Exit(1)
	}
	db.SetMaxOpenConns(1)

	container := sqlstore.NewWithDB(db, "sqlite", logger)
	if err = container.Upgrade(ctx); err != nil {
		logger.Errorf("failed to upgrade device store: %v", err)
		os.Exit(1)
	}

	deviceStore, err := container.GetFirstDevice(ctx)
	if err != nil {
		logger.Errorf("failed to load device: %v", err)
		os.Exit(1)
	}

	client := whatsmeow.NewClient(deviceStore, logger)
	client.AddEventHandler(func(evt any) {
		switch evt.(type) {
		case *events.Connected:
			logger.Infof("connected")
		case *events.LoggedOut:
			logger.Warnf("logged out, delete whatsapp.db and re-run to link again")
			emit(event{Type: "logged_out"})
		}
	})

	if client.Store.ID == nil {
		// No existing session: request a QR code and wait for it to be scanned.
		qrChan, err := client.GetQRChannel(ctx)
		if err != nil {
			logger.Errorf("failed to get QR channel: %v", err)
			emit(event{Type: "error", Message: err.Error()})
			os.Exit(1)
		}
		if err = client.Connect(); err != nil {
			logger.Errorf("failed to connect: %v", err)
			emit(event{Type: "error", Message: err.Error()})
			os.Exit(1)
		}

		for item := range qrChan {
			switch item.Event {
			case "code":
				emit(event{Type: "qr", Code: item.Code})
			case "success":
				logger.Infof("login successful, JID: %s", client.Store.ID)
				emit(event{Type: "connected", JID: client.Store.ID.String()})
			default:
				logger.Errorf("QR pairing failed: %s (%v)", item.Event, item.Error)
				emit(event{Type: "error", Message: fmt.Sprintf("QR pairing failed: %s", item.Event)})
				os.Exit(1)
			}
		}
	} else {
		// Existing session: just reconnect.
		if err = client.Connect(); err != nil {
			logger.Errorf("failed to connect: %v", err)
			emit(event{Type: "error", Message: err.Error()})
			os.Exit(1)
		}
		logger.Infof("reconnected as %s", client.Store.ID)
		emit(event{Type: "connected", JID: client.Store.ID.String()})
	}

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan

	client.Disconnect()
}
