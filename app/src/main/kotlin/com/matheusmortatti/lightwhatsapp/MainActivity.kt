package com.matheusmortatti.lightwhatsapp

import android.Manifest
import android.content.pm.PackageManager
import android.graphics.BitmapFactory
import android.media.MediaPlayer
import android.os.Bundle
import android.os.SystemClock
import android.util.Log
import androidx.activity.ComponentActivity
import androidx.activity.compose.BackHandler
import androidx.activity.compose.rememberLauncherForActivityResult
import androidx.activity.compose.setContent
import androidx.activity.result.contract.ActivityResultContracts
import androidx.compose.foundation.Image
import androidx.compose.foundation.ScrollState
import androidx.compose.foundation.background
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.text.input.rememberTextFieldState
import androidx.compose.runtime.Composable
import androidx.compose.runtime.DisposableEffect
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.collectAsState
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.setValue
import androidx.compose.runtime.snapshotFlow
import androidx.compose.runtime.State
import androidx.compose.runtime.produceState
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.ImageBitmap
import androidx.compose.ui.graphics.asImageBitmap
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import androidx.core.content.ContextCompat
import androidx.lifecycle.viewmodel.compose.viewModel
import com.thelightphone.sdk.ui.LightBarButton
import com.thelightphone.sdk.ui.LightIcon
import com.thelightphone.sdk.ui.LightIcons
import com.thelightphone.sdk.ui.LightScrollView
import com.thelightphone.sdk.ui.LightText
import com.thelightphone.sdk.ui.LightTextField
import com.thelightphone.sdk.ui.LightTextInputEditor
import com.thelightphone.sdk.ui.LightTextVariant
import com.thelightphone.sdk.ui.LightTheme
import com.thelightphone.sdk.ui.LightThemeController
import com.thelightphone.sdk.ui.LightThemeTokens
import com.thelightphone.sdk.ui.LightTopBar
import com.thelightphone.sdk.ui.LightTopBarCenter
import com.thelightphone.sdk.ui.defaultKeyboardOptions
import com.thelightphone.sdk.ui.gridUnitsAsDp
import com.thelightphone.sdk.ui.lightClickable
import java.io.File
import java.io.IOException
import java.util.UUID
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.delay
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.filter
import kotlinx.coroutines.withContext

// Not a light-sdk Tool (no LightScreen/LightActivity/@InitialScreen) — this
// is a plain ComponentActivity. See PROJECT.md for why. Still reuses
// sdk:ui's LightTheme/LightText for visual parity with LightOS.
class MainActivity : ComponentActivity() {
    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        setContent {
            val themeColors by LightThemeController.colors.collectAsState()
            LightTheme(colors = themeColors) {
                QrLoginScreen()
            }
        }
    }
}

@Composable
private fun QrLoginScreen(viewModel: QrLoginViewModel = viewModel()) {
    val state by viewModel.state.collectAsState()
    val chats by viewModel.chats.collectAsState()
    val selectedChat by viewModel.selectedChat.collectAsState()
    val messages by viewModel.messages.collectAsState()

    when {
        state !is LoginState.Connected -> LoginScreen(state = state)
        selectedChat != null -> ChatDetailScreen(
            chat = selectedChat!!,
            messages = messages,
            onBack = viewModel::closeChat,
            onSend = viewModel::sendMessage,
            onSendAudio = viewModel::sendAudio,
        )
        else -> ChatListScreen(chats = chats, onChatClick = viewModel::openChat)
    }
}

@Composable
private fun LoginScreen(state: LoginState) {
    Column(
        modifier = Modifier
            .fillMaxSize()
            .background(LightThemeTokens.colors.background)
            .padding(32.dp),
        horizontalAlignment = Alignment.CenterHorizontally,
        verticalArrangement = Arrangement.Center,
    ) {
        LightText(
            text = "WhatsApp",
            variant = LightTextVariant.Heading,
            modifier = Modifier.padding(bottom = 24.dp),
        )

        when (state) {
            is LoginState.Idle -> LightText(
                text = "Waiting for QR code...",
                variant = LightTextVariant.Copy,
                lighten = true,
            )

            is LoginState.ShowingQr -> {
                Image(
                    bitmap = state.bitmap,
                    contentDescription = "Scan this QR code with WhatsApp on your phone",
                    modifier = Modifier.size(256.dp),
                )
                LightText(
                    text = "Scan with WhatsApp -> Linked Devices",
                    variant = LightTextVariant.Detail,
                    lighten = true,
                    modifier = Modifier.padding(top = 16.dp),
                )
            }

            is LoginState.Error -> LightText(
                text = "Error: ${state.message}",
                variant = LightTextVariant.Copy,
            )

            is LoginState.Connected -> Unit // handled by ChatListScreen
        }
    }
}

@Composable
private fun ChatListScreen(chats: List<Chat>, onChatClick: (Chat) -> Unit) {
    Column(
        modifier = Modifier
            .fillMaxSize()
            .background(LightThemeTokens.colors.background),
    ) {
        LightText(
            text = "WhatsApp",
            variant = LightTextVariant.Heading,
            modifier = Modifier.padding(horizontal = 32.dp, vertical = 16.dp),
        )

        if (chats.isEmpty()) {
            Box(
                modifier = Modifier
                    .weight(1f)
                    .fillMaxWidth(),
                contentAlignment = Alignment.Center,
            ) {
                LightText(
                    text = "Downloading chats...",
                    variant = LightTextVariant.Copy,
                    lighten = true,
                )
            }
        } else {
            LightScrollView(
                modifier = Modifier
                    .weight(1f)
                    .fillMaxWidth()
                    .padding(horizontal = 32.dp),
            ) {
                chats.forEach { chat ->
                    ChatRow(
                        chat = chat,
                        modifier = Modifier
                            .lightClickable { onChatClick(chat) }
                            .padding(vertical = 12.dp),
                    )
                }
            }
        }
    }
}

@Composable
private fun ChatRow(chat: Chat, modifier: Modifier = Modifier) {
    val displayName = if (chat.unreadCount > 0) "${chat.name} (${chat.unreadCount})" else chat.name
    LightText(
        text = displayName,
        variant = LightTextVariant.Copy,
        maxLines = 1,
        overflow = TextOverflow.Ellipsis,
        modifier = modifier.fillMaxWidth(),
    )
}

@Composable
private fun ChatDetailScreen(
    chat: Chat,
    messages: List<Message>,
    onBack: () -> Unit,
    onSend: (String) -> Unit,
    onSendAudio: (String, Long) -> Unit,
) {
    val context = LocalContext.current

    // Keyed on chat.jid so navigating to a different chat doesn't inherit
    // this one's open-editor state.
    var composing by rememberSaveable(chat.jid) { mutableStateOf(false) }

    // Recording state isn't rememberSaveable — VoiceRecorder holds a live
    // MediaRecorder, which can't survive process death, so there's nothing
    // useful to restore anyway.
    var recording by remember(chat.jid) { mutableStateOf(false) }
    var recordingStartMs by remember(chat.jid) { mutableStateOf(0L) }
    val voiceRecorder = remember { VoiceRecorder(context) }
    DisposableEffect(Unit) { onDispose { voiceRecorder.cancel() } }

    fun beginRecording() {
        val file = File(context.filesDir, "voice_tmp/${UUID.randomUUID()}.m4a")
        try {
            voiceRecorder.start(file)
            recordingStartMs = SystemClock.elapsedRealtime()
            recording = true
        } catch (e: Exception) {
            Log.w("MainActivity", "failed to start recording", e)
        }
    }

    fun cancelRecording() {
        voiceRecorder.cancel()
        recording = false
    }

    val requestRecordPermission = rememberLauncherForActivityResult(
        ActivityResultContracts.RequestPermission(),
    ) { granted -> if (granted) beginRecording() }

    BackHandler(onBack = {
        when {
            composing -> composing = false
            recording -> cancelRecording()
            else -> onBack()
        }
    })

    // Fresh state per chat so switching chats doesn't inherit scroll position.
    val scrollState = remember(chat.jid) { ScrollState(0) }
    // ScrollState.maxValue starts as a placeholder (Int.MAX_VALUE) until the
    // first layout measures real content, so that transition is a decrease,
    // not a growth event — handle it separately from the steady-state chase
    // below (which handles content growing further afterward, e.g. images
    // decoding async once we're already pinned to the bottom).
    LaunchedEffect(chat.jid) {
        var lastMax = -1
        snapshotFlow { scrollState.maxValue }
            .filter { it != Int.MAX_VALUE }
            .collect { newMax ->
                if (lastMax == -1 || (newMax > lastMax && scrollState.value >= lastMax)) {
                    scrollState.scrollTo(newMax)
                }
                lastMax = newMax
            }
    }

    if (composing) {
        val textState = rememberTextFieldState("")
        val keyboardOptionsFlow = remember { MutableStateFlow(defaultKeyboardOptions()) }
        LightTextInputEditor(
            title = chat.name,
            state = textState,
            keyboardOptionsFlow = keyboardOptionsFlow,
            submitLabel = "SEND",
            onSubmit = { text ->
                val trimmed = text.toString().trim()
                if (trimmed.isNotEmpty()) onSend(trimmed)
                composing = false
            },
            onBack = { composing = false },
            modifier = Modifier.background(LightThemeTokens.colors.background),
        )
        return
    }

    if (recording) {
        val elapsedMs by produceState(initialValue = 0L, recordingStartMs) {
            while (true) {
                value = SystemClock.elapsedRealtime() - recordingStartMs
                delay(RECORDING_TIMER_TICK_MS)
            }
        }
        RecordingScreen(
            chatName = chat.name,
            elapsedMs = elapsedMs,
            onCancel = { cancelRecording() },
            onSend = {
                val result = voiceRecorder.stop()
                recording = false
                if (result != null) {
                    val (file, durationMs) = result
                    onSendAudio(file.relativeTo(context.filesDir).path, durationMs)
                }
            },
        )
        return
    }

    Column(
        modifier = Modifier
            .fillMaxSize()
            .background(LightThemeTokens.colors.background),
    ) {
        LightTopBar(
            leftButton = LightBarButton.LightIcon(icon = LightIcons.BACK, onClick = onBack),
            center = LightTopBarCenter.Text(chat.name),
        )

        if (messages.isEmpty()) {
            Box(
                modifier = Modifier
                    .weight(1f)
                    .fillMaxWidth(),
                contentAlignment = Alignment.Center,
            ) {
                LightText(
                    text = "Loading messages...",
                    variant = LightTextVariant.Copy,
                    lighten = true,
                )
            }
        } else {
            LightScrollView(
                modifier = Modifier
                    .weight(1f)
                    .fillMaxWidth()
                    .padding(horizontal = 24.dp),
                scrollState = scrollState,
            ) {
                messages.forEach { message ->
                    MessageRow(
                        message = message,
                        isGroup = chat.isGroup,
                        modifier = Modifier.padding(vertical = 8.dp),
                    )
                }
            }
        }

        Row(
            verticalAlignment = Alignment.Bottom,
            modifier = Modifier
                .fillMaxWidth()
                .padding(horizontal = 24.dp, vertical = 12.dp),
        ) {
            LightTextField(
                label = "Message",
                value = "",
                placeholder = "Tap to type a message",
                onClick = { composing = true },
                modifier = Modifier.weight(1f),
            )
            LightIcon(
                icon = LightIcons.MICROPHONE,
                contentDescription = "Record a voice message",
                modifier = Modifier
                    .padding(start = 16.dp, bottom = 6.dp)
                    .lightClickable {
                        val granted = ContextCompat.checkSelfPermission(
                            context,
                            Manifest.permission.RECORD_AUDIO,
                        ) == PackageManager.PERMISSION_GRANTED
                        if (granted) beginRecording() else requestRecordPermission.launch(Manifest.permission.RECORD_AUDIO)
                    },
            )
        }
    }
}

private const val RECORDING_TIMER_TICK_MS = 200L

@Composable
private fun RecordingScreen(
    chatName: String,
    elapsedMs: Long,
    onCancel: () -> Unit,
    onSend: () -> Unit,
) {
    Column(
        modifier = Modifier
            .fillMaxSize()
            .background(LightThemeTokens.colors.background),
    ) {
        LightTopBar(
            leftButton = LightBarButton.LightIcon(icon = LightIcons.CLOSE, onClick = onCancel),
            center = LightTopBarCenter.Text(chatName),
            rightButton = LightBarButton.Text("SEND", onClick = onSend),
        )
        Box(
            modifier = Modifier
                .weight(1f)
                .fillMaxWidth(),
            contentAlignment = Alignment.Center,
        ) {
            Column(horizontalAlignment = Alignment.CenterHorizontally) {
                LightIcon(icon = LightIcons.MICROPHONE, size = 4f, contentDescription = null)
                LightText(
                    text = formatDuration(elapsedMs / 1000),
                    variant = LightTextVariant.Heading,
                    modifier = Modifier.padding(top = 16.dp),
                )
                LightText(
                    text = "Recording...",
                    variant = LightTextVariant.Detail,
                    lighten = true,
                    modifier = Modifier.padding(top = 4.dp),
                )
            }
        }
    }
}

@Composable
private fun MessageRow(message: Message, isGroup: Boolean, modifier: Modifier = Modifier) {
    Column(
        modifier = modifier.fillMaxWidth(),
        horizontalAlignment = if (message.fromMe) Alignment.End else Alignment.Start,
    ) {
        if (isGroup && !message.fromMe && !message.senderName.isNullOrBlank()) {
            LightText(
                text = message.senderName,
                variant = LightTextVariant.Fine,
                lighten = true,
            )
        }

        when (message.type) {
            "image" -> {
                val path = message.imagePath
                if (path != null) {
                    val bitmap by rememberDecodedImage(path)
                    if (bitmap != null) {
                        Image(
                            bitmap = bitmap!!,
                            contentDescription = "Photo",
                            modifier = Modifier
                                .size(200.dp)
                                .padding(bottom = 4.dp),
                        )
                    } else {
                        LightText(text = "[Photo]", variant = LightTextVariant.Copy, lighten = true)
                    }
                } else {
                    LightText(text = "[Photo]", variant = LightTextVariant.Copy, lighten = true)
                }
                if (message.text.isNotBlank()) {
                    LightText(text = message.text, variant = LightTextVariant.Copy)
                }
            }

            "audio" -> {
                val path = message.audioPath
                if (path != null) {
                    AudioMessageRow(relativePath = path, seconds = message.audioSeconds)
                } else {
                    LightText(text = "[Voice message]", variant = LightTextVariant.Copy, lighten = true)
                }
            }

            else -> LightText(text = message.text, variant = LightTextVariant.Copy)
        }
    }
}

// Plays a "audio" message's file off the main thread — core writes it
// relative to context.filesDir, the subprocess's working dir (see
// CoreProcess.kt), same as an image message's path.
@Composable
private fun AudioMessageRow(relativePath: String, seconds: Int, modifier: Modifier = Modifier) {
    val context = LocalContext.current
    val mediaPlayer = remember(relativePath) { MediaPlayer() }
    var prepared by remember(relativePath) { mutableStateOf(false) }
    var isPlaying by remember(relativePath) { mutableStateOf(false) }
    // Server-reported duration until the file's actually prepared, then the
    // decoded value — more accurate, and what mediaPlayer.currentPosition is
    // measured against.
    var durationMs by remember(relativePath) { mutableStateOf(seconds * 1000L) }
    // Mutated both by the polling loop below and directly by the rewind
    // button (an immediate seek, not something a poll tick should lag
    // behind), so it's a plain state, not produceState's derived one.
    var remainingMs by remember(relativePath) { mutableStateOf(durationMs) }

    LaunchedEffect(relativePath) {
        withContext(Dispatchers.IO) {
            try {
                mediaPlayer.setDataSource(File(context.filesDir, relativePath).absolutePath)
                mediaPlayer.prepare()
                prepared = true
            } catch (e: IOException) {
                Log.w("MainActivity", "failed to prepare audio $relativePath", e)
            }
        }
        if (prepared && mediaPlayer.duration > 0) {
            durationMs = mediaPlayer.duration.toLong()
            remainingMs = durationMs
        }
    }
    DisposableEffect(relativePath) {
        mediaPlayer.setOnCompletionListener {
            isPlaying = false
            mediaPlayer.seekTo(0)
            remainingMs = durationMs
        }
        onDispose { mediaPlayer.release() }
    }

    // Counts down while playing: polls actual playback position rather than
    // ticking a fixed clock, so it stays correct across pause/resume/rewind.
    LaunchedEffect(relativePath, isPlaying) {
        while (isPlaying) {
            remainingMs = (durationMs - mediaPlayer.currentPosition).coerceIn(0L, durationMs)
            delay(AUDIO_POSITION_POLL_MS)
        }
    }

    Row(
        verticalAlignment = Alignment.CenterVertically,
        modifier = modifier,
    ) {
        LightIcon(
            icon = if (isPlaying) LightIcons.PAUSE else LightIcons.PLAY,
            size = 1.5f,
            contentDescription = if (isPlaying) "Pause" else "Play",
            modifier = Modifier.lightClickable {
                if (!prepared) return@lightClickable
                if (isPlaying) mediaPlayer.pause() else mediaPlayer.start()
                isPlaying = !isPlaying
                remainingMs = (durationMs - mediaPlayer.currentPosition).coerceIn(0L, durationMs)
            },
        )
        // Fixed-size slot regardless of whether there's progress to rewind —
        // conditionally omitting this box instead would change the row's
        // total width right as playback starts, which shifts every icon
        // before it under the message bubble's end-alignment (see
        // MessageRow). Emptied out (and made unclickable) rather than hidden
        // via alpha so it doesn't eat a click while absent.
        Box(
            modifier = Modifier
                .padding(start = 8.dp)
                .size(1.5f.gridUnitsAsDp()),
            contentAlignment = Alignment.Center,
        ) {
            // Only once there's actual progress to discard — matches "after
            // it has started" rather than cluttering an untouched message.
            if (remainingMs < durationMs) {
                LightIcon(
                    icon = LightIcons.REWIND,
                    size = 1.5f,
                    contentDescription = "Restart from beginning",
                    modifier = Modifier.lightClickable {
                        mediaPlayer.seekTo(0)
                        remainingMs = durationMs
                    },
                )
            }
        }
        // Fixed width + monospace digits so neither a changing digit count
        // nor per-glyph width differences (e.g. "1" vs "0") reflow the row.
        LightText(
            text = formatDuration(remainingMs / 1000),
            variant = LightTextVariant.Copy,
            monospace = true,
            modifier = Modifier
                .padding(start = 8.dp)
                .width(AUDIO_DURATION_TEXT_WIDTH_GRID_UNITS.gridUnitsAsDp()),
        )
    }
}

private const val AUDIO_DURATION_TEXT_WIDTH_GRID_UNITS = 3.5f

private const val AUDIO_POSITION_POLL_MS = 200L

private fun formatDuration(totalSeconds: Long): String {
    val minutes = totalSeconds / 60
    val seconds = totalSeconds % 60
    return "%d:%02d".format(minutes, seconds)
}

// Decodes an image message's file off the main thread — core writes it
// relative to context.filesDir, the subprocess's working dir (see
// CoreProcess.kt).
@Composable
private fun rememberDecodedImage(relativePath: String): State<ImageBitmap?> {
    val context = LocalContext.current
    return produceState<ImageBitmap?>(initialValue = null, relativePath) {
        value = withContext(Dispatchers.IO) {
            BitmapFactory.decodeFile(File(context.filesDir, relativePath).absolutePath)?.asImageBitmap()
        }
    }
}
