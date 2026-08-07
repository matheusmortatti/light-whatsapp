package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
)

// TestHistorySyncActivity verifies the sync_status debounce: a burst of
// history-sync chunks (markHistorySyncActive, possibly several) emits
// "syncing":true just once at the start, and going idle (markHistorySyncIdle,
// normally fired by the debounce timer — called directly here to avoid a
// real sleep) emits it again with syncing omitted (i.e. false).
func TestHistorySyncActivity(t *testing.T) {
	syncMu.Lock()
	syncing = false
	if syncTimer != nil {
		syncTimer.Stop()
		syncTimer = nil
	}
	syncMu.Unlock()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	origStdout := os.Stdout
	os.Stdout = w

	markHistorySyncActive()
	markHistorySyncActive() // second chunk of the same burst shouldn't re-emit
	markHistorySyncIdle()

	syncMu.Lock()
	if syncTimer != nil {
		syncTimer.Stop()
	}
	syncMu.Unlock()

	os.Stdout = origStdout
	w.Close()
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}

	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d emitted lines, want 2: %q", len(lines), out)
	}
	if !strings.Contains(lines[0], `"type":"sync_status"`) || !strings.Contains(lines[0], `"syncing":true`) {
		t.Errorf("first emit = %q, want sync_status syncing:true", lines[0])
	}
	if !strings.Contains(lines[1], `"type":"sync_status"`) || strings.Contains(lines[1], `"syncing"`) {
		t.Errorf("second emit = %q, want sync_status with syncing omitted (false)", lines[1])
	}
}

// TestHistorySyncConversationTimestamp guards against a real ordering bug
// seen after a relink: WhatsApp's post-relink sync stamped conversationTimestamp
// for many chats it wasn't sending full detail for to the same shared
// sync-boundary value — up to 90 minutes newer than some of those chats'
// actual last message — which used to win over both the cached (correct)
// timestamp and the real per-message data in the same chunk, scrambling
// the chat list's most-recent-first order.
func TestHistorySyncConversationTimestamp(t *testing.T) {
	t.Chdir(t.TempDir())
	client := whatsmeow.NewClient(&store.Device{}, waLog.Noop)
	ctx := context.Background()

	newHistoryMsg := func(msgID string, ts int64) *waHistorySync.HistorySyncMsg {
		return &waHistorySync.HistorySyncMsg{
			Message: &waWeb.WebMessageInfo{
				Key:              &waCommon.MessageKey{ID: proto.String(msgID), FromMe: proto.Bool(true)},
				MessageTimestamp: proto.Uint64(uint64(ts)),
				Message:          &waE2E.Message{Conversation: proto.String("hi")},
			},
		}
	}

	t.Run("bogus conversation-level timestamp does not override a cached one with no corroborating message", func(t *testing.T) {
		chats := map[string]chatSummary{
			"111@s.whatsapp.net": {JID: "111@s.whatsapp.net", Name: "Old Chat", Timestamp: 1000},
		}
		messages := make(map[string][]chatMessage)
		hs := &events.HistorySync{Data: &waHistorySync.HistorySync{
			Conversations: []*waHistorySync.Conversation{{
				ID:                    proto.String("111@s.whatsapp.net"),
				Name:                  proto.String("Old Chat"),
				ConversationTimestamp: proto.Uint64(9999), // sync-boundary stub, no real messages back it
			}},
		}}
		handleHistorySync(ctx, client, hs, chats, messages)
		if got := chats["111@s.whatsapp.net"].Timestamp; got != 1000 {
			t.Errorf("got timestamp %d, want cached 1000 preserved (not bogus 9999)", got)
		}
	})

	t.Run("real per-message timestamp in the same chunk still updates a cached one", func(t *testing.T) {
		chats := map[string]chatSummary{
			"222@s.whatsapp.net": {JID: "222@s.whatsapp.net", Name: "Active Chat", Timestamp: 1000},
		}
		messages := make(map[string][]chatMessage)
		hs := &events.HistorySync{Data: &waHistorySync.HistorySync{
			Conversations: []*waHistorySync.Conversation{{
				ID:                    proto.String("222@s.whatsapp.net"),
				Name:                  proto.String("Active Chat"),
				ConversationTimestamp: proto.Uint64(9999), // still bogus/higher
				Messages:              []*waHistorySync.HistorySyncMsg{newHistoryMsg("m1", 2000)},
			}},
		}}
		handleHistorySync(ctx, client, hs, chats, messages)
		if got := chats["222@s.whatsapp.net"].Timestamp; got != 2000 {
			t.Errorf("got timestamp %d, want real message timestamp 2000 (not bogus 9999, not stale 1000)", got)
		}
	})

	t.Run("a chat never seen before falls back to the conversation-level timestamp", func(t *testing.T) {
		chats := map[string]chatSummary{}
		messages := make(map[string][]chatMessage)
		hs := &events.HistorySync{Data: &waHistorySync.HistorySync{
			Conversations: []*waHistorySync.Conversation{{
				ID:                    proto.String("333@s.whatsapp.net"),
				Name:                  proto.String("New Chat"),
				ConversationTimestamp: proto.Uint64(9999),
			}},
		}}
		handleHistorySync(ctx, client, hs, chats, messages)
		if got := chats["333@s.whatsapp.net"].Timestamp; got != 9999 {
			t.Errorf("got timestamp %d, want 9999 (nothing better known for a brand-new chat)", got)
		}
	})
}

func TestReceiptStatusFor(t *testing.T) {
	tests := []struct {
		name     string
		isFromMe bool
		isGroup  bool
		rtype    types.ReceiptType
		want     string
	}{
		{"delivered receipt from recipient", false, false, types.ReceiptTypeDelivered, "delivered"},
		{"read receipt from recipient", false, false, types.ReceiptTypeRead, "read"},
		{"self read-sync ignored (handled by handleReadReceipt)", true, false, types.ReceiptTypeRead, ""},
		{"group chat ignored", false, true, types.ReceiptTypeDelivered, ""},
		{"unrelated receipt type ignored", false, false, types.ReceiptTypeRetry, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			evt := &events.Receipt{
				MessageSource: types.MessageSource{IsFromMe: tt.isFromMe, IsGroup: tt.isGroup},
				Type:          tt.rtype,
			}
			if got := receiptStatusFor(evt); got != tt.want {
				t.Errorf("receiptStatusFor() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestApplyMessageStatus(t *testing.T) {
	t.Run("sets status on matching from-me message", func(t *testing.T) {
		list := []chatMessage{
			{ID: "m1", FromMe: true, Status: "sent"},
			{ID: "m2", FromMe: false},
		}
		changed := applyMessageStatus(list, []types.MessageID{"m1"}, "delivered")
		if len(changed) != 1 || changed[0].ID != "m1" {
			t.Fatalf("expected changed = [m1], got %v", changed)
		}
		if list[0].Status != "delivered" {
			t.Errorf("list[0].Status = %q, want %q", list[0].Status, "delivered")
		}
		if list[1].Status != "" {
			t.Errorf("list[1].Status should stay untouched, got %q", list[1].Status)
		}
	})

	t.Run("never downgrades read back to delivered", func(t *testing.T) {
		list := []chatMessage{{ID: "m1", FromMe: true, Status: "read"}}
		changed := applyMessageStatus(list, []types.MessageID{"m1"}, "delivered")
		if len(changed) != 0 {
			t.Fatalf("expected no changes, read must not downgrade, got %v", changed)
		}
		if list[0].Status != "read" {
			t.Errorf("list[0].Status = %q, want %q", list[0].Status, "read")
		}
	})

	t.Run("message ID not found is a no-op", func(t *testing.T) {
		list := []chatMessage{{ID: "m1", FromMe: true, Status: "sent"}}
		changed := applyMessageStatus(list, []types.MessageID{"missing"}, "delivered")
		if len(changed) != 0 {
			t.Fatalf("expected no changes, got %v", changed)
		}
		if list[0].Status != "sent" {
			t.Errorf("list[0].Status = %q, want unchanged %q", list[0].Status, "sent")
		}
	})

	t.Run("ignores non-from-me messages even if ID matches", func(t *testing.T) {
		list := []chatMessage{{ID: "m1", FromMe: false, Status: ""}}
		changed := applyMessageStatus(list, []types.MessageID{"m1"}, "delivered")
		if len(changed) != 0 {
			t.Fatalf("expected no changes, got %v", changed)
		}
		if list[0].Status != "" {
			t.Errorf("list[0].Status should stay empty, got %q", list[0].Status)
		}
	})
}

func TestApplyReaction(t *testing.T) {
	t.Run("adds a new reaction", func(t *testing.T) {
		got := applyReaction(nil, "a@s.whatsapp.net", "", false, "👍")
		if len(got) != 1 || got[0].Sender != "a@s.whatsapp.net" || got[0].Emoji != "👍" {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("same sender reacting again replaces their emoji, doesn't add a second entry", func(t *testing.T) {
		list := []chatReaction{{Sender: "a@s.whatsapp.net", Emoji: "👍"}}
		got := applyReaction(list, "a@s.whatsapp.net", "", false, "❤️")
		if len(got) != 1 || got[0].Emoji != "❤️" {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("blank emoji removes the sender's existing reaction", func(t *testing.T) {
		list := []chatReaction{
			{Sender: "a@s.whatsapp.net", Emoji: "👍"},
			{Sender: "b@s.whatsapp.net", Emoji: "❤️"},
		}
		got := applyReaction(list, "a@s.whatsapp.net", "", false, "")
		if len(got) != 1 || got[0].Sender != "b@s.whatsapp.net" {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("blank emoji for a sender with no existing reaction is a no-op", func(t *testing.T) {
		list := []chatReaction{{Sender: "a@s.whatsapp.net", Emoji: "👍"}}
		got := applyReaction(list, "b@s.whatsapp.net", "", false, "")
		if len(got) != 1 || got[0].Sender != "a@s.whatsapp.net" {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("multiple senders can each have their own reaction", func(t *testing.T) {
		list := applyReaction(nil, "a@s.whatsapp.net", "", false, "👍")
		list = applyReaction(list, "b@s.whatsapp.net", "Bee", false, "❤️")
		if len(list) != 2 {
			t.Fatalf("got %+v", list)
		}
	})
}

func TestReactionSenderJID(t *testing.T) {
	t.Run("our own message resolves to EmptyJID", func(t *testing.T) {
		chat := types.NewJID("111", types.DefaultUserServer)
		target := chatMessage{ID: "m1", FromMe: true}
		got, err := reactionSenderJID(chat, target)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != types.EmptyJID {
			t.Errorf("got %v, want EmptyJID", got)
		}
	})

	t.Run("1:1 peer's message resolves to the chat JID itself", func(t *testing.T) {
		chat := types.NewJID("111", types.DefaultUserServer)
		target := chatMessage{ID: "m1", FromMe: false}
		got, err := reactionSenderJID(chat, target)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != chat {
			t.Errorf("got %v, want chat JID %v", got, chat)
		}
	})

	t.Run("group participant's message resolves to their JID", func(t *testing.T) {
		chat := types.NewJID("222", types.GroupServer)
		participant := types.NewJID("333", types.DefaultUserServer)
		target := chatMessage{ID: "m1", FromMe: false, Sender: participant.String()}
		got, err := reactionSenderJID(chat, target)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != participant {
			t.Errorf("got %v, want %v", got, participant)
		}
	})

	t.Run("group message with unparseable sender is an error", func(t *testing.T) {
		chat := types.NewJID("222", types.GroupServer)
		target := chatMessage{ID: "m1", FromMe: false, Sender: "not a jid"}
		_, err := reactionSenderJID(chat, target)
		if err == nil {
			t.Fatal("expected an error, got nil")
		}
	})
}

// TestCanonicalizeChatJIDBeforePairing guards against a real crash: a
// *store.Device fresh out of whatsmeow's Container.NewDevice() (no
// whatsapp.db row yet — first run, or a relink where whatsapp.db was
// cleared but chats.json survived) has nil Contacts/LIDs sub-stores until
// Save() runs post-pairing. canonicalizeChatJID must not touch them while
// client.Store.ID is nil, for any JID shape that would otherwise reach
// that lookup.
func TestCanonicalizeChatJIDBeforePairing(t *testing.T) {
	client := whatsmeow.NewClient(&store.Device{}, waLog.Noop)
	ctx := context.Background()

	t.Run("phone-number JID passes through unchanged", func(t *testing.T) {
		jid := types.NewJID("111", types.DefaultUserServer)
		if got := canonicalizeChatJID(ctx, client, jid); got != jid {
			t.Errorf("got %v, want unchanged %v", got, jid)
		}
	})

	t.Run("lid JID passes through unchanged instead of panicking", func(t *testing.T) {
		jid := types.NewJID("222", types.HiddenUserServer)
		if got := canonicalizeChatJID(ctx, client, jid); got != jid {
			t.Errorf("got %v, want unchanged %v", got, jid)
		}
	})

	t.Run("group JID passes through unchanged", func(t *testing.T) {
		jid := types.NewJID("333", types.GroupServer)
		if got := canonicalizeChatJID(ctx, client, jid); got != jid {
			t.Errorf("got %v, want unchanged %v", got, jid)
		}
	})
}

func TestExtractMessage(t *testing.T) {
	t.Run("video message", func(t *testing.T) {
		msg := &waE2E.Message{VideoMessage: &waE2E.VideoMessage{Caption: proto.String("look")}}
		text, msgType, _, _, video, sticker, _, ok := extractMessage(msg)
		if !ok || msgType != "video" || text != "look" || video == nil || sticker != nil {
			t.Fatalf("extractMessage() = text=%q type=%q video=%v sticker=%v ok=%v", text, msgType, video, sticker, ok)
		}
	})

	t.Run("gif is a video message with GifPlayback set", func(t *testing.T) {
		msg := &waE2E.Message{VideoMessage: &waE2E.VideoMessage{GifPlayback: proto.Bool(true)}}
		_, msgType, _, _, video, _, _, ok := extractMessage(msg)
		if !ok || msgType != "gif" || video == nil {
			t.Fatalf("extractMessage() = type=%q video=%v ok=%v", msgType, video, ok)
		}
	})

	t.Run("sticker message", func(t *testing.T) {
		msg := &waE2E.Message{StickerMessage: &waE2E.StickerMessage{IsAnimated: proto.Bool(true)}}
		_, msgType, _, _, _, sticker, _, ok := extractMessage(msg)
		if !ok || msgType != "sticker" || sticker == nil {
			t.Fatalf("extractMessage() = type=%q sticker=%v ok=%v", msgType, sticker, ok)
		}
	})

	t.Run("lottie sticker falls back to unsupported", func(t *testing.T) {
		msg := &waE2E.Message{StickerMessage: &waE2E.StickerMessage{IsLottie: proto.Bool(true)}}
		text, msgType, _, _, _, sticker, _, ok := extractMessage(msg)
		if !ok || msgType != "unsupported" || text != "lottie sticker" || sticker != nil {
			t.Fatalf("extractMessage() = text=%q type=%q sticker=%v ok=%v", text, msgType, sticker, ok)
		}
	})

	t.Run("extended text message carries its ContextInfo", func(t *testing.T) {
		ci := &waE2E.ContextInfo{StanzaID: proto.String("s1")}
		msg := &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{Text: proto.String("hi"), ContextInfo: ci}}
		text, msgType, _, _, _, _, gotCi, ok := extractMessage(msg)
		if !ok || msgType != "text" || text != "hi" || gotCi.GetStanzaID() != "s1" {
			t.Fatalf("extractMessage() = text=%q type=%q ci=%v ok=%v", text, msgType, gotCi, ok)
		}
	})

	t.Run("plain conversation text carries no ContextInfo", func(t *testing.T) {
		msg := &waE2E.Message{Conversation: proto.String("hi")}
		_, msgType, _, _, _, _, gotCi, ok := extractMessage(msg)
		if !ok || msgType != "text" || gotCi != nil {
			t.Fatalf("extractMessage() = type=%q ci=%v ok=%v", msgType, gotCi, ok)
		}
	})
}

func TestWebMessageInfoStatus(t *testing.T) {
	tests := []struct {
		name   string
		status waWeb.WebMessageInfo_Status
		want   string
	}{
		{"server ack maps to sent", waWeb.WebMessageInfo_SERVER_ACK, "sent"},
		{"delivery ack maps to delivered", waWeb.WebMessageInfo_DELIVERY_ACK, "delivered"},
		{"read maps to read", waWeb.WebMessageInfo_READ, "read"},
		{"played maps to read", waWeb.WebMessageInfo_PLAYED, "read"},
		{"pending maps to unknown", waWeb.WebMessageInfo_PENDING, ""},
		{"error maps to unknown", waWeb.WebMessageInfo_ERROR, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := webMessageInfoStatus(tt.status); got != tt.want {
				t.Errorf("webMessageInfoStatus(%v) = %q, want %q", tt.status, got, tt.want)
			}
		})
	}
}

func TestIsPermanentDownloadFailure(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"403 is permanent", whatsmeow.DownloadHTTPError{Response: &http.Response{StatusCode: 403}}, true},
		{"404 is permanent", whatsmeow.DownloadHTTPError{Response: &http.Response{StatusCode: 404}}, true},
		{"410 is permanent", whatsmeow.DownloadHTTPError{Response: &http.Response{StatusCode: 410}}, true},
		{"500 is transient", whatsmeow.DownloadHTTPError{Response: &http.Response{StatusCode: 500}}, false},
		{"wrapped 403 is still permanent", fmt.Errorf("download failed: %w", whatsmeow.DownloadHTTPError{Response: &http.Response{StatusCode: 403}}), true},
		{"generic network error is transient", errors.New("connection reset"), false},
		{"nil is transient", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isPermanentDownloadFailure(c.err); got != c.want {
				t.Errorf("isPermanentDownloadFailure(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}

func TestUpdateCachedMessage(t *testing.T) {
	messages := map[string][]chatMessage{
		"jid1": {
			{ID: "a", Text: "hello"},
			{ID: "b", Text: "world"},
		},
	}

	updated, found := updateCachedMessage(messages, "jid1", "b", func(cm *chatMessage) {
		cm.Text = "edited"
	})
	if !found {
		t.Fatal("expected found=true for existing message")
	}
	if updated.Text != "edited" {
		t.Errorf("updated.Text = %q, want %q", updated.Text, "edited")
	}
	if messages["jid1"][1].Text != "edited" {
		t.Errorf("messages map not updated in place: got %q", messages["jid1"][1].Text)
	}

	_, found = updateCachedMessage(messages, "jid1", "missing-id", func(cm *chatMessage) {})
	if found {
		t.Error("expected found=false for missing message id")
	}

	_, found = updateCachedMessage(messages, "missing-jid", "a", func(cm *chatMessage) {})
	if found {
		t.Error("expected found=false for missing jid")
	}
}

func TestApplyImageFailureClearsDownloadState(t *testing.T) {
	cm := chatMessage{
		ID:                 "m1",
		Type:               "image",
		ImageDirectPath:    "/some/path",
		ImageMediaKey:      []byte("key"),
		ImageFileSHA256:    []byte("sha"),
		ImageFileEncSHA256: []byte("encsha"),
		ImageMimetype:      "image/jpeg",
		Text:               "caption preserved",
	}
	applyImageFailure(&cm)

	if !cm.ImageFailed {
		t.Error("expected ImageFailed = true")
	}
	if cm.ImageDirectPath != "" {
		t.Errorf("expected ImageDirectPath cleared, got %q", cm.ImageDirectPath)
	}
	if cm.ImageMediaKey != nil || cm.ImageFileSHA256 != nil || cm.ImageFileEncSHA256 != nil || cm.ImageMimetype != "" {
		t.Error("expected all image key material cleared")
	}
	if cm.Text != "caption preserved" {
		t.Error("applyImageFailure must not touch unrelated fields")
	}
}
