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

// Only "text" and "image" ever show up here (see core/main.go's extractMessage) —
// every other WhatsApp message type is dropped before it reaches the app.
data class Message(
    val id: String,
    val timestamp: Long,
    val fromMe: Boolean,
    val senderName: String?,
    val type: String,
    val text: String,
    // Path relative to the process's working dir (context.filesDir) — null until
    // core finishes downloading it, for image messages.
    val imagePath: String?,
)

sealed class CoreEvent {
    data class Qr(val code: String) : CoreEvent()
    data class Connected(val jid: String) : CoreEvent()
    object LoggedOut : CoreEvent()
    data class Error(val message: String) : CoreEvent()
    data class Chats(val chats: List<Chat>) : CoreEvent()
    data class Messages(val jid: String, val messages: List<Message>) : CoreEvent()
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
        val process = ProcessBuilder(binary.absolutePath)
            .directory(context.filesDir)
            .start()
        this@CoreProcess.process = process

        // Human-readable logs go to stderr (see core/main.go) — just surface
        // them in logcat, they're not part of the event protocol.
        val stderrThread = Thread({
            process.errorStream.bufferedReader().forEachLine { Log.d(TAG, it) }
        }, "core-stderr").apply { isDaemon = true; start() }

        val stdoutThread = Thread({
            try {
                process.inputStream.bufferedReader().forEachLine { line ->
                    parseEvent(line)?.let { trySend(it) }
                }
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
     * the given chat's messages — see core/main.go's readCommands/handleOpenChat.
     * A no-op if the subprocess isn't running (events() not collected yet, or
     * already torn down).
     */
    fun openChat(jid: String) {
        val out = process?.outputStream ?: return
        val line = JSONObject().put("type", "open_chat").put("jid", jid).toString() + "\n"
        try {
            synchronized(writeLock) {
                out.write(line.toByteArray())
                out.flush()
            }
        } catch (e: IOException) {
            Log.w(TAG, "failed to send open_chat command", e)
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
                senderName = o.optString("sender_name").ifBlank { null },
                type = o.optString("type", "text"),
                text = o.optString("text"),
                imagePath = o.optString("image_path").ifBlank { null },
            )
        }
    }

    companion object {
        private const val TAG = "CoreProcess"
    }
}
