package main

import (
	"testing"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

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

func TestExtractMessage(t *testing.T) {
	t.Run("video message", func(t *testing.T) {
		msg := &waE2E.Message{VideoMessage: &waE2E.VideoMessage{Caption: proto.String("look")}}
		text, msgType, _, _, video, sticker, ok := extractMessage(msg)
		if !ok || msgType != "video" || text != "look" || video == nil || sticker != nil {
			t.Fatalf("extractMessage() = text=%q type=%q video=%v sticker=%v ok=%v", text, msgType, video, sticker, ok)
		}
	})

	t.Run("gif is a video message with GifPlayback set", func(t *testing.T) {
		msg := &waE2E.Message{VideoMessage: &waE2E.VideoMessage{GifPlayback: proto.Bool(true)}}
		_, msgType, _, _, video, _, ok := extractMessage(msg)
		if !ok || msgType != "gif" || video == nil {
			t.Fatalf("extractMessage() = type=%q video=%v ok=%v", msgType, video, ok)
		}
	})

	t.Run("sticker message", func(t *testing.T) {
		msg := &waE2E.Message{StickerMessage: &waE2E.StickerMessage{IsAnimated: proto.Bool(true)}}
		_, msgType, _, _, _, sticker, ok := extractMessage(msg)
		if !ok || msgType != "sticker" || sticker == nil {
			t.Fatalf("extractMessage() = type=%q sticker=%v ok=%v", msgType, sticker, ok)
		}
	})

	t.Run("lottie sticker falls back to unsupported", func(t *testing.T) {
		msg := &waE2E.Message{StickerMessage: &waE2E.StickerMessage{IsLottie: proto.Bool(true)}}
		text, msgType, _, _, _, sticker, ok := extractMessage(msg)
		if !ok || msgType != "unsupported" || text != "lottie sticker" || sticker != nil {
			t.Fatalf("extractMessage() = text=%q type=%q sticker=%v ok=%v", text, msgType, sticker, ok)
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
