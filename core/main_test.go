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

func TestApplyPollVote(t *testing.T) {
	t.Run("adds a new vote", func(t *testing.T) {
		got := applyPollVote(nil, "a@s.whatsapp.net", "", false, []int{0})
		if len(got) != 1 || got[0].Sender != "a@s.whatsapp.net" || len(got[0].SelectedOptions) != 1 || got[0].SelectedOptions[0] != 0 {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("same sender voting again replaces their selection, doesn't add a second entry", func(t *testing.T) {
		votes := []chatPollVote{{Sender: "a@s.whatsapp.net", SelectedOptions: []int{0}}}
		got := applyPollVote(votes, "a@s.whatsapp.net", "", false, []int{1, 2})
		if len(got) != 1 || len(got[0].SelectedOptions) != 2 || got[0].SelectedOptions[0] != 1 {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("empty selection removes the sender's existing vote (retract)", func(t *testing.T) {
		votes := []chatPollVote{
			{Sender: "a@s.whatsapp.net", SelectedOptions: []int{0}},
			{Sender: "b@s.whatsapp.net", SelectedOptions: []int{1}},
		}
		got := applyPollVote(votes, "a@s.whatsapp.net", "", false, nil)
		if len(got) != 1 || got[0].Sender != "b@s.whatsapp.net" {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("empty selection for a sender with no existing vote is a no-op", func(t *testing.T) {
		votes := []chatPollVote{{Sender: "a@s.whatsapp.net", SelectedOptions: []int{0}}}
		got := applyPollVote(votes, "b@s.whatsapp.net", "", false, nil)
		if len(got) != 1 || got[0].Sender != "a@s.whatsapp.net" {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("multiple senders can each have their own vote", func(t *testing.T) {
		votes := applyPollVote(nil, "a@s.whatsapp.net", "", false, []int{0})
		votes = applyPollVote(votes, "b@s.whatsapp.net", "Bee", false, []int{1})
		if len(votes) != 2 {
			t.Fatalf("got %+v", votes)
		}
	})
}

func TestMatchPollVoteOptions(t *testing.T) {
	options := []string{"Pepperoni", "Mushroom", "Pineapple"}
	hashes := whatsmeow.HashPollOptions(options)

	t.Run("matches a single selected hash to its option index", func(t *testing.T) {
		got := matchPollVoteOptions(options, [][]byte{hashes[1]})
		if len(got) != 1 || got[0] != 1 {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("matches multiple selected hashes in order", func(t *testing.T) {
		got := matchPollVoteOptions(options, [][]byte{hashes[2], hashes[0]})
		if len(got) != 2 || got[0] != 2 || got[1] != 0 {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("an unmatched hash is skipped, not appended as -1 or similar", func(t *testing.T) {
		got := matchPollVoteOptions(options, [][]byte{[]byte("not a real hash")})
		if len(got) != 0 {
			t.Fatalf("got %+v, want empty", got)
		}
	})

	t.Run("no selected hashes returns an empty (not nil-vs-empty-sensitive) slice", func(t *testing.T) {
		got := matchPollVoteOptions(options, nil)
		if len(got) != 0 {
			t.Fatalf("got %+v, want empty", got)
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
		text, msgType, _, _, video, sticker, _, _, ok := extractMessage(msg)
		if !ok || msgType != "video" || text != "look" || video == nil || sticker != nil {
			t.Fatalf("extractMessage() = text=%q type=%q video=%v sticker=%v ok=%v", text, msgType, video, sticker, ok)
		}
	})

	t.Run("gif is a video message with GifPlayback set", func(t *testing.T) {
		msg := &waE2E.Message{VideoMessage: &waE2E.VideoMessage{GifPlayback: proto.Bool(true)}}
		_, msgType, _, _, video, _, _, _, ok := extractMessage(msg)
		if !ok || msgType != "gif" || video == nil {
			t.Fatalf("extractMessage() = type=%q video=%v ok=%v", msgType, video, ok)
		}
	})

	t.Run("sticker message", func(t *testing.T) {
		msg := &waE2E.Message{StickerMessage: &waE2E.StickerMessage{IsAnimated: proto.Bool(true)}}
		_, msgType, _, _, _, sticker, _, _, ok := extractMessage(msg)
		if !ok || msgType != "sticker" || sticker == nil {
			t.Fatalf("extractMessage() = type=%q sticker=%v ok=%v", msgType, sticker, ok)
		}
	})

	t.Run("lottie sticker falls back to unsupported", func(t *testing.T) {
		msg := &waE2E.Message{StickerMessage: &waE2E.StickerMessage{IsLottie: proto.Bool(true)}}
		text, msgType, _, _, _, sticker, _, _, ok := extractMessage(msg)
		if !ok || msgType != "unsupported" || text != "lottie sticker" || sticker != nil {
			t.Fatalf("extractMessage() = text=%q type=%q sticker=%v ok=%v", text, msgType, sticker, ok)
		}
	})

	t.Run("extended text message carries its ContextInfo", func(t *testing.T) {
		ci := &waE2E.ContextInfo{StanzaID: proto.String("s1")}
		msg := &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{Text: proto.String("hi"), ContextInfo: ci}}
		text, msgType, _, _, _, _, _, gotCi, ok := extractMessage(msg)
		if !ok || msgType != "text" || text != "hi" || gotCi.GetStanzaID() != "s1" {
			t.Fatalf("extractMessage() = text=%q type=%q ci=%v ok=%v", text, msgType, gotCi, ok)
		}
	})

	t.Run("plain conversation text carries no ContextInfo", func(t *testing.T) {
		msg := &waE2E.Message{Conversation: proto.String("hi")}
		_, msgType, _, _, _, _, _, gotCi, ok := extractMessage(msg)
		if !ok || msgType != "text" || gotCi != nil {
			t.Fatalf("extractMessage() = type=%q ci=%v ok=%v", msgType, gotCi, ok)
		}
	})

	t.Run("a genuinely unrecognized content type is still shown as unsupported", func(t *testing.T) {
		msg := &waE2E.Message{LocationMessage: &waE2E.LocationMessage{}}
		text, msgType, _, _, _, _, _, _, ok := extractMessage(msg)
		if !ok || msgType != "unsupported" || text != "location" {
			t.Fatalf("extractMessage() = text=%q type=%q ok=%v", text, msgType, ok)
		}
	})

	t.Run("a message with only a sender-key-distribution envelope is dropped, not shown as unsupported", func(t *testing.T) {
		msg := &waE2E.Message{SenderKeyDistributionMessage: &waE2E.SenderKeyDistributionMessage{GroupID: proto.String("g1")}}
		text, msgType, _, _, _, _, _, _, ok := extractMessage(msg)
		if ok || text != "" || msgType != "" {
			t.Fatalf("extractMessage() = text=%q type=%q ok=%v, want dropped (ok=false)", text, msgType, ok)
		}
	})

	t.Run("a message with only MessageContextInfo is dropped, not shown as unsupported", func(t *testing.T) {
		msg := &waE2E.Message{MessageContextInfo: &waE2E.MessageContextInfo{MessageSecret: []byte("s")}}
		text, msgType, _, _, _, _, _, _, ok := extractMessage(msg)
		if ok || text != "" || msgType != "" {
			t.Fatalf("extractMessage() = text=%q type=%q ok=%v, want dropped (ok=false)", text, msgType, ok)
		}
	})

	t.Run("a fully empty message is dropped, not shown as unsupported", func(t *testing.T) {
		msg := &waE2E.Message{}
		text, msgType, _, _, _, _, _, _, ok := extractMessage(msg)
		if ok || text != "" || msgType != "" {
			t.Fatalf("extractMessage() = text=%q type=%q ok=%v, want dropped (ok=false)", text, msgType, ok)
		}
	})

	t.Run("poll message", func(t *testing.T) {
		msg := &waE2E.Message{PollCreationMessageV3: &waE2E.PollCreationMessage{
			Name: proto.String("Best pizza topping?"),
			Options: []*waE2E.PollCreationMessage_Option{
				{OptionName: proto.String("Pepperoni")},
				{OptionName: proto.String("Mushroom")},
			},
			SelectableOptionsCount: proto.Uint32(1),
		}}
		text, msgType, _, _, _, _, poll, _, ok := extractMessage(msg)
		if !ok || msgType != "poll" || text != "Best pizza topping?" || poll == nil {
			t.Fatalf("extractMessage() = text=%q type=%q poll=%v ok=%v", text, msgType, poll, ok)
		}
		if len(poll.GetOptions()) != 2 || poll.GetOptions()[0].GetOptionName() != "Pepperoni" {
			t.Fatalf("poll options = %+v", poll.GetOptions())
		}
	})

	t.Run("poll message checks every real protocol variant, not just the base field", func(t *testing.T) {
		variants := []*waE2E.Message{
			{PollCreationMessage: &waE2E.PollCreationMessage{Name: proto.String("q")}},
			{PollCreationMessageV2: &waE2E.PollCreationMessage{Name: proto.String("q")}},
			{PollCreationMessageV3: &waE2E.PollCreationMessage{Name: proto.String("q")}},
			{PollCreationMessageV5: &waE2E.PollCreationMessage{Name: proto.String("q")}},
			{PollCreationMessageV6: &waE2E.PollCreationMessage{Name: proto.String("q")}},
		}
		for i, msg := range variants {
			_, msgType, _, _, _, _, poll, _, ok := extractMessage(msg)
			if !ok || msgType != "poll" || poll == nil {
				t.Fatalf("variant %d: extractMessage() = type=%q poll=%v ok=%v", i, msgType, poll, ok)
			}
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

// fakeContactStore is a minimal store.ContactStore test double: GetContact
// reports a fixed name for one specific JID and "not found" for everything
// else, letting TestSetQuotedFields exercise the real contact-lookup path
// (cachedMentionName -> contactName -> client.Store.Contacts.GetContact)
// without a real SQLite-backed store.
type fakeContactStore struct {
	known types.JID
	name  string
}

func (f fakeContactStore) PutPushName(ctx context.Context, user types.JID, pushName string) (bool, string, error) {
	return false, "", nil
}
func (f fakeContactStore) PutBusinessName(ctx context.Context, user types.JID, businessName string) (bool, string, error) {
	return false, "", nil
}
func (f fakeContactStore) PutContactName(ctx context.Context, user types.JID, fullName, firstName string) error {
	return nil
}
func (f fakeContactStore) PutAllContactNames(ctx context.Context, contacts []store.ContactEntry) error {
	return nil
}
func (f fakeContactStore) PutManyRedactedPhones(ctx context.Context, entries []store.RedactedPhoneEntry) error {
	return nil
}
func (f fakeContactStore) GetContact(ctx context.Context, user types.JID) (types.ContactInfo, error) {
	if user == f.known {
		return types.ContactInfo{Found: true, FullName: f.name}, nil
	}
	return types.ContactInfo{Found: false}, nil
}
func (f fakeContactStore) GetAllContacts(ctx context.Context) (map[types.JID]types.ContactInfo, error) {
	return nil, nil
}

func TestSetQuotedFields(t *testing.T) {
	ownJID := types.NewJID("111", types.DefaultUserServer)
	otherJID := types.NewJID("222", types.DefaultUserServer)

	newClient := func() *whatsmeow.Client {
		return whatsmeow.NewClient(&store.Device{
			ID:       &ownJID,
			Contacts: fakeContactStore{known: types.NewJID("222", types.HiddenUserServer), name: "Alice"},
		}, waLog.Noop)
	}

	t.Run("nil ContextInfo is a no-op", func(t *testing.T) {
		cm := chatMessage{ID: "m1"}
		setQuotedFields(context.Background(), newClient(), &cm, nil)
		if cm.QuotedID != "" || cm.QuotedType != "" {
			t.Fatalf("expected no quoted fields set, got %+v", cm)
		}
	})

	t.Run("blank stanza id is a no-op", func(t *testing.T) {
		cm := chatMessage{ID: "m1"}
		setQuotedFields(context.Background(), newClient(), &cm, &waE2E.ContextInfo{})
		if cm.QuotedID != "" || cm.QuotedType != "" {
			t.Fatalf("expected no quoted fields set, got %+v", cm)
		}
	})

	t.Run("reply to our own text message sets QuotedFromMe", func(t *testing.T) {
		cm := chatMessage{ID: "m1"}
		ci := &waE2E.ContextInfo{
			StanzaID:      proto.String("orig1"),
			Participant:   proto.String(ownJID.String()),
			QuotedMessage: &waE2E.Message{Conversation: proto.String("Sure, sounds good")},
		}
		setQuotedFields(context.Background(), newClient(), &cm, ci)
		if cm.QuotedID != "orig1" || !cm.QuotedFromMe || cm.QuotedSenderName != "" {
			t.Fatalf("got %+v", cm)
		}
		if cm.QuotedType != "text" || cm.QuotedText != "Sure, sounds good" {
			t.Fatalf("got QuotedType=%q QuotedText=%q", cm.QuotedType, cm.QuotedText)
		}
	})

	t.Run("reply to someone else's message resolves their name via the contact store", func(t *testing.T) {
		cm := chatMessage{ID: "m1"}
		ci := &waE2E.ContextInfo{
			StanzaID:      proto.String("orig2"),
			Participant:   proto.String(otherJID.String()),
			QuotedMessage: &waE2E.Message{ImageMessage: &waE2E.ImageMessage{Caption: proto.String("nice")}},
		}
		setQuotedFields(context.Background(), newClient(), &cm, ci)
		if cm.QuotedFromMe {
			t.Fatalf("expected QuotedFromMe=false, got %+v", cm)
		}
		if cm.QuotedSenderName != "Alice" {
			t.Fatalf("expected QuotedSenderName=Alice, got %q", cm.QuotedSenderName)
		}
		if cm.QuotedType != "image" || cm.QuotedText != "nice" {
			t.Fatalf("got QuotedType=%q QuotedText=%q", cm.QuotedType, cm.QuotedText)
		}
	})

	t.Run("quoted message with no embedded payload falls back to unsupported", func(t *testing.T) {
		cm := chatMessage{ID: "m1"}
		ci := &waE2E.ContextInfo{
			StanzaID:    proto.String("orig3"),
			Participant: proto.String(ownJID.String()),
		}
		setQuotedFields(context.Background(), newClient(), &cm, ci)
		if cm.QuotedType != "unsupported" || cm.QuotedText != "" {
			t.Fatalf("got QuotedType=%q QuotedText=%q", cm.QuotedType, cm.QuotedText)
		}
	})

	t.Run("quoted protocol message falls back to unsupported", func(t *testing.T) {
		cm := chatMessage{ID: "m1"}
		ci := &waE2E.ContextInfo{
			StanzaID:      proto.String("orig4"),
			Participant:   proto.String(ownJID.String()),
			QuotedMessage: &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{}},
		}
		setQuotedFields(context.Background(), newClient(), &cm, ci)
		if cm.QuotedType != "unsupported" {
			t.Fatalf("got QuotedType=%q", cm.QuotedType)
		}
	})

	t.Run("reply to a poll sets QuotedType=poll and the question as QuotedText", func(t *testing.T) {
		cm := chatMessage{ID: "m1"}
		ci := &waE2E.ContextInfo{
			StanzaID:    proto.String("orig5"),
			Participant: proto.String(ownJID.String()),
			QuotedMessage: &waE2E.Message{PollCreationMessageV3: &waE2E.PollCreationMessage{
				Name: proto.String("Best pizza topping?"),
			}},
		}
		setQuotedFields(context.Background(), newClient(), &cm, ci)
		if cm.QuotedType != "poll" || cm.QuotedText != "Best pizza topping?" {
			t.Fatalf("got QuotedType=%q QuotedText=%q", cm.QuotedType, cm.QuotedText)
		}
	})
}

func TestSetPollFields(t *testing.T) {
	cm := chatMessage{}
	poll := &waE2E.PollCreationMessage{
		Name: proto.String("Best pizza topping?"),
		Options: []*waE2E.PollCreationMessage_Option{
			{OptionName: proto.String("Pepperoni")},
			{OptionName: proto.String("Mushroom")},
			{OptionName: proto.String("Pineapple")},
		},
		SelectableOptionsCount: proto.Uint32(2),
	}
	setPollFields(&cm, poll)
	if len(cm.PollOptions) != 3 || cm.PollOptions[0] != "Pepperoni" || cm.PollOptions[2] != "Pineapple" {
		t.Fatalf("PollOptions = %+v", cm.PollOptions)
	}
	if cm.PollSelectableCount != 2 {
		t.Fatalf("PollSelectableCount = %d, want 2", cm.PollSelectableCount)
	}
}
