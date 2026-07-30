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
	Type     string        `json:"type"` // "qr" | "connected" | "logged_out" | "error" | "chats" | "messages"
	Code     string        `json:"code,omitempty"`
	JID      string        `json:"jid,omitempty"`
	Message  string        `json:"message,omitempty"`
	Chats    []chatSummary `json:"chats,omitempty"`
	Messages []chatMessage `json:"messages,omitempty"`
}

// command is one line of the stdin protocol: the app asking core for
// something. Currently just "open_chat", to fetch (and start filling in
// images for) one conversation's messages.
type command struct {
	Type string `json:"type"`
	JID  string `json:"jid,omitempty"`
}

// chatMessage is one message within a chat, as sent to the app. Only text
// and image messages are represented — everything else (video, audio,
// documents, stickers, polls, reactions, ...) is dropped during extraction,
// per the app's scope.
type chatMessage struct {
	ID         string `json:"id"`
	Timestamp  int64  `json:"timestamp"`
	FromMe     bool   `json:"from_me"`
	Sender     string `json:"sender,omitempty"`      // sender JID, group chats only
	SenderName string `json:"sender_name,omitempty"` // best-effort display name, group chats only
	Type       string `json:"type"`                  // "text" | "image"
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
// messages arrive in. Anything else (video, audio, documents, stickers,
// polls, reactions, location, ...) comes back ok=false — the app only shows
// text and images.
func extractMessage(m *waE2E.Message) (text, msgType string, img *waE2E.ImageMessage, ok bool) {
	for i := 0; i < 4 && m != nil; i++ {
		switch {
		case m.GetConversation() != "":
			return m.GetConversation(), "text", nil, true
		case m.GetExtendedTextMessage() != nil:
			return m.GetExtendedTextMessage().GetText(), "text", nil, true
		case m.GetImageMessage() != nil:
			im := m.GetImageMessage()
			return im.GetCaption(), "image", im, true
		case m.GetEphemeralMessage() != nil:
			m = m.GetEphemeralMessage().GetMessage()
		case m.GetViewOnceMessage() != nil:
			m = m.GetViewOnceMessage().GetMessage()
		case m.GetViewOnceMessageV2() != nil:
			m = m.GetViewOnceMessageV2().GetMessage()
		default:
			return "", "", nil, false
		}
	}
	return "", "", nil, false
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
		user := match[1:]
		if name := contactName(ctx, client, types.NewJID(user, types.HiddenUserServer)); name != "" {
			return "@" + name
		}
		if name := contactName(ctx, client, types.NewJID(user, types.DefaultUserServer)); name != "" {
			return "@" + name
		}
		if client.Store.PushName != "" && ((client.Store.ID != nil && user == client.Store.ID.User) || user == client.Store.GetLID().User) {
			return "@" + client.Store.PushName
		}
		return match
	})
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
	text, msgType, img, ok := extractMessage(waMsg)
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

// downloadImage fetches an image message's media (using the persisted
// download reference set by setImageFields, not the original *waE2E.
// ImageMessage — that doesn't survive a process restart, this does), writes
// it to disk, and updates the stored chatMessage with the resulting path,
// clearing the now-unneeded key material and re-emitting the chat's message
// list so the app can render it once it lands. Meant to run in its own
// goroutine — image messages show up caption/placeholder-only until this
// completes.
func downloadImage(ctx context.Context, client *whatsmeow.Client, logger waLog.Logger, messages map[string][]chatMessage, jid string, m chatMessage) {
	data, err := client.DownloadMediaWithPath(ctx, m.ImageDirectPath, m.ImageFileEncSHA256, m.ImageFileSHA256, m.ImageMediaKey, whatsmeow.MediaImage, "", false)
	if err != nil {
		logger.Warnf("failed to download image %s/%s: %v", jid, m.ID, err)
		return
	}
	path := imagePath(jid, m.ID, m.ImageMimetype)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		logger.Warnf("failed to create media dir for %s: %v", jid, err)
		return
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		logger.Warnf("failed to write image %s/%s: %v", jid, m.ID, err)
		return
	}

	messagesMu.Lock()
	list, ok := messages[jid]
	if ok {
		for i, cur := range list {
			if cur.ID == m.ID {
				list[i].ImagePath = path
				list[i].ImageDirectPath = ""
				list[i].ImageMediaKey = nil
				list[i].ImageFileSHA256 = nil
				list[i].ImageFileEncSHA256 = nil
				list[i].ImageMimetype = ""
				break
			}
		}
		messages[jid] = list
		saveMessages(jid, list)
	}
	messagesMu.Unlock()

	if ok {
		emit(event{Type: "messages", JID: jid, Messages: resolveMentionsInList(ctx, client, list)})
	}
}

// handleOpenChat responds to the app requesting one chat's messages: emits
// what's already known (from history sync and/or prior live messages)
// immediately, then downloads any images in that batch that haven't been
// fetched yet, re-emitting the chat once each one lands.
func handleOpenChat(ctx context.Context, client *whatsmeow.Client, logger waLog.Logger, jid string, messages map[string][]chatMessage) {
	messagesMu.Lock()
	list, ok := messages[jid]
	if !ok {
		list = loadCachedMessages(jid)
		messages[jid] = list
	}
	var toDownload []chatMessage
	for _, m := range list {
		if m.Type == "image" && m.ImagePath == "" && m.ImageDirectPath != "" {
			toDownload = append(toDownload, m)
		}
	}
	messagesMu.Unlock()

	logger.Debugf("open_chat %s: %d cached messages, %d images to download", jid, len(list), len(toDownload))
	emit(event{Type: "messages", JID: jid, Messages: resolveMentionsInList(ctx, client, list)})

	for _, m := range toDownload {
		go downloadImage(ctx, client, logger, messages, jid, m)
	}
}

// readCommands is the stdin half of the protocol: one JSON command per
// line from the app. Runs for the life of the process; a closed stdin
// (app process torn down) just ends the loop.
func readCommands(ctx context.Context, client *whatsmeow.Client, logger waLog.Logger, messages map[string][]chatMessage) {
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
				go handleOpenChat(ctx, client, logger, cmd.JID, messages)
			}
		default:
			logger.Warnf("unknown command from app: %s", cmd.Type)
		}
	}
	if err := scanner.Err(); err != nil {
		logger.Warnf("stdin read error: %v", err)
	}
}

// handleHistorySync folds one history-sync chunk's conversations into chats
// and emits the updated snapshot. WhatsApp sends this in multiple chunks
// (bootstrap, recent, push-name, ...) so this is called once per chunk and
// just re-emits the accumulated total each time.
func handleHistorySync(ctx context.Context, client *whatsmeow.Client, hs *events.HistorySync, chats map[string]chatSummary, messages map[string][]chatMessage) {
	convs := hs.Data.GetConversations()
	if len(convs) == 0 {
		return
	}

	chatsMu.Lock()
	for _, conv := range convs {
		jid, err := types.ParseJID(conv.GetID())
		if err != nil || jid.Server == types.BroadcastServer {
			continue
		}

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
	jid := evt.Info.Chat
	if jid.Server == types.BroadcastServer {
		return
	}
	timestamp := evt.Info.Timestamp.Unix()

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
		chats[jid.String()] = c
	}
	list := saveChats(chats)
	chatsMu.Unlock()

	if bumped {
		emit(event{Type: "chats", Chats: list})
	}

	text, msgType, img, ok := extractMessage(evt.Message)
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
	if evt.Info.IsGroup && !evt.Info.IsFromMe {
		cm.Sender = evt.Info.Sender.String()
		cm.SenderName = evt.Info.PushName
	}

	messagesMu.Lock()
	msgList := upsertMessage(messages, jid.String(), cm)
	messagesMu.Unlock()

	emit(event{Type: "messages", JID: jid.String(), Messages: resolveMentionsInList(ctx, client, msgList)})

	if msgType == "image" && img != nil {
		go downloadImage(ctx, client, logger, messages, jid.String(), cm)
	}
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
		case *events.Contact:
			refreshContactName(ctx, client, e.JID, chats)
		case *events.PushName:
			refreshContactName(ctx, client, e.JID, chats)
		}
	})
	go readCommands(ctx, client, logger, messages)

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
