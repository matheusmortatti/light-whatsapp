package com.matheusmortatti.lightwhatsapp

import android.content.Context
import android.util.Log
import java.io.File
import java.io.IOException
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.channels.awaitClose
import kotlinx.coroutines.flow.Flow
import kotlinx.coroutines.flow.callbackFlow
import kotlinx.coroutines.flow.flowOn
import org.json.JSONArray
import org.json.JSONObject

data class Chat(
    val jid: String,
    val name: String,
    val timestamp: Long,
    val unreadCount: Int,
    val isGroup: Boolean,
)

// "text", "image", "audio", "video", "gif", "sticker", and "poll" show up
// here (see core/main.go's extractMessage) — every other WhatsApp message
// type is dropped before it reaches the app.
data class Message(
    val id: String,
    val timestamp: Long,
    val fromMe: Boolean,
    // "sent" | "delivered" | "read", 1:1 chats only — null for group chats,
    // incoming messages, and messages sent before this field existed. See
    // core/main.go's chatMessage.Status.
    val status: String?,
    val senderName: String?,
    val type: String,
    val text: String,
    // Path relative to the process's working dir (context.filesDir) — null until
    // core finishes downloading it, for image messages.
    val imagePath: String?,
    // Same deal as imagePath, but for audio messages; audioSeconds is known
    // up front (from the sender's protobuf) even while the file itself is
    // still downloading.
    val audioPath: String?,
    val audioSeconds: Int,
    // Same deal again, but for video/gif messages ("gif" is WhatsApp's
    // GifPlayback-flagged video encoding — see core/main.go's
    // extractMessage). isGif mirrors type == "gif", provided directly so
    // rendering code doesn't need to re-derive it from a string.
    val videoPath: String?,
    val videoSeconds: Int,
    val isGif: Boolean,
    // Same deal again, but for sticker messages. Lottie (vector) stickers
    // never reach here — core/main.go treats them as unsupported.
    val stickerPath: String?,
    val stickerIsAnimated: Boolean,
    // Poll question/options/selectable-count and current votes — see
    // core/main.go's chatMessage.PollOptions/PollSelectableCount/PollVotes.
    // The question itself is `text`, same as an image's caption.
    val pollOptions: List<String>,
    val pollSelectableCount: Int,
    val pollVotes: List<PollVote>,
    // True once core has classified this media's download as permanently
    // failed (403/404/410 — see core/main.go's isPermanentDownloadFailure).
    // The corresponding *Path stays null forever in that case; no further
    // download will be retried. No retry action exists yet.
    val imageFailed: Boolean,
    val audioFailed: Boolean,
    val videoFailed: Boolean,
    val stickerFailed: Boolean,
    // Reactions on this message — see core/main.go's chatMessage.Reactions.
    // Populated whether the message is ours or theirs.
    val reactions: List<Reaction>,
    // Set only when this message is a reply — see core/main.go's
    // chatMessage.QuotedID. quotedType == null means "not a reply".
    // quotedSenderName is null when quotedFromMe is true, or when the
    // quoting participant's name isn't known yet.
    val quotedId: String?,
    val quotedFromMe: Boolean,
    val quotedSenderName: String?,
    val quotedType: String?,
    val quotedText: String,
)

// One person's current reaction to a message — see core/main.go's chatReaction.
data class Reaction(
    val sender: String,
    val senderName: String?,
    val fromMe: Boolean,
    val emoji: String,
)

// One person's current vote on a poll message — see core/main.go's
// chatPollVote. selectedOptions are indices into the poll message's
// pollOptions.
data class PollVote(
    val sender: String,
    val senderName: String?,
    val fromMe: Boolean,
    val selectedOptions: List<Int>,
)

sealed class CoreEvent {
    data class Qr(val code: String) : CoreEvent()
    data class Connected(val jid: String) : CoreEvent()
    object LoggedOut : CoreEvent()
    data class Error(val message: String) : CoreEvent()
    data class Chats(val chats: List<Chat>) : CoreEvent()
    data class Messages(val jid: String, val messages: List<Message>) : CoreEvent()
    // Incremental counterpart to Messages: one or a few messages that changed
    // (a download completing, a status receipt, a new send/receive) rather
    // than the chat's whole list — see core/main.go's "message_update" event.
    data class MessageUpdate(val jid: String, val messages: List<Message>) : CoreEvent()
    // See core/main.go's markHistorySyncActive: true while a burst of
    // history-sync chunks is in flight, false once it's been idle for a bit.
    data class SyncStatus(val syncing: Boolean) : CoreEvent()
}

/**
 * Launches core/'s cross-compiled binary (bundled as a "native lib" so it's
 * extracted to disk and executable — see core/build_android.sh) and turns
 * its stdout JSON-lines protocol into a [CoreEvent] stream. The subprocess
 * is tied to collection of the returned flow: cancelling collection (e.g.
 * ViewModel clearing its scope) destroys the process via [awaitClose].
 */
class CoreProcess(private val context: Context) {

    // Set while events() is collected, cleared in awaitClose — openChat() needs
    // this to write to core's stdin, the other half of the protocol.
    @Volatile private var process: Process? = null
    private val writeLock = Any()

    fun events(): Flow<CoreEvent> = callbackFlow {
        val binary = File(context.applicationInfo.nativeLibraryDir, "libwhatsmeowcore.so")
        val process = try {
            ProcessBuilder(binary.absolutePath)
                .directory(context.filesDir)
                .start()
        } catch (e: IOException) {
            Log.e(TAG, "failed to start core binary at ${binary.absolutePath}", e)
            trySend(CoreEvent.Error("failed to start core: ${e.message}"))
            close()
            return@callbackFlow
        }
        this@CoreProcess.process = process

        // Human-readable logs go to stderr (see core/main.go) — just surface
        // them in logcat, they're not part of the event protocol.
        val stderrThread = Thread({
            try {
                process.errorStream.bufferedReader().forEachLine { Log.d(TAG, it) }
            } catch (e: IOException) {
                // Expected when awaitClose interrupts this thread to unblock
                // the read during teardown — not an error.
            }
        }, "core-stderr").apply { isDaemon = true; start() }

        val stdoutThread = Thread({
            try {
                process.inputStream.bufferedReader().forEachLine { line ->
                    parseEvent(line)?.let { trySend(it) }
                }
            } catch (e: IOException) {
                // Expected when awaitClose interrupts this thread to unblock
                // the read during teardown — not an error.
            } finally {
                close()
            }
        }, "core-stdout").apply { isDaemon = true; start() }

        awaitClose {
            this@CoreProcess.process = null
            process.destroy()
            stdoutThread.interrupt()
            stderrThread.interrupt()
        }
    }.flowOn(Dispatchers.IO)

    /**
     * Sends an "open_chat" command to core's stdin, asking it to (re-)emit
     * the given chat's messages and mark it read — see core/main.go's
     * readCommands/handleOpenChat. A no-op if the subprocess isn't running
     * (events() not collected yet, or already torn down).
     */
    fun openChat(jid: String) {
        writeCommand(JSONObject().put("type", "open_chat").put("jid", jid))
    }

    /**
     * Sends a "close_chat" command to core's stdin, telling it the app has
     * navigated away from the given chat — see core/main.go's
     * readCommands/openChatJID. A no-op if the subprocess isn't running.
     */
    fun closeChat(jid: String) {
        writeCommand(JSONObject().put("type", "close_chat").put("jid", jid))
    }

    /**
     * Sends a "send_message" command to core's stdin, asking it to send a
     * text message to the given chat — see core/main.go's
     * readCommands/handleSendMessage. A no-op if the subprocess isn't
     * running (events() not collected yet, or already torn down).
     */
    fun sendMessage(jid: String, text: String) {
        writeCommand(JSONObject().put("type", "send_message").put("jid", jid).put("text", text))
    }

    /**
     * Sends a "send_audio" command to core's stdin, asking it to upload and
     * send a recorded voice message to the given chat — see core/main.go's
     * readCommands/handleSendAudio. [audioPath] must be relative to
     * context.filesDir (core's working dir), same as a downloaded message's
     * imagePath/audioPath. A no-op if the subprocess isn't running.
     */
    fun sendAudio(jid: String, audioPath: String, durationMs: Long) {
        writeCommand(
            JSONObject()
                .put("type", "send_audio")
                .put("jid", jid)
                .put("audio_path", audioPath)
                .put("duration_ms", durationMs),
        )
    }

    /**
     * Sends a "send_reaction" command to core's stdin, asking it to react
     * to an existing message — see core/main.go's
     * readCommands/handleSendReaction. [emoji] == "" removes a previously-
     * sent reaction. A no-op if the subprocess isn't running.
     */
    fun sendReaction(jid: String, messageId: String, emoji: String) {
        writeCommand(
            JSONObject()
                .put("type", "send_reaction")
                .put("jid", jid)
                .put("message_id", messageId)
                .put("emoji", emoji),
        )
    }

    /**
     * Sends a "send_poll_vote" command to core's stdin, asking it to cast
     * (or change) this device's vote on a poll message — see
     * core/main.go's readCommands/handleSendPollVote. [selectedOptions] is
     * the complete replacement selection (indices into the poll's
     * options), not a delta — an empty list retracts the vote. A no-op if
     * the subprocess isn't running.
     */
    fun sendPollVote(jid: String, messageId: String, selectedOptions: List<Int>) {
        writeCommand(
            JSONObject()
                .put("type", "send_poll_vote")
                .put("jid", jid)
                .put("message_id", messageId)
                .put("selected_options", JSONArray(selectedOptions)),
        )
    }

    private fun writeCommand(command: JSONObject) {
        val out = process?.outputStream ?: return
        val line = command.toString() + "\n"
        try {
            synchronized(writeLock) {
                out.write(line.toByteArray())
                out.flush()
            }
        } catch (e: IOException) {
            Log.w(TAG, "failed to write command to core: $command", e)
        }
    }

    private fun parseEvent(line: String): CoreEvent? = try {
        val obj = JSONObject(line)
        when (obj.optString("type")) {
            "qr" -> CoreEvent.Qr(obj.getString("code"))
            "connected" -> CoreEvent.Connected(obj.getString("jid"))
            "logged_out" -> CoreEvent.LoggedOut
            "error" -> CoreEvent.Error(obj.optString("message"))
            "chats" -> CoreEvent.Chats(parseChats(obj.optJSONArray("chats")))
            "messages" -> CoreEvent.Messages(obj.getString("jid"), parseMessages(obj.optJSONArray("messages")))
            "message_update" -> CoreEvent.MessageUpdate(obj.getString("jid"), parseMessages(obj.optJSONArray("messages")))
            "sync_status" -> CoreEvent.SyncStatus(obj.optBoolean("syncing"))
            else -> null
        }
    } catch (e: Exception) {
        Log.w(TAG, "unparseable line from core: $line", e)
        null
    }

    private fun parseChats(array: JSONArray?): List<Chat> {
        if (array == null) return emptyList()
        return (0 until array.length()).map { i ->
            val o = array.getJSONObject(i)
            Chat(
                jid = o.getString("jid"),
                name = o.optString("name").ifBlank { o.getString("jid") },
                timestamp = o.optLong("timestamp", 0L),
                unreadCount = o.optInt("unread_count", 0),
                isGroup = o.optBoolean("is_group", false),
            )
        }
    }

    private fun parseMessages(array: JSONArray?): List<Message> {
        if (array == null) return emptyList()
        return (0 until array.length()).map { i ->
            val o = array.getJSONObject(i)
            Message(
                id = o.getString("id"),
                timestamp = o.optLong("timestamp", 0L),
                fromMe = o.optBoolean("from_me", false),
                status = o.optString("status").ifBlank { null },
                senderName = o.optString("sender_name").ifBlank { null },
                type = o.optString("type", "text"),
                text = o.optString("text"),
                imagePath = o.optString("image_path").ifBlank { null },
                audioPath = o.optString("audio_path").ifBlank { null },
                audioSeconds = o.optInt("audio_seconds", 0),
                videoPath = o.optString("video_path").ifBlank { null },
                videoSeconds = o.optInt("video_seconds", 0),
                isGif = o.optBoolean("is_gif", false),
                stickerPath = o.optString("sticker_path").ifBlank { null },
                stickerIsAnimated = o.optBoolean("sticker_is_animated", false),
                pollOptions = parsePollOptions(o.optJSONArray("poll_options")),
                pollSelectableCount = o.optInt("poll_selectable_count", 0),
                pollVotes = parsePollVotes(o.optJSONArray("poll_votes")),
                imageFailed = o.optBoolean("image_failed", false),
                audioFailed = o.optBoolean("audio_failed", false),
                videoFailed = o.optBoolean("video_failed", false),
                stickerFailed = o.optBoolean("sticker_failed", false),
                reactions = parseReactions(o.optJSONArray("reactions")),
                quotedId = o.optString("quoted_id").ifBlank { null },
                quotedFromMe = o.optBoolean("quoted_from_me", false),
                quotedSenderName = o.optString("quoted_sender_name").ifBlank { null },
                quotedType = o.optString("quoted_type").ifBlank { null },
                quotedText = o.optString("quoted_text"),
            )
        }
    }

    private fun parseReactions(array: JSONArray?): List<Reaction> {
        if (array == null) return emptyList()
        return (0 until array.length()).map { i ->
            val o = array.getJSONObject(i)
            Reaction(
                sender = o.getString("sender"),
                senderName = o.optString("sender_name").ifBlank { null },
                fromMe = o.optBoolean("from_me", false),
                emoji = o.optString("emoji"),
            )
        }
    }

    private fun parsePollOptions(array: JSONArray?): List<String> {
        if (array == null) return emptyList()
        return (0 until array.length()).map { i -> array.getString(i) }
    }

    private fun parsePollVotes(array: JSONArray?): List<PollVote> {
        if (array == null) return emptyList()
        return (0 until array.length()).map { i ->
            val o = array.getJSONObject(i)
            val selected = o.optJSONArray("selected_options")
            PollVote(
                sender = o.getString("sender"),
                senderName = o.optString("sender_name").ifBlank { null },
                fromMe = o.optBoolean("from_me", false),
                selectedOptions = if (selected == null) emptyList() else (0 until selected.length()).map { j -> selected.getInt(j) },
            )
        }
    }

    companion object {
        private const val TAG = "CoreProcess"
    }
}
