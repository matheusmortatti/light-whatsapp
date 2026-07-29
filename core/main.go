// core is the WhatsApp connection process for the Light Phone tool.
//
// It runs as a subprocess of the Android tool (see ../tool), speaking
// WhatsApp's multi-device protocol via whatsmeow and exposing a local
// IPC interface (TBD: unix socket vs localhost websocket — see PROJECT.md)
// for the Kotlin UI to consume. Session/key state lives in a local
// SQLite store (pure-Go driver, no CGO, so it cross-compiles cleanly
// for android/arm64).
//
// Right now this is a desktop-only prototype: it proves QR-based login
// against a real WhatsApp account works before any Android/IPC work
// begins (see PROJECT.md next steps).
package main

import (
	"context"
	"database/sql"
	"os"
	"os/signal"
	"syscall"

	"github.com/mdp/qrterminal/v3"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"

	_ "modernc.org/sqlite"
)

func main() {
	logger := waLog.Stdout("light-whatsapp", "INFO", true)
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
		}
	})

	if client.Store.ID == nil {
		// No existing session: request a QR code and wait for it to be scanned.
		qrChan, err := client.GetQRChannel(ctx)
		if err != nil {
			logger.Errorf("failed to get QR channel: %v", err)
			os.Exit(1)
		}
		if err = client.Connect(); err != nil {
			logger.Errorf("failed to connect: %v", err)
			os.Exit(1)
		}

		for item := range qrChan {
			switch item.Event {
			case "code":
				qrterminal.GenerateHalfBlock(item.Code, qrterminal.L, os.Stdout)
			case "success":
				logger.Infof("login successful, JID: %s", client.Store.ID)
			default:
				logger.Errorf("QR pairing failed: %s (%v)", item.Event, item.Error)
				os.Exit(1)
			}
		}
	} else {
		// Existing session: just reconnect.
		if err = client.Connect(); err != nil {
			logger.Errorf("failed to connect: %v", err)
			os.Exit(1)
		}
		logger.Infof("reconnected as %s", client.Store.ID)
	}

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan

	client.Disconnect()
}
