package main

import (
	"testing"

	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
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
		if !changed {
			t.Fatal("expected changed = true")
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
		if changed {
			t.Fatal("expected changed = false, read must not downgrade")
		}
		if list[0].Status != "read" {
			t.Errorf("list[0].Status = %q, want %q", list[0].Status, "read")
		}
	})

	t.Run("message ID not found is a no-op", func(t *testing.T) {
		list := []chatMessage{{ID: "m1", FromMe: true, Status: "sent"}}
		changed := applyMessageStatus(list, []types.MessageID{"missing"}, "delivered")
		if changed {
			t.Fatal("expected changed = false")
		}
		if list[0].Status != "sent" {
			t.Errorf("list[0].Status = %q, want unchanged %q", list[0].Status, "sent")
		}
	})

	t.Run("ignores non-from-me messages even if ID matches", func(t *testing.T) {
		list := []chatMessage{{ID: "m1", FromMe: false, Status: ""}}
		changed := applyMessageStatus(list, []types.MessageID{"m1"}, "delivered")
		if changed {
			t.Fatal("expected changed = false")
		}
		if list[0].Status != "" {
			t.Errorf("list[0].Status should stay empty, got %q", list[0].Status)
		}
	})
}
