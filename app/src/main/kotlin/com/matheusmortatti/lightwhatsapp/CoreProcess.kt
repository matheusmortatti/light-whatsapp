package com.matheusmortatti.lightwhatsapp

import android.content.Context
import android.util.Log
import java.io.File
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.channels.awaitClose
import kotlinx.coroutines.flow.Flow
import kotlinx.coroutines.flow.callbackFlow
import kotlinx.coroutines.flow.flowOn
import org.json.JSONObject

sealed class CoreEvent {
    data class Qr(val code: String) : CoreEvent()
    data class Connected(val jid: String) : CoreEvent()
    object LoggedOut : CoreEvent()
    data class Error(val message: String) : CoreEvent()
}

/**
 * Launches core/'s cross-compiled binary (bundled as a "native lib" so it's
 * extracted to disk and executable — see core/build_android.sh) and turns
 * its stdout JSON-lines protocol into a [CoreEvent] stream. The subprocess
 * is tied to collection of the returned flow: cancelling collection (e.g.
 * ViewModel clearing its scope) destroys the process via [awaitClose].
 */
class CoreProcess(private val context: Context) {

    fun events(): Flow<CoreEvent> = callbackFlow {
        val binary = File(context.applicationInfo.nativeLibraryDir, "libwhatsmeowcore.so")
        val process = ProcessBuilder(binary.absolutePath)
            .directory(context.filesDir)
            .start()

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
            process.destroy()
            stdoutThread.interrupt()
            stderrThread.interrupt()
        }
    }.flowOn(Dispatchers.IO)

    private fun parseEvent(line: String): CoreEvent? = try {
        val obj = JSONObject(line)
        when (obj.optString("type")) {
            "qr" -> CoreEvent.Qr(obj.getString("code"))
            "connected" -> CoreEvent.Connected(obj.getString("jid"))
            "logged_out" -> CoreEvent.LoggedOut
            "error" -> CoreEvent.Error(obj.optString("message"))
            else -> null
        }
    } catch (e: Exception) {
        Log.w(TAG, "unparseable line from core: $line", e)
        null
    }

    companion object {
        private const val TAG = "CoreProcess"
    }
}
