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
import androidx.compose.material3.Text
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.ImageBitmap
import androidx.compose.ui.graphics.asImageBitmap
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.text.TextStyle
import androidx.compose.ui.text.font.FontFamily
import androidx.compose.ui.text.style.TextAlign
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.isSpecified
import androidx.core.content.ContextCompat
import androidx.core.view.WindowCompat
import androidx.core.view.WindowInsetsCompat
import androidx.core.view.WindowInsetsControllerCompat
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
import com.thelightphone.sdk.ui.designVerticalPxToSp
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

        // Match LightActivity's chrome: no status/nav bars, swipe to reveal
        // transiently. Plain ComponentActivity doesn't get this for free.
        WindowCompat.setDecorFitsSystemWindows(window, false)
        val controller = WindowInsetsControllerCompat(window, window.decorView)
        controller.hide(WindowInsetsCompat.Type.systemBars())
        controller.systemBarsBehavior = WindowInsetsControllerCompat.BEHAVIOR_SHOW_TRANSIENT_BARS_BY_SWIPE

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
                text = "Loading...",
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
    // Fullscreen text entry (searching) vs. an applied filter (query) are
    // tracked separately, same split as ChatDetailScreen's composing flow:
    // the keyboard editor closes on submit, but the filter it produced stays
    // applied until explicitly cleared.
    var searching by rememberSaveable { mutableStateOf(false) }
    var query by rememberSaveable { mutableStateOf("") }

    BackHandler(enabled = searching || query.isNotBlank()) {
        if (searching) searching = false else query = ""
    }

    if (searching) {
        val textState = rememberTextFieldState(query)
        val keyboardOptionsFlow = remember { MutableStateFlow(defaultKeyboardOptions()) }
        LightTextInputEditor(
            title = "Search",
            state = textState,
            keyboardOptionsFlow = keyboardOptionsFlow,
            submitLabel = "SEARCH",
            singleLine = true,
            onSubmit = { text ->
                query = text.toString().trim()
                searching = false
            },
            onBack = { searching = false },
            modifier = Modifier.background(LightThemeTokens.colors.background),
        )
        return
    }

    val filteredChats = remember(chats, query) {
        if (query.isBlank()) chats else chats.filter { it.name.contains(query, ignoreCase = true) }
    }

    Column(
        modifier = Modifier
            .fillMaxSize()
            .background(LightThemeTokens.colors.background),
    ) {
        LightTopBar(
            center = LightTopBarCenter.Text("WhatsApp"),
            rightButton = if (query.isNotBlank()) {
                LightBarButton.LightIcon(icon = LightIcons.CLOSE, onClick = { query = "" })
            } else {
                LightBarButton.LightIcon(icon = LightIcons.SEARCH, onClick = { searching = true })
            },
            modifier = Modifier.padding(horizontal = 16.dp, vertical = 8.dp),
        )

        if (query.isNotBlank()) {
            LightText(
                text = "Results for \"$query\"",
                variant = LightTextVariant.Detail,
                lighten = true,
                maxLines = 1,
                overflow = TextOverflow.Ellipsis,
                modifier = Modifier.padding(horizontal = 32.dp, vertical = 4.dp),
            )
        }

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
        } else if (filteredChats.isEmpty()) {
            Box(
                modifier = Modifier
                    .weight(1f)
                    .fillMaxWidth(),
                contentAlignment = Alignment.Center,
            ) {
                LightText(
                    text = "No chats found",
                    variant = LightTextVariant.Copy,
                    lighten = true,
                )
            }
        } else {
            LightScrollView(
                modifier = Modifier
                    .weight(1f)
                    .fillMaxWidth(),
            ) {
                filteredChats.forEach { chat ->
                    ChatRow(
                        chat = chat,
                        modifier = Modifier
                            .lightClickable { onChatClick(chat) }
                            .padding(horizontal = 32.dp, vertical = 12.dp),
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
                    .fillMaxWidth(),
                scrollState = scrollState,
            ) {
                messages.forEachIndexed { index, message ->
                    val previous = messages.getOrNull(index - 1)
                    val sameSender = previous != null &&
                        previous.fromMe == message.fromMe &&
                        (!chat.isGroup || previous.senderName == message.senderName)
                    val withinClusterWindow = previous != null &&
                        (message.timestamp - previous.timestamp) <= MESSAGE_CLUSTER_WINDOW_SECONDS
                    val dateChanged = previous == null || !isSameLocalDate(previous.timestamp, message.timestamp)
                    val showHeader = dateChanged || !(sameSender && withinClusterWindow)
                    if (dateChanged) {
                        DateSeparator(
                            timestampSeconds = message.timestamp,
                            modifier = Modifier.padding(
                                start = 24.dp,
                                end = 24.dp,
                                top = if (index == 0) 0.dp else 16.dp,
                            ),
                        )
                    }
                    MessageRow(
                        message = message,
                        isGroup = chat.isGroup,
                        chatName = chat.name,
                        showHeader = showHeader,
                        modifier = Modifier.padding(
                            start = 24.dp,
                            end = 24.dp,
                            // Must clear Copy's own wrapped-line leading
                            // (fontSize * 1.5 lineHeight, ~10-12dp on this
                            // screen) or separate clustered messages read as
                            // one wrapped message.
                            top = if (showHeader) 14.dp else 9.dp,
                        ),
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

// Consecutive messages from the same sender within this window are
// clustered under one header; a longer gap starts a fresh one even if the
// sender hasn't changed.
private const val MESSAGE_CLUSTER_WINDOW_SECONDS = 5 * 60L

private val messageTimeFormatter = java.time.format.DateTimeFormatter.ofPattern("HH:mm")

// message.timestamp is Unix seconds (see core/main.go's handleMessage).
private fun formatMessageTime(timestampSeconds: Long): String =
    java.time.Instant.ofEpochSecond(timestampSeconds)
        .atZone(java.time.ZoneId.systemDefault())
        .format(messageTimeFormatter)

private fun localDateOf(timestampSeconds: Long): java.time.LocalDate =
    java.time.Instant.ofEpochSecond(timestampSeconds)
        .atZone(java.time.ZoneId.systemDefault())
        .toLocalDate()

private fun isSameLocalDate(aSeconds: Long, bSeconds: Long): Boolean =
    localDateOf(aSeconds) == localDateOf(bSeconds)

private val messageDateFormatter = java.time.format.DateTimeFormatter.ofPattern("EEEE, MMM d")

private fun formatMessageDate(timestampSeconds: Long): String {
    val date = localDateOf(timestampSeconds)
    val today = java.time.LocalDate.now(java.time.ZoneId.systemDefault())
    return when (date) {
        today -> "Today"
        today.minusDays(1) -> "Yesterday"
        else -> date.format(messageDateFormatter)
    }
}

@Composable
private fun DateSeparator(timestampSeconds: Long, modifier: Modifier = Modifier) {
    Box(modifier = modifier.fillMaxWidth(), contentAlignment = Alignment.Center) {
        ChatMetaText(text = formatMessageDate(timestampSeconds))
    }
}

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
private fun MessageRow(
    message: Message,
    isGroup: Boolean,
    chatName: String,
    showHeader: Boolean,
    modifier: Modifier = Modifier,
) {
    val bodyAlign = if (message.fromMe) TextAlign.End else TextAlign.Start
    Column(
        modifier = modifier.fillMaxWidth(),
        horizontalAlignment = if (message.fromMe) Alignment.End else Alignment.Start,
    ) {
        // Header (sender + time) is skipped for messages clustered with the
        // one before them — long messages wrap across the full line width,
        // which erases the left/right alignment cue, so the header is what
        // actually tells you who sent it and when.
        if (showHeader) {
            val senderLabel = when {
                message.fromMe -> "You"
                isGroup -> message.senderName?.takeIf { it.isNotBlank() } ?: chatName
                else -> chatName
            }
            Row {
                ChatMetaText(text = senderLabel)
                ChatMetaText(text = "  " + formatMessageTime(message.timestamp))
            }
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
                        MessageBodyText(text = "[Photo]", lighten = true, align = bodyAlign)
                    }
                } else {
                    MessageBodyText(text = "[Photo]", lighten = true, align = bodyAlign)
                }
                if (message.text.isNotBlank()) {
                    MessageBodyText(text = message.text, align = bodyAlign)
                }
            }

            "audio" -> {
                val path = message.audioPath
                if (path != null) {
                    AudioMessageRow(relativePath = path, seconds = message.audioSeconds)
                } else {
                    MessageBodyText(text = "[Voice message]", lighten = true, align = bodyAlign)
                }
            }

            else -> MessageBodyText(text = message.text, align = bodyAlign)
        }
    }
}

// Copy's own line-height (fontSize * 1.5, see LightTheme.kt) is tuned for
// short standalone lines, not paragraphs — at that ratio, the gap between
// two wrapped lines of ONE message reads about as big as the gap we put
// between two separate clustered messages, so lines of a single message
// look like distinct messages. Message bodies get their own tighter
// line-height instead, kept local to this screen rather than changed on
// the shared Copy token (which other Light SDK consumers rely on).
private const val MESSAGE_LINE_HEIGHT_MULTIPLIER = 1.15f

// Copy's full size is tuned for short standalone lines elsewhere in the app;
// message bubbles are dense and small-screen space is scarce, so bodies run
// smaller than Copy.
private const val MESSAGE_FONT_SCALE = 0.75f

// Sender/timestamp headers and date separators are scaled by the same
// factor as the body (see MESSAGE_FONT_SCALE) so the whole chat screen
// shrinks together and keeps its original size relationship (Fine was
// already smaller than Copy before either was scaled down).
private const val HEADER_FONT_SCALE = MESSAGE_FONT_SCALE

// Applies a uniform size scale to a design-token TextStyle, working in the
// same raw design-px units the token's fontSize/lineHeight/letterSpacing
// are authored in (see designVerticalPxToSp) rather than already-resolved
// sp, so it composes correctly with LightTheme's screen-height scaling.
// lineHeightMultiplier overrides the token's own fontSize:lineHeight ratio
// when a tighter (or looser) one is needed at the new size.
@Composable
private fun scaledStyle(base: TextStyle, scale: Float, lineHeightMultiplier: Float? = null): TextStyle {
    val scaledFontSizePx = base.fontSize.value * scale
    val ratio = lineHeightMultiplier
        ?: if (base.lineHeight.isSpecified) base.lineHeight.value / base.fontSize.value else 1f
    val scaledLetterSpacing = if (base.letterSpacing.isSpecified) {
        (base.letterSpacing.value * scale).designVerticalPxToSp()
    } else {
        base.letterSpacing
    }
    return base.copy(
        fontSize = scaledFontSizePx.designVerticalPxToSp(),
        lineHeight = (scaledFontSizePx * ratio).designVerticalPxToSp(),
        letterSpacing = scaledLetterSpacing,
    )
}

// align matters beyond cosmetics here: a wrapping multi-line message measures
// at the FULL available width (Compose sizes a soft-wrapped Text to its
// constraint, not its longest line), so Column's End alignment alone doesn't
// pull a sent message's lines to the right — each line still defaults to
// TextAlign.Start inside that full-width box unless told otherwise.
@Composable
private fun MessageBodyText(
    text: String,
    modifier: Modifier = Modifier,
    lighten: Boolean = false,
    align: TextAlign = TextAlign.Start,
) {
    val style = scaledStyle(LightThemeTokens.typography.copy, MESSAGE_FONT_SCALE, MESSAGE_LINE_HEIGHT_MULTIPLIER)
        .copy(textAlign = align)
    Text(
        text = text,
        modifier = modifier,
        color = if (lighten) LightThemeTokens.colors.contentSecondary else LightThemeTokens.colors.content,
        style = style,
    )
}

// Sender name / timestamp header above a message, and the date separator
// between days — both were LightTextVariant.Fine, scaled down in step with
// MessageBodyText (see HEADER_FONT_SCALE).
@Composable
private fun ChatMetaText(text: String, modifier: Modifier = Modifier) {
    val style = scaledStyle(LightThemeTokens.typography.fine, HEADER_FONT_SCALE)
    Text(
        text = text,
        modifier = modifier,
        color = LightThemeTokens.colors.contentSecondary,
        style = style,
    )
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
            size = AUDIO_ICON_SIZE_GRID_UNITS,
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
                .size(AUDIO_ICON_SIZE_GRID_UNITS.gridUnitsAsDp()),
            contentAlignment = Alignment.Center,
        ) {
            // Only once there's actual progress to discard — matches "after
            // it has started" rather than cluttering an untouched message.
            if (remainingMs < durationMs) {
                LightIcon(
                    icon = LightIcons.REWIND,
                    size = AUDIO_ICON_SIZE_GRID_UNITS,
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
        // Deliberately a notch above MessageBodyText's scale — it's the one
        // number on the row doing double duty as a label, so it reads best
        // slightly bigger than surrounding prose rather than matching it.
        val durationStyle = scaledStyle(LightThemeTokens.typography.copy, AUDIO_DURATION_FONT_SCALE)
            .copy(fontFamily = FontFamily.Monospace, textAlign = TextAlign.End)
        Text(
            text = formatDuration(remainingMs / 1000),
            style = durationStyle,
            color = LightThemeTokens.colors.content,
            maxLines = 1,
            modifier = Modifier
                .padding(start = 8.dp)
                .width(AUDIO_DURATION_TEXT_WIDTH_GRID_UNITS.gridUnitsAsDp()),
        )
    }
}

// Wide enough to fit "0:00"-"99:59" in monospace with slack for real-device
// font metrics (the real LP3's system monospace renders slightly wider than
// the emulator's, which was clipping/wrapping the last digit onto a second
// line). maxLines = 1 on the Text below is the hard backstop.
private const val AUDIO_DURATION_TEXT_WIDTH_GRID_UNITS = 4.5f

private const val AUDIO_DURATION_FONT_SCALE = 0.85f

// Play/rewind were originally sized (1.5 grid units) to sit next to a
// full-Copy-size duration label; scaled down in step with
// AUDIO_DURATION_FONT_SCALE so they stay visually balanced against it
// instead of towering over the now-smaller text.
private const val AUDIO_ICON_BASE_SIZE_GRID_UNITS = 1.5f
private const val AUDIO_ICON_SIZE_GRID_UNITS = AUDIO_ICON_BASE_SIZE_GRID_UNITS * AUDIO_DURATION_FONT_SCALE

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
