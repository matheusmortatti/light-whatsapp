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
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"

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
	Type string `json:"type"` // "qr" | "connected" | "logged_out" | "error" | "chats" | "messages" | "message_update" | "sync_status"
	Code string `json:"code,omitempty"`
	JID  string `json:"jid,omitempty"`
	// "messages" carries the chat's full list (open_chat replies only).
	// "message_update" carries just the messages that changed — one for a
	// download completion or a newly sent/received message, possibly several
	// for a status receipt covering multiple message IDs — for the app to
	// upsert into its already-held list instead of replacing it wholesale.
	Message  string        `json:"message,omitempty"`
	Chats    []chatSummary `json:"chats,omitempty"`
	Messages []chatMessage `json:"messages,omitempty"`
	// "sync_status" reports whether a burst of history-sync chunks is
	// currently in flight — see markHistorySyncActive. Absent (false)
	// covers the common case, so it's omitempty like everything else here.
	Syncing bool `json:"syncing,omitempty"`
}

// command is one line of the stdin protocol: the app asking core for
// something — "open_chat" to fetch (and start filling in images/audio for)
// one conversation's messages and mark it read, "close_chat" to tell core
// the app navigated away (see openChatJID), "send_message" to send a text
// message to a jid, or "send_audio" to upload and send a recorded voice
// message (AudioPath, relative to the working dir, plus its DurationMs).
type command struct {
	Type       string `json:"type"`
	JID        string `json:"jid,omitempty"`
	Text       string `json:"text,omitempty"`
	AudioPath  string `json:"audio_path,omitempty"`
	DurationMs int64  `json:"duration_ms,omitempty"`
}

// chatMessage is one message within a chat, as sent to the app. Only text,
// image, and audio messages are represented — everything else (documents,
// polls, ...) is dropped during extraction, per the app's scope. Reactions
// aren't their own chatMessage — see Reactions and handleReaction.
type chatMessage struct {
	ID        string `json:"id"`
	Timestamp int64  `json:"timestamp"`
	FromMe    bool   `json:"from_me"`

	// Sent -> delivered -> read status for a message *we* sent in a 1:1
	// chat, driven by handleSentMessageStatusReceipt. Never set for
	// incoming messages or group chats (WhatsApp only reports "read" once
	// every group member has read a message, which needs per-recipient
	// aggregation this app doesn't do yet). "" means unknown/not
	// applicable, not "unsent".
	Status string `json:"status,omitempty"` // "sent" | "delivered" | "read"

	Sender     string `json:"sender,omitempty"`      // sender JID, group chats only
	SenderName string `json:"sender_name,omitempty"` // best-effort display name, group chats only
	Type       string `json:"type"`                  // "text" | "image" | "audio"
	Text       string `json:"text,omitempty"`        // body, or an image's caption
	ImagePath  string `json:"image_path,omitempty"`  // path (relative to the working dir) once downloaded

	// Set only while an image message's ImagePath is still empty, so
	// downloadImage can (re)fetch it — including after a process restart,
	// since unlike an in-memory-only map this rides along in the on-disk
	// cache. Cleared once ImagePath is filled in. Harmless to also hand to
	// the app in the "messages" event; it just ignores unknown fields.
	ImageDirectPath    string `json:"image_direct_path,omitempty"`
	ImageMediaKey      []byte `json:"image_media_key,omitempty"`
	ImageFileSHA256    []byte `json:"image_file_sha256,omitempty"`
	ImageFileEncSHA256 []byte `json:"image_file_enc_sha256,omitempty"`
	ImageMimetype      string `json:"image_mimetype,omitempty"`

	AudioPath    string `json:"audio_path,omitempty"`    // path (relative to the working dir) once downloaded
	AudioSeconds uint32 `json:"audio_seconds,omitempty"` // duration, known up front unlike images

	// Same deal as the Image* fields above, but for audio (see downloadAudio).
	AudioDirectPath    string `json:"audio_direct_path,omitempty"`
	AudioMediaKey      []byte `json:"audio_media_key,omitempty"`
	AudioFileSHA256    []byte `json:"audio_file_sha256,omitempty"`
	AudioFileEncSHA256 []byte `json:"audio_file_enc_sha256,omitempty"`
	AudioMimetype      string `json:"audio_mimetype,omitempty"`

	VideoPath          string `json:"video_path,omitempty"`    // path (relative to the working dir) once downloaded
	VideoSeconds       uint32 `json:"video_seconds,omitempty"` // duration, known up front unlike images
	IsGif              bool   `json:"is_gif,omitempty"`        // true if this "video" is WhatsApp's GIF encoding (VideoMessage.GifPlayback)

	// Same deal as the Image*/Audio* fields above, but for video (see downloadVideo).
	VideoDirectPath    string `json:"video_direct_path,omitempty"`
	VideoMediaKey      []byte `json:"video_media_key,omitempty"`
	VideoFileSHA256    []byte `json:"video_file_sha256,omitempty"`
	VideoFileEncSHA256 []byte `json:"video_file_enc_sha256,omitempty"`
	VideoMimetype      string `json:"video_mimetype,omitempty"`

	StickerPath       string `json:"sticker_path,omitempty"`        // path (relative to the working dir) once downloaded
	StickerIsAnimated bool   `json:"sticker_is_animated,omitempty"` // WhatsApp stickers are usually static or animated WebP

	// Same deal again, but for stickers (see downloadSticker).
	StickerDirectPath    string `json:"sticker_direct_path,omitempty"`
	StickerMediaKey      []byte `json:"sticker_media_key,omitempty"`
	StickerFileSHA256    []byte `json:"sticker_file_sha256,omitempty"`
	StickerFileEncSHA256 []byte `json:"sticker_file_enc_sha256,omitempty"`
	StickerMimetype      string `json:"sticker_mimetype,omitempty"`

	// Reactions on this message, received live (see handleReaction) — never
	// set for a message this app sends, since sending a reaction isn't
	// supported.
	Reactions []chatReaction `json:"reactions,omitempty"`
}

// chatReaction is one person's current reaction to a message, keyed by
// Sender so a later reaction from the same person replaces (or, with Emoji
// == "", removes) their earlier one — WhatsApp allows exactly one active
// reaction per person per message.
type chatReaction struct {
	Sender     string `json:"sender"`
	SenderName string `json:"sender_name,omitempty"` // best-effort display name, group chats only
	FromMe     bool   `json:"from_me,omitempty"`
	Emoji      string `json:"emoji"`
}

// chatSummary is one entry of a "chats" event: a single conversation as
// reconstructed from WhatsApp's history-sync payload.
type chatSummary struct {
	JID         string `json:"jid"`
	Name        string `json:"name"`
	Timestamp   int64  `json:"timestamp"` // unix seconds of the last message, 0 if unknown
	UnreadCount int    `json:"unread_count"`
	IsGroup     bool   `json:"is_group"`
}

// emitMu serializes stdout writes: history-sync chunks are dispatched from a
// dedicated goroutine (see whatsmeow's handleHistorySyncNotificationLoop),
// separate from the goroutine that dispatches Connected/LoggedOut/QR events,
// so two emits could otherwise race on the same underlying Write.
var emitMu sync.Mutex

// chatsMu guards the chats map: handleHistorySync runs on whatsmeow's
// dedicated history-sync goroutine, while fetchGroupNames runs on whatever
// goroutine dispatches *events.Connected — both mutate the same map.
var chatsMu sync.Mutex

// mediaDownloadSem caps how many media downloads — image, audio, video, or
// sticker — run concurrently across all four types. handleOpenChat
// dispatches one goroutine per undownloaded item in a newly opened chat
// with no limit of its own; a chat with a large backlog would otherwise
// open that many simultaneous HTTPS downloads at once on core's own
// hardware (the Light Phone III itself). Acquired/released inside
// downloadMedia itself so the cap is self-contained there.
var mediaDownloadSem = make(chan struct{}, 3)

// openChatJID is the chat the app currently has on screen, set by
// handleOpenChat and cleared by a "close_chat" command (see readCommands).
// handleMessage consults it so a live message for the chat the user is
// actively looking at doesn't bump its unread count.
var (
	openChatMu  sync.Mutex
	openChatJID string
)

func setOpenChatJID(jid string) {
	openChatMu.Lock()
	openChatJID = jid
	openChatMu.Unlock()
}

func clearOpenChatJID(jid string) {
	openChatMu.Lock()
	if openChatJID == jid {
		openChatJID = ""
	}
	openChatMu.Unlock()
}

func isOpenChatJID(jid string) bool {
	openChatMu.Lock()
	defer openChatMu.Unlock()
	return openChatJID == jid
}

// syncMu guards syncing and syncTimer for the "sync_status" event: history-
// sync chunks arrive in a burst (bootstrap, recent, push-name, ...) with
// gaps between them, so rather than toggling the app's indicator on every
// single chunk, syncing flips true on the first chunk of a burst and a
// debounce timer flips it back false historySyncIdleDelay after the last
// one — smoothing over the gaps instead of flickering.
var (
	syncMu    sync.Mutex
	syncing   bool
	syncTimer *time.Timer
)

const historySyncIdleDelay = 2 * time.Second

// markHistorySyncActive is called on every history-sync chunk, including
// ones with no conversations (push-name/non-blocking-data chunks still
// count as sync activity for the app's indicator).
func markHistorySyncActive() {
	syncMu.Lock()
	defer syncMu.Unlock()
	if !syncing {
		syncing = true
		emit(event{Type: "sync_status", Syncing: true})
	}
	if syncTimer != nil {
		syncTimer.Stop()
	}
	syncTimer = time.AfterFunc(historySyncIdleDelay, markHistorySyncIdle)
}

func markHistorySyncIdle() {
	syncMu.Lock()
	defer syncMu.Unlock()
	if syncing {
		syncing = false
		emit(event{Type: "sync_status", Syncing: false})
	}
}

func emit(e event) {
	emitMu.Lock()
	defer emitMu.Unlock()
	// Errors here would mean stdout is gone (pipe closed by the
	// supervising process) — nothing useful to do but ignore it.
	_ = json.NewEncoder(os.Stdout).Encode(e)
}

// chatsFile persists the last-known chat list next to whatsapp.db so a
// reconnect (no fresh history sync) still has something to show immediately.
const chatsFile = "chats.json"

func loadCachedChats() map[string]chatSummary {
	chats := make(map[string]chatSummary)
	data, err := os.ReadFile(chatsFile)
	if err != nil {
		return chats
	}
	var list []chatSummary
	if err := json.Unmarshal(data, &list); err != nil {
		return chats
	}
	for _, c := range list {
		chats[c.JID] = c
	}
	return chats
}

// saveChats writes the current chat set to disk and returns it as a
// timestamp-descending slice, ready to emit.
func saveChats(chats map[string]chatSummary) []chatSummary {
	list := make([]chatSummary, 0, len(chats))
	for _, c := range chats {
		list = append(list, c)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Timestamp > list[j].Timestamp })
	if data, err := json.Marshal(list); err == nil {
		_ = os.WriteFile(chatsFile, data, 0o600)
	}
	return list
}

// messagesMu guards both messages (jid -> that chat's messages, oldest
// first) and pendingImages (jid -> message ID -> that message's still-to-be-
// downloaded image) — always touched together, so one mutex covers both.
// Populated from the same two goroutines as chatsMu: history-sync chunks
// and live *events.Message.
var messagesMu sync.Mutex

// messagesDir holds one cached JSON file per chat (its message list),
// mirroring chatsFile's "survive a restart without a fresh sync" role.
const messagesDir = "messages"

// maxMessagesPerChat bounds how much scrollback is kept in memory/disk per
// chat — plenty for a phone screen, small enough to stay cheap across
// hundreds of chats.
const maxMessagesPerChat = 500

func messagesFilePath(jid string) string {
	return filepath.Join(messagesDir, jid+".json")
}

func loadCachedMessages(jid string) []chatMessage {
	data, err := os.ReadFile(messagesFilePath(jid))
	if err != nil {
		return nil
	}
	var list []chatMessage
	if err := json.Unmarshal(data, &list); err != nil {
		return nil
	}
	return list
}

func saveMessages(jid string, list []chatMessage) {
	if err := os.MkdirAll(messagesDir, 0o700); err != nil {
		return
	}
	if data, err := json.Marshal(list); err == nil {
		_ = os.WriteFile(messagesFilePath(jid), data, 0o600)
	}
}

// upsertMessage inserts or replaces msg in messages[jid] (keyed by message
// ID, so a history-sync replay of an already-seen message updates it in
// place instead of duplicating it), keeps the slice sorted oldest-first,
// trims it to maxMessagesPerChat, persists it, and returns it. Caller must
// hold messagesMu.
func upsertMessage(messages map[string][]chatMessage, jid string, msg chatMessage) []chatMessage {
	list, ok := messages[jid]
	if !ok {
		list = loadCachedMessages(jid)
	}
	replaced := false
	for i, m := range list {
		if m.ID == msg.ID {
			list[i] = msg
			replaced = true
			break
		}
	}
	if !replaced {
		list = append(list, msg)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Timestamp < list[j].Timestamp })
	if len(list) > maxMessagesPerChat {
		list = list[len(list)-maxMessagesPerChat:]
	}
	messages[jid] = list
	saveMessages(jid, list)
	return list
}

// extractMessage pulls the text (or image) content out of a WhatsApp
// message, unwrapping the ephemeral/view-once wrappers disappearing
// messages arrive in. Anything else (video, documents, stickers, polls,
// reactions, location, ...) the app doesn't render comes back as msgType
// "unsupported" with a human-readable label in text (see
// unsupportedMessageLabel) rather than being dropped — the app shows it as
// "Unsupported message: <label>" so at least its arrival is visible, even
// though its content isn't.
func extractMessage(m *waE2E.Message) (text, msgType string, img *waE2E.ImageMessage, audio *waE2E.AudioMessage, video *waE2E.VideoMessage, sticker *waE2E.StickerMessage, ok bool) {
	for i := 0; i < 4 && m != nil; i++ {
		switch {
		case m.GetConversation() != "":
			return m.GetConversation(), "text", nil, nil, nil, nil, true
		case m.GetExtendedTextMessage() != nil:
			return m.GetExtendedTextMessage().GetText(), "text", nil, nil, nil, nil, true
		case m.GetImageMessage() != nil:
			im := m.GetImageMessage()
			return im.GetCaption(), "image", im, nil, nil, nil, true
		case m.GetAudioMessage() != nil:
			return "", "audio", nil, m.GetAudioMessage(), nil, nil, true
		case m.GetVideoMessage() != nil:
			vm := m.GetVideoMessage()
			vType := "video"
			if vm.GetGifPlayback() {
				vType = "gif"
			}
			return vm.GetCaption(), vType, nil, nil, vm, nil, true
		case m.GetStickerMessage() != nil && !m.GetStickerMessage().GetIsLottie():
			return "", "sticker", nil, nil, nil, m.GetStickerMessage(), true
		case m.GetEphemeralMessage() != nil:
			m = m.GetEphemeralMessage().GetMessage()
		case m.GetViewOnceMessage() != nil:
			m = m.GetViewOnceMessage().GetMessage()
		case m.GetViewOnceMessageV2() != nil:
			m = m.GetViewOnceMessageV2().GetMessage()
		default:
			return unsupportedMessageLabel(m), "unsupported", nil, nil, nil, nil, true
		}
	}
	return "", "", nil, nil, nil, nil, false
}

// unsupportedMessageLabel names whichever content field is populated on m,
// for display when none of extractMessage's known cases match. waE2E.Message
// has ~100 possible content fields (video, document, sticker, poll,
// location, reaction, ...) covering every WhatsApp message type; walking
// them via reflection here means new message types WhatsApp adds show up
// with a sensible label automatically, instead of this needing a matching
// hardcoded case added by hand.
func unsupportedMessageLabel(m *waE2E.Message) string {
	if m == nil {
		return "message"
	}
	// WhatsApp sends GIFs as a VideoMessage with GifPlayback set — there's no
	// separate "gif" content field to catch via the generic field walk below.
	if vm := m.GetVideoMessage(); vm != nil && vm.GetGifPlayback() {
		return "gif"
	}
	if sm := m.GetStickerMessage(); sm != nil && sm.GetIsLottie() {
		return "lottie sticker"
	}
	v := reflect.ValueOf(m).Elem()
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		if !field.IsExported() {
			continue
		}
		switch field.Name {
		case "MessageContextInfo", "SenderKeyDistributionMessage", "FastRatchetKeySenderKeyDistributionMessage":
			continue
		}
		fv := v.Field(i)
		if fv.Kind() != reflect.Pointer || fv.IsNil() {
			continue
		}
		return humanizeFieldName(strings.TrimSuffix(field.Name, "Message"))
	}
	return "message"
}

// humanizeFieldName turns a Go struct field name like "GroupInvite" or
// "PollCreationV3" into "group invite"/"poll creation v3" for display.
func humanizeFieldName(name string) string {
	if name == "" {
		return "message"
	}
	var b strings.Builder
	for i, r := range name {
		if i > 0 && r >= 'A' && r <= 'Z' {
			b.WriteByte(' ')
		}
		b.WriteRune(r)
	}
	return strings.ToLower(b.String())
}

// mentionPattern matches WhatsApp's raw @-mention syntax embedded directly in
// message text: "@" followed by the mentioned JID's bare user part (digits
// only, no "+", no domain). Resolved at emit time (see resolveMentionsInList)
// rather than baked into the cached Text at ingest time, so a message cached
// by an older build (raw number, no resolution logic yet) still resolves
// correctly today — the source of truth is the text itself, not a
// separately-persisted mention list.
var mentionPattern = regexp.MustCompile(`@(\d{5,15})\b`)

// resolveMentions rewrites each "@<number>" mention in text into "@<display
// name>", using the same contact-store lookup as chat names. The number is
// the mentioned user's bare JID user part, but *not* necessarily a phone
// number: WhatsApp mentions people by their privacy-preserving "LID" (a
// per-account opaque ID, server "lid") rather than their phone number
// (server "s.whatsapp.net") wherever LID addressing is in effect for the
// chat, so LID is tried first — confirmed via the local contact store
// (`whatsmeow_contacts`, keyed by the full "<id>@lid" JID, PushName field)
// rather than assumed. Falls back to the phone-number JID, then to the
// device's own push name for a self-mention (neither JID form necessarily
// has a contact-store row for yourself). Left as-is (raw number) if no name
// is known yet.
func resolveMentions(ctx context.Context, client *whatsmeow.Client, text string) string {
	if !strings.Contains(text, "@") {
		return text
	}
	return mentionPattern.ReplaceAllStringFunc(text, func(match string) string {
		if name := cachedMentionName(ctx, client, match[1:]); name != "" {
			return "@" + name
		}
		return match
	})
}

// mentionNameMu guards mentionNameCache.
var mentionNameMu sync.Mutex

// mentionNameCache memoizes lookupMentionName by the mentioned user's bare
// JID part, since resolveMentionsInList re-resolves every message's mentions
// on every emit (see its own comment) and each miss costs up to two SQLite
// round-trips. Only successful lookups are cached — a "" result is left
// unmemoized so a contact that resolves later (address-book sync, a Contact/
// PushName event) is picked up on the next mention rather than staying
// stuck at the raw number. Entries are dropped by invalidateMentionName when
// a Contact/PushName event says a name changed.
var mentionNameCache = make(map[string]string)

func cachedMentionName(ctx context.Context, client *whatsmeow.Client, user string) string {
	mentionNameMu.Lock()
	name, ok := mentionNameCache[user]
	mentionNameMu.Unlock()
	if ok {
		return name
	}

	name = lookupMentionName(ctx, client, user)
	if name == "" {
		return ""
	}

	mentionNameMu.Lock()
	mentionNameCache[user] = name
	mentionNameMu.Unlock()
	return name
}

// invalidateMentionName drops user's cached mention name so the next mention
// of them re-resolves against the contact store, rather than keeping a
// stale name after a Contact/PushName event.
func invalidateMentionName(user string) {
	mentionNameMu.Lock()
	delete(mentionNameCache, user)
	mentionNameMu.Unlock()
}

// lookupMentionName is resolveMentions' uncached lookup — see its comment
// for why LID is tried first, then phone number, then self push name.
func lookupMentionName(ctx context.Context, client *whatsmeow.Client, user string) string {
	if name := contactName(ctx, client, types.NewJID(user, types.HiddenUserServer)); name != "" {
		return name
	}
	if name := contactName(ctx, client, types.NewJID(user, types.DefaultUserServer)); name != "" {
		return name
	}
	if client.Store.PushName != "" && ((client.Store.ID != nil && user == client.Store.ID.User) || user == client.Store.GetLID().User) {
		return client.Store.PushName
	}
	return ""
}

// resolveMentionsInList returns a copy of list with each message's mentions
// resolved to display names (see resolveMentions) — a copy so the underlying
// cache keeps the raw, unresolved text (resolution depends on contact-store
// state that can improve over time, so it's redone on every emit rather than
// baked in once).
func resolveMentionsInList(ctx context.Context, client *whatsmeow.Client, list []chatMessage) []chatMessage {
	if len(list) == 0 {
		return list
	}
	out := make([]chatMessage, len(list))
	for i, m := range list {
		m.Text = resolveMentions(ctx, client, m.Text)
		out[i] = m
	}
	return out
}

// setImageFields fills in cm's persisted download reference from img, so
// downloadImage can (re)fetch it later — including from a process started
// after the one that originally saw this message, since these fields ride
// along in the on-disk message cache rather than living only in memory.
func setImageFields(cm *chatMessage, img *waE2E.ImageMessage) {
	cm.ImageDirectPath = img.GetDirectPath()
	cm.ImageMediaKey = img.GetMediaKey()
	cm.ImageFileSHA256 = img.GetFileSHA256()
	cm.ImageFileEncSHA256 = img.GetFileEncSHA256()
	cm.ImageMimetype = img.GetMimetype()
}

// setAudioFields is setImageFields' counterpart for audio messages — fills
// in cm's persisted download reference from audio, plus its duration (known
// up front, unlike an image's dimensions/size).
func setAudioFields(cm *chatMessage, audio *waE2E.AudioMessage) {
	cm.AudioDirectPath = audio.GetDirectPath()
	cm.AudioMediaKey = audio.GetMediaKey()
	cm.AudioFileSHA256 = audio.GetFileSHA256()
	cm.AudioFileEncSHA256 = audio.GetFileEncSHA256()
	cm.AudioMimetype = audio.GetMimetype()
	cm.AudioSeconds = audio.GetSeconds()
}

// setVideoFields is setImageFields' counterpart for video (and GIF, which
// WhatsApp encodes as a VideoMessage with GifPlayback set — see
// extractMessage) — fills in cm's persisted download reference from v, plus
// its duration and whether it's a GIF.
func setVideoFields(cm *chatMessage, v *waE2E.VideoMessage) {
	cm.VideoDirectPath = v.GetDirectPath()
	cm.VideoMediaKey = v.GetMediaKey()
	cm.VideoFileSHA256 = v.GetFileSHA256()
	cm.VideoFileEncSHA256 = v.GetFileEncSHA256()
	cm.VideoMimetype = v.GetMimetype()
	cm.VideoSeconds = v.GetSeconds()
	cm.IsGif = v.GetGifPlayback()
}

// setStickerFields is setImageFields' counterpart for stickers — fills in
// cm's persisted download reference from s, plus whether it's animated.
// Lottie stickers never reach here (see extractMessage).
func setStickerFields(cm *chatMessage, s *waE2E.StickerMessage) {
	cm.StickerDirectPath = s.GetDirectPath()
	cm.StickerMediaKey = s.GetMediaKey()
	cm.StickerFileSHA256 = s.GetFileSHA256()
	cm.StickerFileEncSHA256 = s.GetFileEncSHA256()
	cm.StickerMimetype = s.GetMimetype()
	cm.StickerIsAnimated = s.GetIsAnimated()
}

// extractHistoryMessage pulls one history-sync message into messages, if it's
// a type the app renders (see extractMessage). Image media is noted but not
// downloaded here — history sync can carry years of backlog, so downloading
// is deferred to handleOpenChat, on demand.
func extractHistoryMessage(ctx context.Context, client *whatsmeow.Client, jid types.JID, info *waWeb.WebMessageInfo, messages map[string][]chatMessage) {
	if info == nil {
		return
	}
	key := info.GetKey()
	waMsg := info.GetMessage()
	if waMsg == nil || key.GetID() == "" {
		return
	}
	text, msgType, img, audio, video, sticker, ok := extractMessage(waMsg)
	if !ok {
		return
	}

	cm := chatMessage{
		ID:        key.GetID(),
		Timestamp: int64(info.GetMessageTimestamp()),
		FromMe:    key.GetFromMe(),
		Type:      msgType,
		Text:      text,
	}
	if msgType == "image" && img != nil {
		setImageFields(&cm, img)
	}
	if msgType == "audio" && audio != nil {
		setAudioFields(&cm, audio)
	}
	if (msgType == "video" || msgType == "gif") && video != nil {
		setVideoFields(&cm, video)
	}
	if msgType == "sticker" && sticker != nil {
		setStickerFields(&cm, sticker)
	}
	// A message we sent from a different linked device (e.g. the phone)
	// arrives here with no status of its own — handleSendMessage/
	// handleSendAudio only stamp "sent" for messages sent through *this*
	// app. History sync carries the real delivery/read state WhatsApp
	// already tracked for it, so use that instead of leaving it blank.
	if key.GetFromMe() && jid.Server != types.GroupServer {
		if s := webMessageInfoStatus(info.GetStatus()); s != "" {
			cm.Status = s
		}
	}
	if jid.Server == types.GroupServer && !key.GetFromMe() {
		participant := key.GetParticipant()
		if participant == "" {
			participant = info.GetParticipant()
		}
		if participant != "" {
			cm.Sender = participant
			if pjid, err := types.ParseJID(participant); err == nil {
				if contact, err := client.Store.Contacts.GetContact(ctx, pjid); err == nil && contact.Found {
					switch {
					case contact.FullName != "":
						cm.SenderName = contact.FullName
					case contact.PushName != "":
						cm.SenderName = contact.PushName
					}
				}
			}
		}
	}

	messagesMu.Lock()
	upsertMessage(messages, jid.String(), cm)
	messagesMu.Unlock()
}

// imageExtension maps a media mimetype to a file extension; WhatsApp images
// are almost always JPEG, so that's the fallback for anything unrecognized.
func imageExtension(mimetype string) string {
	switch mimetype {
	case "image/png":
		return "png"
	case "image/webp":
		return "webp"
	default:
		return "jpg"
	}
}

func imagePath(jid, msgID, mimetype string) string {
	return filepath.Join("media", jid, msgID+"."+imageExtension(mimetype))
}

// downloadMedia fetches one message's media, streaming straight to disk via
// DownloadMediaWithPathToFile rather than buffering the whole decrypted
// file in memory first — core runs on the Light Phone III itself, and a
// pile of simultaneous multi-MB in-memory buffers is a real OOM risk.
// mediaDownloadSem bounds how many of these run concurrently, regardless of
// type, since handleOpenChat can dispatch one goroutine per undownloaded
// item in a chat with no limit of its own. Once the file lands, apply is
// called with a pointer into the stored chatMessage to fill in the
// type-specific path and clear that type's now-unneeded key material, and a
// message_update is emitted for it. Meant to run in its own goroutine —
// image/audio/video/sticker messages show up placeholder-only until this
// completes.
func downloadMedia(
	ctx context.Context,
	client *whatsmeow.Client,
	logger waLog.Logger,
	messages map[string][]chatMessage,
	jid string,
	m chatMessage,
	kind string,
	mediaType whatsmeow.MediaType,
	directPath string,
	fileEncSHA256, fileSHA256, mediaKey []byte,
	path string,
	apply func(cm *chatMessage, path string),
) {
	mediaDownloadSem <- struct{}{}
	defer func() { <-mediaDownloadSem }()

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		logger.Warnf("failed to create media dir for %s: %v", jid, err)
		return
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		logger.Warnf("failed to create %s file %s/%s: %v", kind, jid, m.ID, err)
		return
	}
	err = client.DownloadMediaWithPathToFile(ctx, directPath, fileEncSHA256, fileSHA256, mediaKey, mediaType, "", false, f)
	closeErr := f.Close()
	if err != nil {
		logger.Warnf("failed to download %s %s/%s: %v", kind, jid, m.ID, err)
		_ = os.Remove(path)
		return
	}
	if closeErr != nil {
		logger.Warnf("failed to close %s file %s/%s: %v", kind, jid, m.ID, closeErr)
		_ = os.Remove(path)
		return
	}

	messagesMu.Lock()
	list, ok := messages[jid]
	var updated chatMessage
	found := false
	if ok {
		for i, cur := range list {
			if cur.ID == m.ID {
				apply(&list[i], path)
				updated = list[i]
				found = true
				break
			}
		}
		messages[jid] = list
		saveMessages(jid, list)
	}
	messagesMu.Unlock()

	if found {
		emit(event{Type: "message_update", JID: jid, Messages: resolveMentionsInList(ctx, client, []chatMessage{updated})})
	}
}

// downloadImage fetches an image message's media (using the persisted
// download reference set by setImageFields, not the original *waE2E.
// ImageMessage — that doesn't survive a process restart, this does). See
// downloadMedia.
func downloadImage(ctx context.Context, client *whatsmeow.Client, logger waLog.Logger, messages map[string][]chatMessage, jid string, m chatMessage) {
	path := imagePath(jid, m.ID, m.ImageMimetype)
	downloadMedia(ctx, client, logger, messages, jid, m, "image", whatsmeow.MediaImage,
		m.ImageDirectPath, m.ImageFileEncSHA256, m.ImageFileSHA256, m.ImageMediaKey, path,
		func(cm *chatMessage, path string) {
			cm.ImagePath = path
			cm.ImageDirectPath = ""
			cm.ImageMediaKey = nil
			cm.ImageFileSHA256 = nil
			cm.ImageFileEncSHA256 = nil
			cm.ImageMimetype = ""
		})
}

// audioExtension maps a media mimetype to a file extension. WhatsApp voice
// notes are normally Opus-in-Ogg, but this app's own recordings (see
// handleSendAudio) are AAC-in-MPEG-4, so both need to round-trip cleanly.
func audioExtension(mimetype string) string {
	switch {
	case strings.Contains(mimetype, "ogg"):
		return "ogg"
	case strings.Contains(mimetype, "amr"):
		return "amr"
	case strings.Contains(mimetype, "mpeg"), strings.Contains(mimetype, "mp3"):
		return "mp3"
	case strings.Contains(mimetype, "wav"):
		return "wav"
	default:
		return "m4a"
	}
}

func audioPath(jid, msgID, mimetype string) string {
	return filepath.Join("media", jid, msgID+"."+audioExtension(mimetype))
}

// downloadAudio is downloadImage's counterpart for audio messages: fetches
// the media using the persisted download reference set by setAudioFields.
// See downloadMedia.
func downloadAudio(ctx context.Context, client *whatsmeow.Client, logger waLog.Logger, messages map[string][]chatMessage, jid string, m chatMessage) {
	path := audioPath(jid, m.ID, m.AudioMimetype)
	downloadMedia(ctx, client, logger, messages, jid, m, "audio", whatsmeow.MediaAudio,
		m.AudioDirectPath, m.AudioFileEncSHA256, m.AudioFileSHA256, m.AudioMediaKey, path,
		func(cm *chatMessage, path string) {
			cm.AudioPath = path
			cm.AudioDirectPath = ""
			cm.AudioMediaKey = nil
			cm.AudioFileSHA256 = nil
			cm.AudioFileEncSHA256 = nil
			cm.AudioMimetype = ""
		})
}

// videoExtension maps a media mimetype to a file extension; WhatsApp videos
// (and GIFs, encoded as videos — see extractMessage) are almost always
// MP4/H.264, so that's the fallback for anything unrecognized.
func videoExtension(mimetype string) string {
	switch {
	case strings.Contains(mimetype, "3gpp"):
		return "3gp"
	default:
		return "mp4"
	}
}

func videoPath(jid, msgID, mimetype string) string {
	return filepath.Join("media", jid, msgID+"."+videoExtension(mimetype))
}

// downloadVideo is downloadImage's counterpart for video/GIF messages:
// fetches the media using the persisted download reference set by
// setVideoFields. See downloadMedia.
func downloadVideo(ctx context.Context, client *whatsmeow.Client, logger waLog.Logger, messages map[string][]chatMessage, jid string, m chatMessage) {
	path := videoPath(jid, m.ID, m.VideoMimetype)
	downloadMedia(ctx, client, logger, messages, jid, m, "video", whatsmeow.MediaVideo,
		m.VideoDirectPath, m.VideoFileEncSHA256, m.VideoFileSHA256, m.VideoMediaKey, path,
		func(cm *chatMessage, path string) {
			cm.VideoPath = path
			cm.VideoDirectPath = ""
			cm.VideoMediaKey = nil
			cm.VideoFileSHA256 = nil
			cm.VideoFileEncSHA256 = nil
			cm.VideoMimetype = ""
		})
}

// stickerExtension maps a media mimetype to a file extension; WhatsApp
// stickers are almost always WebP (static or animated).
func stickerExtension(mimetype string) string {
	switch mimetype {
	case "image/png":
		return "png"
	default:
		return "webp"
	}
}

func stickerPath(jid, msgID, mimetype string) string {
	return filepath.Join("media", jid, msgID+"."+stickerExtension(mimetype))
}

// downloadSticker is downloadImage's counterpart for sticker messages.
// Stickers download as whatsmeow.MediaImage (confirmed via whatsmeow's
// classToMediaType map in download.go — there's no separate sticker media
// type). See downloadMedia.
func downloadSticker(ctx context.Context, client *whatsmeow.Client, logger waLog.Logger, messages map[string][]chatMessage, jid string, m chatMessage) {
	path := stickerPath(jid, m.ID, m.StickerMimetype)
	downloadMedia(ctx, client, logger, messages, jid, m, "sticker", whatsmeow.MediaImage,
		m.StickerDirectPath, m.StickerFileEncSHA256, m.StickerFileSHA256, m.StickerMediaKey, path,
		func(cm *chatMessage, path string) {
			cm.StickerPath = path
			cm.StickerDirectPath = ""
			cm.StickerMediaKey = nil
			cm.StickerFileSHA256 = nil
			cm.StickerFileEncSHA256 = nil
			cm.StickerMimetype = ""
		})
}

// markChatRead sends WhatsApp read receipts for every not-from-me message in
// list, so the sender sees the chat as read the same way a phone client
// would. Receipts must be grouped by sender (MarkRead's requirement — see
// whatsmeow/receipt.go), which only matters for groups: 1:1 chats have a
// single implicit sender and an empty types.JID is passed instead.
func markChatRead(ctx context.Context, client *whatsmeow.Client, logger waLog.Logger, jid types.JID, list []chatMessage) {
	bySender := make(map[string][]string)
	for _, m := range list {
		if m.FromMe {
			continue
		}
		bySender[m.Sender] = append(bySender[m.Sender], m.ID)
	}
	for senderStr, ids := range bySender {
		sender := types.EmptyJID
		if senderStr != "" {
			parsed, err := types.ParseJID(senderStr)
			if err != nil {
				continue
			}
			sender = parsed
		}
		if err := client.MarkRead(ctx, ids, time.Now(), jid, sender); err != nil {
			logger.Warnf("failed to mark %d message(s) read in %s: %v", len(ids), jid, err)
		}
	}
}

// unreadTail returns the last count not-from-me messages in list (oldest
// first, same as list itself), walking backward from the newest message —
// i.e. the actual unread messages, per chats[jid].UnreadCount, rather than
// the whole cached scrollback. Without this, reopening a long-running chat
// would resend read receipts for its entire history every single time.
func unreadTail(list []chatMessage, count int) []chatMessage {
	if count <= 0 {
		return nil
	}
	out := make([]chatMessage, 0, count)
	for i := len(list) - 1; i >= 0 && len(out) < count; i-- {
		if !list[i].FromMe {
			out = append(out, list[i])
		}
	}
	return out
}

// markChatReadAndClearUnread marks the chat's actually-unread messages read
// (see unreadTail) and, if its unread count was nonzero, resets it to 0 and
// emits the updated chat list. Meant to run in its own goroutine — the read
// receipt is a network call.
func markChatReadAndClearUnread(ctx context.Context, client *whatsmeow.Client, logger waLog.Logger, jidStr string, list []chatMessage, chats map[string]chatSummary) {
	jid, err := types.ParseJID(jidStr)
	if err != nil {
		return
	}

	chatsMu.Lock()
	c, ok := chats[jidStr]
	if !ok || c.UnreadCount == 0 {
		chatsMu.Unlock()
		return
	}
	unreadCount := c.UnreadCount
	c.UnreadCount = 0
	chats[jidStr] = c
	updated := saveChats(chats)
	chatsMu.Unlock()

	markChatRead(ctx, client, logger, jid, unreadTail(list, unreadCount))
	emit(event{Type: "chats", Chats: updated})
}

// handleReadReceipt reacts to a *events.Receipt telling us WhatsApp's own
// sync mechanism, not the app, marked a chat read: when another linked
// device opens a chat, WhatsApp sends every other device (including this
// one) a receipt from our own JID with ReceiptTypeRead (or ReceiptTypeSelf
// if the account has read receipts disabled) and the chat that was read in
// the "recipient" attribute — surfaced by whatsmeow as source.Chat with
// source.IsFromMe set (see parseMessageSource). Receipts for messages we
// sent being read by the other party look the same shape-wise except
// IsFromMe is false, so that's the filter that keeps this from firing on
// every incoming read receipt. No MarkRead call here — the other device
// already told the server, so this only needs to update local state.
func handleReadReceipt(ctx context.Context, client *whatsmeow.Client, evt *events.Receipt, chats map[string]chatSummary) {
	if !evt.MessageSource.IsFromMe || (evt.Type != types.ReceiptTypeRead && evt.Type != types.ReceiptTypeReadSelf) {
		return
	}
	jid := canonicalizeChatJID(ctx, client, evt.MessageSource.Chat)
	jidStr := jid.String()

	chatsMu.Lock()
	c, ok := chats[jidStr]
	if !ok || c.UnreadCount == 0 {
		chatsMu.Unlock()
		return
	}
	c.UnreadCount = 0
	chats[jidStr] = c
	updated := saveChats(chats)
	chatsMu.Unlock()

	emit(event{Type: "chats", Chats: updated})
}

// webMessageInfoStatus maps a history-synced message's own WebMessageInfo.Status
// (WhatsApp's real per-message delivery/read state, carried in the sync
// payload itself) to the chatMessage.Status it implies — "" for PENDING/ERROR
// (not yet meaningfully sent) or any status this app doesn't track.
func webMessageInfoStatus(status waWeb.WebMessageInfo_Status) string {
	switch status {
	case waWeb.WebMessageInfo_SERVER_ACK:
		return "sent"
	case waWeb.WebMessageInfo_DELIVERY_ACK:
		return "delivered"
	case waWeb.WebMessageInfo_READ, waWeb.WebMessageInfo_PLAYED:
		return "read"
	default:
		return ""
	}
}

// receiptStatusFor maps evt to the chatMessage.Status it implies for a
// message *we* sent, or "" if this receipt doesn't carry that (it's the
// self-read-sync case handleReadReceipt already handles, a group chat — see
// chatMessage.Status — or a receipt type this app doesn't track).
func receiptStatusFor(evt *events.Receipt) string {
	if evt.MessageSource.IsFromMe || evt.MessageSource.IsGroup {
		return ""
	}
	switch evt.Type {
	case types.ReceiptTypeDelivered:
		return "delivered"
	case types.ReceiptTypeRead:
		return "read"
	default:
		return ""
	}
}

// applyMessageStatus sets Status to status, in place, on every message in
// list whose ID is in ids and is FromMe — except it never downgrades an
// existing "read" back to "delivered" (a delivered receipt can in
// principle arrive after a read one, out of order). Returns the messages it
// changed, for the caller to emit as an incremental update.
func applyMessageStatus(list []chatMessage, ids []types.MessageID, status string) []chatMessage {
	idSet := make(map[types.MessageID]bool, len(ids))
	for _, id := range ids {
		idSet[id] = true
	}
	var changed []chatMessage
	for i, m := range list {
		if !m.FromMe || !idSet[m.ID] || m.Status == "read" {
			continue
		}
		list[i].Status = status
		changed = append(changed, list[i])
	}
	return changed
}

// handleSentMessageStatusReceipt reacts to a *events.Receipt telling us the
// actual recipient (not our own read-sync — see handleReadReceipt) received
// or read a message we sent, updating that message's Status and emitting a
// message_update so the UI updates live.
func handleSentMessageStatusReceipt(ctx context.Context, client *whatsmeow.Client, evt *events.Receipt, messages map[string][]chatMessage) {
	status := receiptStatusFor(evt)
	if status == "" {
		return
	}
	jidStr := canonicalizeChatJID(ctx, client, evt.MessageSource.Chat).String()

	messagesMu.Lock()
	list, ok := messages[jidStr]
	if !ok {
		list = loadCachedMessages(jidStr)
		if len(list) == 0 {
			messagesMu.Unlock()
			return
		}
		messages[jidStr] = list
	}
	changed := applyMessageStatus(list, evt.MessageIDs, status)
	if len(changed) > 0 {
		saveMessages(jidStr, list)
	}
	messagesMu.Unlock()

	if len(changed) > 0 {
		emit(event{Type: "message_update", JID: jidStr, Messages: resolveMentionsInList(ctx, client, changed)})
	}
}

// handleOpenChat responds to the app requesting one chat's messages: emits
// what's already known (from history sync and/or prior live messages)
// immediately, then downloads any images in that batch that haven't been
// fetched yet, re-emitting the chat once each one lands. Also marks the chat
// read (see markChatReadAndClearUnread) and records it as the currently
// open chat (see openChatJID) so a live message arriving while it's open
// doesn't bump its unread count back up.
func handleOpenChat(ctx context.Context, client *whatsmeow.Client, logger waLog.Logger, jid string, chats map[string]chatSummary, messages map[string][]chatMessage) {
	setOpenChatJID(jid)

	messagesMu.Lock()
	list, ok := messages[jid]
	if !ok {
		list = loadCachedMessages(jid)
		messages[jid] = list
	}
	var toDownload []chatMessage
	var toDownloadAudio []chatMessage
	var toDownloadVideo []chatMessage
	var toDownloadSticker []chatMessage
	for _, m := range list {
		if m.Type == "image" && m.ImagePath == "" && m.ImageDirectPath != "" {
			toDownload = append(toDownload, m)
		}
		if m.Type == "audio" && m.AudioPath == "" && m.AudioDirectPath != "" {
			toDownloadAudio = append(toDownloadAudio, m)
		}
		if (m.Type == "video" || m.Type == "gif") && m.VideoPath == "" && m.VideoDirectPath != "" {
			toDownloadVideo = append(toDownloadVideo, m)
		}
		if m.Type == "sticker" && m.StickerPath == "" && m.StickerDirectPath != "" {
			toDownloadSticker = append(toDownloadSticker, m)
		}
	}
	messagesMu.Unlock()

	logger.Debugf("open_chat %s: %d cached messages, %d images to download, %d audio to download, %d video to download, %d stickers to download", jid, len(list), len(toDownload), len(toDownloadAudio), len(toDownloadVideo), len(toDownloadSticker))
	emit(event{Type: "messages", JID: jid, Messages: resolveMentionsInList(ctx, client, list)})

	go markChatReadAndClearUnread(ctx, client, logger, jid, list, chats)

	for _, m := range toDownload {
		go downloadImage(ctx, client, logger, messages, jid, m)
	}
	for _, m := range toDownloadAudio {
		go downloadAudio(ctx, client, logger, messages, jid, m)
	}
	for _, m := range toDownloadVideo {
		go downloadVideo(ctx, client, logger, messages, jid, m)
	}
	for _, m := range toDownloadSticker {
		go downloadSticker(ctx, client, logger, messages, jid, m)
	}
}

// handleSendMessage sends a text message to jid via WhatsApp, then folds the
// sent message into the local cache the same way a live incoming message
// would (see handleMessage) — bumping the chat to the front and emitting
// both updated events — so it shows up immediately rather than waiting for
// the server to echo it back as a *events.Message.
func handleSendMessage(ctx context.Context, client *whatsmeow.Client, logger waLog.Logger, jidStr, text string, chats map[string]chatSummary, messages map[string][]chatMessage) {
	jid, err := types.ParseJID(jidStr)
	if err != nil {
		logger.Warnf("send_message: bad jid %q: %v", jidStr, err)
		return
	}

	resp, err := client.SendMessage(ctx, jid, &waE2E.Message{Conversation: proto.String(text)})
	if err != nil {
		logger.Warnf("send_message to %s failed: %v", jidStr, err)
		emit(event{Type: "error", Message: fmt.Sprintf("failed to send message: %v", err)})
		return
	}
	timestamp := resp.Timestamp.Unix()

	chatsMu.Lock()
	c, ok := chats[jidStr]
	if !ok {
		c = chatSummary{JID: jidStr, Name: jid.User, IsGroup: jid.Server == types.GroupServer}
	}
	c.Timestamp = timestamp
	chats[jidStr] = c
	list := saveChats(chats)
	chatsMu.Unlock()
	emit(event{Type: "chats", Chats: list})

	status := "sent"
	if c.IsGroup {
		status = ""
	}
	cm := chatMessage{
		ID:        resp.ID,
		Timestamp: timestamp,
		FromMe:    true,
		Status:    status,
		Type:      "text",
		Text:      text,
	}

	messagesMu.Lock()
	upsertMessage(messages, jidStr, cm)
	messagesMu.Unlock()

	emit(event{Type: "message_update", JID: jidStr, Messages: resolveMentionsInList(ctx, client, []chatMessage{cm})})
}

// reactionSenderJID resolves the JID BuildMessageKey needs as its "sender"
// argument to identify target within chat when building a reaction to it:
// types.EmptyJID for our own message (BuildMessageKey then marks the built
// key FromMe: true), chat itself for a 1:1 peer's message (the chat IS the
// peer's JID there), or target.Sender for a group participant's message.
func reactionSenderJID(chat types.JID, target chatMessage) (types.JID, error) {
	if target.FromMe {
		return types.EmptyJID, nil
	}
	if chat.Server != types.GroupServer {
		return chat, nil
	}
	sender, err := types.ParseJID(target.Sender)
	if err != nil {
		return types.EmptyJID, fmt.Errorf("bad sender jid %q: %w", target.Sender, err)
	}
	if sender.User == "" {
		return types.EmptyJID, fmt.Errorf("bad sender jid %q: no user part", target.Sender)
	}
	return sender, nil
}

// recordedAudioMimetype is what this app's own recordings are encoded as
// (AAC audio in an MPEG-4 container — see app/'s VoiceRecorder). Sent as a
// regular (non-PTT) audio attachment rather than faking an Opus/Ogg voice
// note: WhatsApp clients trust the mimetype, and claiming "audio/ogg;
// codecs=opus" over AAC bytes would fail to decode on the receiving end.
const recordedAudioMimetype = "audio/mp4"

// handleSendAudio uploads a recorded voice message (an AAC/MPEG-4 file at
// path, relative to the process's working directory — see CoreProcess.kt's
// sendAudio) to WhatsApp servers, sends it as an audio message to jid, then
// folds it into the local cache the same way handleSendMessage does for
// text. The recording is moved into the same media/<jid>/<msgID>.<ext>
// layout downloadAudio uses for incoming audio, so it survives independent
// of wherever the app staged it.
func handleSendAudio(ctx context.Context, client *whatsmeow.Client, logger waLog.Logger, jidStr, path string, durationMs int64, chats map[string]chatSummary, messages map[string][]chatMessage) {
	jid, err := types.ParseJID(jidStr)
	if err != nil {
		logger.Warnf("send_audio: bad jid %q: %v", jidStr, err)
		return
	}

	data, err := os.ReadFile(path)
	if err != nil {
		logger.Warnf("send_audio: failed to read %s: %v", path, err)
		emit(event{Type: "error", Message: fmt.Sprintf("failed to read recording: %v", err)})
		return
	}

	uploaded, err := client.Upload(ctx, data, whatsmeow.MediaAudio)
	if err != nil {
		logger.Warnf("send_audio: upload to %s failed: %v", jidStr, err)
		emit(event{Type: "error", Message: fmt.Sprintf("failed to upload recording: %v", err)})
		return
	}

	seconds := uint32(durationMs / 1000)
	resp, err := client.SendMessage(ctx, jid, &waE2E.Message{
		AudioMessage: &waE2E.AudioMessage{
			URL:           proto.String(uploaded.URL),
			DirectPath:    proto.String(uploaded.DirectPath),
			Mimetype:      proto.String(recordedAudioMimetype),
			FileLength:    proto.Uint64(uploaded.FileLength),
			FileSHA256:    uploaded.FileSHA256,
			FileEncSHA256: uploaded.FileEncSHA256,
			MediaKey:      uploaded.MediaKey,
			Seconds:       proto.Uint32(seconds),
			PTT:           proto.Bool(false),
		},
	})
	if err != nil {
		logger.Warnf("send_audio to %s failed: %v", jidStr, err)
		emit(event{Type: "error", Message: fmt.Sprintf("failed to send recording: %v", err)})
		return
	}
	timestamp := resp.Timestamp.Unix()

	chatsMu.Lock()
	c, ok := chats[jidStr]
	if !ok {
		c = chatSummary{JID: jidStr, Name: jid.User, IsGroup: jid.Server == types.GroupServer}
	}
	c.Timestamp = timestamp
	chats[jidStr] = c
	list := saveChats(chats)
	chatsMu.Unlock()
	emit(event{Type: "chats", Chats: list})

	finalPath := audioPath(jidStr, resp.ID, recordedAudioMimetype)
	if err := os.MkdirAll(filepath.Dir(finalPath), 0o700); err != nil {
		logger.Warnf("send_audio: failed to create media dir for %s: %v", jidStr, err)
	} else if err := os.Rename(path, finalPath); err != nil {
		logger.Warnf("send_audio: failed to move recording into media cache: %v", err)
	} else {
		path = finalPath
	}

	status := "sent"
	if c.IsGroup {
		status = ""
	}
	cm := chatMessage{
		ID:           resp.ID,
		Timestamp:    timestamp,
		FromMe:       true,
		Status:       status,
		Type:         "audio",
		AudioPath:    path,
		AudioSeconds: seconds,
	}

	messagesMu.Lock()
	upsertMessage(messages, jidStr, cm)
	messagesMu.Unlock()

	emit(event{Type: "message_update", JID: jidStr, Messages: resolveMentionsInList(ctx, client, []chatMessage{cm})})
}

// readCommands is the stdin half of the protocol: one JSON command per
// line from the app. Runs for the life of the process; a closed stdin
// (app process torn down) just ends the loop.
func readCommands(ctx context.Context, client *whatsmeow.Client, logger waLog.Logger, chats map[string]chatSummary, messages map[string][]chatMessage) {
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		var cmd command
		if err := json.Unmarshal(scanner.Bytes(), &cmd); err != nil {
			logger.Warnf("unparseable command from app: %v", err)
			continue
		}
		switch cmd.Type {
		case "open_chat":
			if cmd.JID != "" {
				go handleOpenChat(ctx, client, logger, cmd.JID, chats, messages)
			}
		case "close_chat":
			if cmd.JID != "" {
				clearOpenChatJID(cmd.JID)
			}
		case "send_message":
			if cmd.JID != "" && cmd.Text != "" {
				go handleSendMessage(ctx, client, logger, cmd.JID, cmd.Text, chats, messages)
			}
		case "send_audio":
			if cmd.JID != "" && cmd.AudioPath != "" {
				go handleSendAudio(ctx, client, logger, cmd.JID, cmd.AudioPath, cmd.DurationMs, chats, messages)
			}
		default:
			logger.Warnf("unknown command from app: %s", cmd.Type)
		}
	}
	if err := scanner.Err(); err != nil {
		logger.Warnf("stdin read error: %v", err)
	}
}

// isJunkChatJID reports whether jid is one of the garbage conversation IDs
// WhatsApp's history sync occasionally includes (e.g. "0@s.whatsapp.net")
// that don't correspond to any real chat and would otherwise surface as a
// chat literally named "0".
func isJunkChatJID(jid types.JID) bool {
	return jid.User == "" || jid.User == "0"
}

// canonicalizeChatJID collapses a contact's LID and phone-number JIDs onto a
// single stable key. WhatsApp addresses the same conversation under
// whichever mode is active for a given sync chunk or event — sometimes the
// phone-number JID, sometimes the opaque @lid — so without this the same
// person forks into two chat-list entries with two different names (the
// phone's address-book name on the PN-keyed one, their self-set WhatsApp
// name on the LID-keyed one). The phone-number JID is always chosen as
// canonical so the address-book name (see contactName) wins.
func canonicalizeChatJID(ctx context.Context, client *whatsmeow.Client, jid types.JID) types.JID {
	if jid.Server != types.DefaultUserServer && jid.Server != types.HiddenUserServer {
		return jid
	}

	ownID := client.Store.GetJID().ToNonAD()
	ownLID := client.Store.GetLID().ToNonAD()
	if !ownID.IsEmpty() && !ownLID.IsEmpty() && jid.ToNonAD() == ownLID {
		return ownID
	}

	if jid.Server == types.HiddenUserServer {
		if pn, err := client.Store.LIDs.GetPNForLID(ctx, jid.ToNonAD()); err == nil && !pn.IsEmpty() {
			return pn
		}
	}
	return jid
}

// sanitizeChats cleans up a chat map loaded from disk: dropping junk entries
// (see isJunkChatJID) and merging any duplicate "me" chat left over from
// before canonicalizeChatJID existed into a single entry, keeping whichever
// side has the newer timestamp.
func sanitizeChats(ctx context.Context, client *whatsmeow.Client, chats map[string]chatSummary) {
	for jidStr, c := range chats {
		jid, err := types.ParseJID(jidStr)
		if err != nil || isJunkChatJID(jid) {
			delete(chats, jidStr)
			continue
		}
		canonical := canonicalizeChatJID(ctx, client, jid)
		if canonical.String() == jidStr {
			continue
		}
		delete(chats, jidStr)
		if merged, ok := chats[canonical.String()]; !ok || c.Timestamp > merged.Timestamp {
			c.JID = canonical.String()
			chats[canonical.String()] = c
		}
	}
}

// sanitizeMessageCache folds any on-disk message cache left over under a
// non-canonical JID (see canonicalizeChatJID) into its canonical JID's
// cache. Message caches are per-JID files keyed by the exact JID a chat was
// addressed under at the time, so a contact whose chat forked into a
// LID-keyed and a phone-number-keyed entry also left behind two separate
// message histories; without this merge, collapsing the chat-list entries
// (sanitizeChats) would silently orphan whichever JID's messages the app no
// longer looks up. Must run before anything reads from messagesDir.
func sanitizeMessageCache(ctx context.Context, client *whatsmeow.Client) {
	entries, err := os.ReadDir(messagesDir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".json") {
			continue
		}
		jidStr := strings.TrimSuffix(name, ".json")
		jid, err := types.ParseJID(jidStr)
		if err != nil {
			continue
		}
		canonical := canonicalizeChatJID(ctx, client, jid)
		if canonical.String() == jidStr {
			continue
		}

		stale := loadCachedMessages(jidStr)
		if len(stale) > 0 {
			merged := loadCachedMessages(canonical.String())
			seen := make(map[string]bool, len(merged))
			for _, m := range merged {
				seen[m.ID] = true
			}
			for _, m := range stale {
				if seen[m.ID] {
					continue
				}
				seen[m.ID] = true
				merged = append(merged, m)
			}
			sort.Slice(merged, func(i, j int) bool { return merged[i].Timestamp < merged[j].Timestamp })
			if len(merged) > maxMessagesPerChat {
				merged = merged[len(merged)-maxMessagesPerChat:]
			}
			saveMessages(canonical.String(), merged)
		}
		os.Remove(messagesFilePath(jidStr))
	}
}

// handleHistorySync folds one history-sync chunk's conversations into chats
// and emits the updated snapshot. WhatsApp sends this in multiple chunks
// (bootstrap, recent, push-name, ...) so this is called once per chunk and
// just re-emits the accumulated total each time.
func handleHistorySync(ctx context.Context, client *whatsmeow.Client, hs *events.HistorySync, chats map[string]chatSummary, messages map[string][]chatMessage) {
	markHistorySyncActive()

	convs := hs.Data.GetConversations()
	if len(convs) == 0 {
		return
	}

	chatsMu.Lock()
	for _, conv := range convs {
		jid, err := types.ParseJID(conv.GetID())
		if err != nil || jid.Server == types.BroadcastServer || isJunkChatJID(jid) {
			continue
		}
		jid = canonicalizeChatJID(ctx, client, jid)

		// Merge into whatever's already known for this JID rather than
		// overwriting it: WhatsApp sends multiple sync chunks per JID
		// (bootstrap, recent, ...) and later chunks routinely leave fields
		// like conversationTimestamp unset, so a blind overwrite silently
		// throws away good data an earlier chunk already supplied.
		existing, hadExisting := chats[jid.String()]

		name := conv.GetName()
		if name == "" && hadExisting {
			name = existing.Name
		}
		if name == "" {
			name = contactName(ctx, client, jid)
		}
		if name == "" {
			name = jid.User
		}

		// Conversation-level timestamps are frequently left unset in later
		// sync chunks — fall back to the newest per-message timestamp
		// bundled in this entry, then to whatever was already known, and
		// keep the max across all of that (chunks can arrive in either
		// order).
		timestamp := int64(conv.GetConversationTimestamp())
		if timestamp == 0 {
			timestamp = int64(conv.GetLastMsgTimestamp())
		}
		for _, hm := range conv.GetMessages() {
			info := hm.GetMessage()
			if ts := int64(info.GetMessageTimestamp()); ts > timestamp {
				timestamp = ts
			}
			extractHistoryMessage(ctx, client, jid, info, messages)
		}
		if hadExisting && existing.Timestamp > timestamp {
			timestamp = existing.Timestamp
		}

		unread := existing.UnreadCount
		if conv.UnreadCount != nil {
			unread = int(conv.GetUnreadCount())
		}

		chats[jid.String()] = chatSummary{
			JID:         jid.String(),
			Name:        name,
			Timestamp:   timestamp,
			UnreadCount: unread,
			IsGroup:     jid.Server == types.GroupServer,
		}
	}
	list := saveChats(chats)
	chatsMu.Unlock()

	emit(event{Type: "chats", Chats: list})
}

// handleMessage keeps chat ordering fresh after the one-time history sync:
// any new message (in or out, from any linked device) bumps its chat to the
// front, the same way WhatsApp's own chat list behaves. It also folds the
// message itself into messages and, if it carries an image, downloads it
// right away — unlike history-sync backlog, live messages arrive one at a
// time so eager downloading is cheap.
func handleMessage(ctx context.Context, client *whatsmeow.Client, logger waLog.Logger, evt *events.Message, chats map[string]chatSummary, messages map[string][]chatMessage) {
	// A reaction arrives as its own *events.Message (Message.ReactionMessage
	// set, everything else nil) rather than an edit to the target message —
	// handle it separately and don't fall through to the rest of this
	// function, which would otherwise treat it as a new (unsupported)
	// message and bump the chat/unread count for it.
	if evt.Message.GetReactionMessage() != nil {
		handleReaction(ctx, client, evt, messages)
		return
	}

	jid := evt.Info.Chat
	if jid.Server == types.BroadcastServer || isJunkChatJID(jid) {
		return
	}
	jid = canonicalizeChatJID(ctx, client, jid)
	timestamp := evt.Info.Timestamp.Unix()
	// Suppress the unread bump for the chat currently on screen — the app
	// is showing this message as it arrives, so it's already "read".
	isOpen := isOpenChatJID(jid.String())

	chatsMu.Lock()
	c, ok := chats[jid.String()]
	if !ok {
		name := contactName(ctx, client, jid)
		if name == "" {
			name = evt.Info.PushName
		}
		if name == "" {
			name = jid.User
		}
		c = chatSummary{JID: jid.String(), Name: name, IsGroup: jid.Server == types.GroupServer}
	}
	bumped := timestamp > c.Timestamp
	if bumped {
		c.Timestamp = timestamp
	}
	unread := !evt.Info.IsFromMe && !isOpen
	if unread {
		c.UnreadCount++
	}
	changed := bumped || unread || !ok
	var list []chatSummary
	if changed {
		chats[jid.String()] = c
		list = saveChats(chats)
	}
	chatsMu.Unlock()

	if changed {
		emit(event{Type: "chats", Chats: list})
	}

	text, msgType, img, audio, video, sticker, ok := extractMessage(evt.Message)
	if !ok {
		return
	}
	cm := chatMessage{
		ID:        evt.Info.ID,
		Timestamp: timestamp,
		FromMe:    evt.Info.IsFromMe,
		Type:      msgType,
		Text:      text,
	}
	if msgType == "image" && img != nil {
		setImageFields(&cm, img)
	}
	if msgType == "audio" && audio != nil {
		setAudioFields(&cm, audio)
	}
	if (msgType == "video" || msgType == "gif") && video != nil {
		setVideoFields(&cm, video)
	}
	if msgType == "sticker" && sticker != nil {
		setStickerFields(&cm, sticker)
	}
	// Same gap as extractHistoryMessage: a message sent from another linked
	// device arrives here live with no status. Unlike history sync, a live
	// *events.Message carries no delivery/read state of its own — but its
	// mere arrival means the server already has it, so "sent" is a safe
	// starting point; handleSentMessageStatusReceipt upgrades it further as
	// real receipts come in (applyMessageStatus doesn't require an existing
	// status to do that).
	if evt.Info.IsFromMe && !evt.Info.IsGroup {
		cm.Status = "sent"
	}
	if evt.Info.IsGroup && !evt.Info.IsFromMe {
		cm.Sender = evt.Info.Sender.String()
		cm.SenderName = evt.Info.PushName
	}

	messagesMu.Lock()
	upsertMessage(messages, jid.String(), cm)
	messagesMu.Unlock()

	emit(event{Type: "message_update", JID: jid.String(), Messages: resolveMentionsInList(ctx, client, []chatMessage{cm})})

	if !evt.Info.IsFromMe && isOpen {
		go markChatRead(ctx, client, logger, jid, []chatMessage{cm})
	}

	if msgType == "image" && img != nil {
		go downloadImage(ctx, client, logger, messages, jid.String(), cm)
	}
	if msgType == "audio" && audio != nil {
		go downloadAudio(ctx, client, logger, messages, jid.String(), cm)
	}
	if (msgType == "video" || msgType == "gif") && video != nil {
		go downloadVideo(ctx, client, logger, messages, jid.String(), cm)
	}
	if msgType == "sticker" && sticker != nil {
		go downloadSticker(ctx, client, logger, messages, jid.String(), cm)
	}
}

// handleReaction updates the Reactions of whichever cached message
// evt.Message.ReactionMessage.Key.ID names, for a *events.Message that
// carries a reaction instead of ordinary content (see handleMessage). A
// blank target ID, a chat this app ignores (broadcast/junk), or a target
// message not currently cached (older than maxMessagesPerChat, or in a chat
// never opened) are all silent no-ops — the same way an unmatched receipt
// is in applyMessageStatus.
func handleReaction(ctx context.Context, client *whatsmeow.Client, evt *events.Message, messages map[string][]chatMessage) {
	r := evt.Message.GetReactionMessage()
	targetID := r.GetKey().GetID()
	if targetID == "" {
		return
	}
	jid := canonicalizeChatJID(ctx, client, evt.Info.Chat)
	if jid.Server == types.BroadcastServer || isJunkChatJID(jid) {
		return
	}
	jidStr := jid.String()

	senderName := ""
	if evt.Info.IsGroup && !evt.Info.IsFromMe {
		senderName = evt.Info.PushName
	}

	messagesMu.Lock()
	list, ok := messages[jidStr]
	var updated chatMessage
	found := false
	if ok {
		for i, cur := range list {
			if cur.ID == targetID {
				cur.Reactions = applyReaction(cur.Reactions, evt.Info.Sender.String(), senderName, evt.Info.IsFromMe, r.GetText())
				list[i] = cur
				updated = cur
				found = true
				break
			}
		}
		if found {
			messages[jidStr] = list
			saveMessages(jidStr, list)
		}
	}
	messagesMu.Unlock()

	if found {
		emit(event{Type: "message_update", JID: jidStr, Messages: resolveMentionsInList(ctx, client, []chatMessage{updated})})
	}
}

// applyReaction upserts sender's reaction into reactions: emoji == "" (see
// whatsmeow.RemoveReactionText) removes sender's existing entry if any,
// otherwise sender's entry is added or replaced with emoji.
func applyReaction(reactions []chatReaction, sender, senderName string, fromMe bool, emoji string) []chatReaction {
	idx := -1
	for i, r := range reactions {
		if r.Sender == sender {
			idx = i
			break
		}
	}
	if emoji == "" {
		if idx == -1 {
			return reactions
		}
		return append(reactions[:idx], reactions[idx+1:]...)
	}
	entry := chatReaction{Sender: sender, SenderName: senderName, FromMe: fromMe, Emoji: emoji}
	if idx == -1 {
		return append(reactions, entry)
	}
	reactions[idx] = entry
	return reactions
}

// contactName looks up the best display name whatsmeow's contact store has
// for jid: the phone's address-book name if synced, else the contact's
// self-set push name, else their business name. Returns "" if nothing is
// known yet — callers fall back to the raw phone number in that case.
func contactName(ctx context.Context, client *whatsmeow.Client, jid types.JID) string {
	contact, err := client.Store.Contacts.GetContact(ctx, jid)
	if err != nil || !contact.Found {
		return ""
	}
	switch {
	case contact.FullName != "":
		return contact.FullName
	case contact.PushName != "":
		return contact.PushName
	case contact.BusinessName != "":
		return contact.BusinessName
	default:
		return ""
	}
}

// reconcileContactNames re-resolves every cached 1:1 chat's name against
// the (local, no-network) contact store, upgrading any that were cached
// before their contact's name was known. See refreshContactName for the
// live-event counterpart that keeps this correct as new names arrive.
func reconcileContactNames(ctx context.Context, client *whatsmeow.Client, chats map[string]chatSummary) {
	for jidStr, c := range chats {
		if c.IsGroup {
			continue
		}
		jid, err := types.ParseJID(jidStr)
		if err != nil {
			continue
		}
		name := contactName(ctx, client, jid)
		if name == "" || name == c.Name {
			continue
		}
		c.Name = name
		chats[jidStr] = c
	}
}

// refreshContactName re-resolves a 1:1 chat's display name against the
// contact store and emits an updated chat list if it improved. Contact
// names (address-book sync, push names) routinely arrive after the chat
// list is first built from history sync, so a chat initially falls back to
// showing the raw phone number until this backfills it.
func refreshContactName(ctx context.Context, client *whatsmeow.Client, jid types.JID, chats map[string]chatSummary) {
	if jid.Server == types.GroupServer {
		return
	}
	jid = canonicalizeChatJID(ctx, client, jid)
	name := contactName(ctx, client, jid)
	if name == "" {
		return
	}

	chatsMu.Lock()
	c, ok := chats[jid.String()]
	if !ok || c.Name == name {
		chatsMu.Unlock()
		return
	}
	c.Name = name
	chats[jid.String()] = c
	list := saveChats(chats)
	chatsMu.Unlock()

	emit(event{Type: "chats", Chats: list})
}

// fetchGroupNames resolves group subjects that history sync never sends
// (Conversation.Name is only populated for 1:1 chats there): one bulk IQ
// query for all joined groups, versus one query per group.
func fetchGroupNames(ctx context.Context, client *whatsmeow.Client, logger waLog.Logger, chats map[string]chatSummary) {
	groups, err := client.GetJoinedGroups(ctx)
	if err != nil {
		logger.Warnf("failed to fetch joined group names: %v", err)
		return
	}

	chatsMu.Lock()
	changed := false
	for _, g := range groups {
		jid := g.JID.String()
		c, ok := chats[jid]
		if !ok || g.Name == "" || c.Name == g.Name {
			continue
		}
		c.Name = g.Name
		chats[jid] = c
		changed = true
	}
	var list []chatSummary
	if changed {
		list = saveChats(chats)
	}
	chatsMu.Unlock()

	if changed {
		emit(event{Type: "chats", Chats: list})
	}
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

	chats := loadCachedChats()
	messages := make(map[string][]chatMessage)

	client := whatsmeow.NewClient(deviceStore, logger)
	sanitizeChats(ctx, client, chats)
	sanitizeMessageCache(ctx, client)

	// Contact names (address-book sync, push names) often arrive after a
	// chat was first cached, so a chat can get stuck showing the raw phone
	// number forever once written to disk with no live event to correct
	// it. The contact store is local (no network needed), so re-resolve
	// every cached 1:1 chat's name against it before the first emit.
	if len(chats) > 0 {
		reconcileContactNames(ctx, client, chats)
		emit(event{Type: "chats", Chats: saveChats(chats)})
	}
	client.AddEventHandler(func(evt any) {
		switch e := evt.(type) {
		case *events.Connected:
			logger.Infof("connected")
			go fetchGroupNames(ctx, client, logger, chats)
		case *events.LoggedOut:
			logger.Warnf("logged out, delete whatsapp.db and re-run to link again")
			emit(event{Type: "logged_out"})
		case *events.HistorySync:
			handleHistorySync(ctx, client, e, chats, messages)
		case *events.Message:
			handleMessage(ctx, client, logger, e, chats, messages)
		case *events.Receipt:
			handleReadReceipt(ctx, client, e, chats)
			handleSentMessageStatusReceipt(ctx, client, e, messages)
		case *events.Contact:
			refreshContactName(ctx, client, e.JID, chats)
			invalidateMentionName(e.JID.User)
		case *events.PushName:
			refreshContactName(ctx, client, e.JID, chats)
			invalidateMentionName(e.JID.User)
		}
	})
	go readCommands(ctx, client, logger, chats, messages)

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
