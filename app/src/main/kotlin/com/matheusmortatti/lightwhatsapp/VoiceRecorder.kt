package com.matheusmortatti.lightwhatsapp

import android.content.Context
import android.media.MediaRecorder
import android.os.SystemClock
import android.util.Log
import java.io.File

private const val TAG = "VoiceRecorder"

// Self-contained recorder — sdk:client has an equivalent (LightAudioRecorder)
// but its constructor is internal to that module and app/ deliberately has
// no dependency on sdk:client (see CoreProcess.kt/message-sending history).
// Records AAC-in-MPEG-4, sent as-is by core/main.go's handleSendAudio.
class VoiceRecorder(private val context: Context) {
    private var recorder: MediaRecorder? = null
    private var outputFile: File? = null
    private var startedAtMs = 0L

    /**
     * Starts recording microphone input to [file], cancelling any active
     * recording first. Parent directories are created as needed; partial
     * output is deleted on failure. Requires RECORD_AUDIO to already be
     * granted.
     *
     * @throws Exception when the platform cannot prepare or start recording
     */
    fun start(file: File) {
        cancel()
        file.parentFile?.mkdirs()
        outputFile = file
        startedAtMs = SystemClock.elapsedRealtime()
        val newRecorder = MediaRecorder(context)
        try {
            newRecorder.apply {
                setAudioSource(MediaRecorder.AudioSource.MIC)
                setOutputFormat(MediaRecorder.OutputFormat.MPEG_4)
                setAudioEncoder(MediaRecorder.AudioEncoder.AAC)
                setOutputFile(file.absolutePath)
                prepare()
                start()
            }
            recorder = newRecorder
        } catch (e: Exception) {
            runCatching { newRecorder.release() }
            outputFile?.delete()
            outputFile = null
            startedAtMs = 0L
            throw e
        }
    }

    /**
     * Stops and finalizes the active recording.
     *
     * @return the finished file paired with elapsed recording milliseconds,
     *   or null when inactive or when the platform rejects the recording
     *   (invalid output is deleted in that case).
     */
    fun stop(): Pair<File, Long>? {
        val activeRecorder = recorder ?: return null
        val file = outputFile
        recorder = null
        val durationMs = SystemClock.elapsedRealtime() - startedAtMs
        return try {
            activeRecorder.stop()
            outputFile = null
            startedAtMs = 0L
            file?.let { it to durationMs }
        } catch (e: RuntimeException) {
            Log.w(TAG, "recorder.stop() rejected the recording, discarding", e)
            file?.delete()
            outputFile = null
            startedAtMs = 0L
            null
        } finally {
            runCatching { activeRecorder.release() }
        }
    }

    /** Cancels the active recording and deletes its output. Idempotent. */
    fun cancel() {
        val activeRecorder = recorder ?: return
        recorder = null
        runCatching { activeRecorder.stop() }
        runCatching { activeRecorder.release() }
        outputFile?.delete()
        outputFile = null
        startedAtMs = 0L
    }
}
