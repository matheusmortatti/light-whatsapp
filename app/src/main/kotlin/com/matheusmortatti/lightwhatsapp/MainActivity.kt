package com.matheusmortatti.lightwhatsapp

import android.Manifest
import android.content.pm.PackageManager
import android.graphics.Bitmap
import android.graphics.BitmapFactory
import android.media.MediaMetadataRetriever
import android.media.MediaPlayer
import android.os.Bundle
import android.os.SystemClock
import android.util.Log
import android.util.LruCache
import android.widget.VideoView
import androidx.activity.ComponentActivity
import androidx.activity.compose.BackHandler
import androidx.activity.compose.rememberLauncherForActivityResult
import androidx.activity.compose.setContent
import androidx.activity.result.contract.ActivityResultContracts
import androidx.compose.foundation.Image
import androidx.compose.foundation.background
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.lazy.LazyListState
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.lazy.itemsIndexed
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
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.ImageBitmap
import androidx.compose.ui.graphics.asImageBitmap
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.platform.LocalDensity
import androidx.compose.ui.text.TextStyle
import androidx.compose.ui.text.font.FontFamily
import androidx.compose.ui.text.style.TextAlign
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.Dp
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.isSpecified
import androidx.compose.ui.viewinterop.AndroidView
import androidx.core.content.ContextCompat
import androidx.core.view.WindowCompat
import androidx.core.view.WindowInsetsCompat
import androidx.core.view.WindowInsetsControllerCompat
import androidx.lifecycle.viewmodel.compose.viewModel
import com.thelightphone.sdk.ui.LightBarButton
import com.thelightphone.sdk.ui.LightIcon
import com.thelightphone.sdk.ui.LightIcons
import com.thelightphone.sdk.ui.LightLazyScrollView
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
import java.util.UUID
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.delay
import kotlinx.coroutines.flow.MutableStateFlow
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
    val syncing by viewModel.syncing.collectAsState()

    when {
        state !is LoginState.Connected -> LoginScreen(state = state)
        selectedChat != null -> ChatDetailScreen(
            chat = selectedChat!!,
            messages = messages,
            onBack = viewModel::closeChat,
            onSend = viewModel::sendMessage,
            onSendAudio = viewModel::sendAudio,
            onSendReaction = viewModel::sendReaction,
        )
        else -> ChatListScreen(chats = chats, syncing = syncing, onChatClick = viewModel::openChat)
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
private fun ChatListScreen(chats: List<Chat>, syncing: Boolean, onChatClick: (Chat) -> Unit) {
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
            leftButton = if (syncing) {
                LightBarButton.LightIcon(
                    icon = LightIcons.REFRESH,
                    onClick = null,
                    contentDescription = "Updating chats",
                )
            } else {
                null
            },
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
            LightLazyScrollView(
                modifier = Modifier
                    .weight(1f)
                    .fillMaxWidth(),
                uniformItemHeightGridUnits = CHAT_ROW_HEIGHT_GRID_UNITS,
            ) {
                items(filteredChats, key = { it.jid }) { chat ->
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

// A single-line Copy row (30sp text, lineHeight 1.5x) plus its 12dp top/bottom
// padding — used only for LightLazyScrollView's scrollbar-geometry estimate,
// not enforced as an actual row height, so a little drift here just means the
// scrollbar thumb tracks approximately rather than exactly.
private const val CHAT_ROW_HEIGHT_GRID_UNITS = 2.6f

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
    onSendReaction: (String, String) -> Unit,
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
    var recordingFailed by remember(chat.jid) { mutableStateOf(false) }
    val voiceRecorder = remember { VoiceRecorder(context) }
    DisposableEffect(Unit) { onDispose { voiceRecorder.cancel() } }

    // Path (relative to context.filesDir) + loop flag of the video/gif
    // currently open in the full-screen player, or null if none. Not
    // rememberSaveable — re-opening from the thumbnail on process restart
    // is cheap and there's no live player state worth restoring.
    var playingVideo by remember(chat.jid) { mutableStateOf<Pair<String, Boolean>?>(null) }

    // The message whose reaction picker is currently open, or null. Not
    // rememberSaveable — same reasoning as playingVideo, re-opening it is
    // one tap away and there's no meaningful state to restore.
    var reactingTo by remember(chat.jid) { mutableStateOf<Message?>(null) }

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
            reactingTo != null -> reactingTo = null
            playingVideo != null -> playingVideo = null
            composing -> composing = false
            recordingFailed -> recordingFailed = false
            recording -> cancelRecording()
            else -> onBack()
        }
    })

    // Fresh state per chat so switching chats doesn't inherit scroll position.
    val listState = remember(chat.jid) { LazyListState() }
    // Tracks how many messages were on screen the last time we auto-scrolled,
    // so a later growth (e.g. a new incoming message) only re-chases the
    // bottom if the user was already viewing up to the previous last message
    // — not if they'd scrolled up to read history.
    var lastAppliedCount by remember(chat.jid) { mutableStateOf(0) }
    LaunchedEffect(chat.jid, messages.size) {
        val size = messages.size
        if (size == 0) return@LaunchedEffect
        val previousLastIndex = lastAppliedCount - 1
        val wasAtBottom = lastAppliedCount == 0 ||
            (listState.layoutInfo.visibleItemsInfo.lastOrNull()?.index ?: -1) >= previousLastIndex
        if (wasAtBottom) {
            listState.scrollToItem(size - 1)
        }
        lastAppliedCount = size
    }

    // Catches a message growing IN PLACE after we've already pinned to it —
    // e.g. an image message's row is short ("[Photo]" placeholder text)
    // until its bitmap finishes decoding async (see rememberDecodedImage),
    // at which point the row grows and the true bottom moves further down
    // than where we last scrolled to. The message-count effect above can't
    // see this: nothing was added, an existing item just got taller.
    //
    // Distinguished from the user manually scrolling away by watching the
    // viewport's top anchor (firstVisibleItemIndex/ScrollOffset): a manual
    // scroll always moves it, a later item silently growing off-screen
    // doesn't. Only chase when the anchor held steady since the last check
    // and there's now unseen content below (canScrollForward).
    LaunchedEffect(chat.jid) {
        var anchorIndex = -1
        var anchorOffset = -1
        snapshotFlow {
            Triple(listState.firstVisibleItemIndex, listState.firstVisibleItemScrollOffset, listState.canScrollForward)
        }.collect { (firstIndex, firstOffset, canScrollForward) ->
            val anchorUnchanged = firstIndex == anchorIndex && firstOffset == anchorOffset
            if (anchorUnchanged && canScrollForward) {
                val lastIndex = listState.layoutInfo.totalItemsCount - 1
                if (lastIndex >= 0) listState.scrollToItem(lastIndex)
            }
            anchorIndex = listState.firstVisibleItemIndex
            anchorOffset = listState.firstVisibleItemScrollOffset
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

    if (recordingFailed) {
        RecordingFailedScreen(
            chatName = chat.name,
            onDismiss = { recordingFailed = false },
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
                // Guard against a double SEND tap: if a first tap already
                // stopped the recording (and sent it) before this screen was
                // dismissed, a second tap lands here with recording already
                // false. Without this, voiceRecorder.stop()'s "nothing
                // active" null (same return value as a genuine platform
                // rejection) would incorrectly show the "wasn't sent"
                // screen for a message that actually sent fine.
                if (recording) {
                    val result = voiceRecorder.stop()
                    recording = false
                    if (result != null) {
                        val (file, durationMs) = result
                        onSendAudio(file.relativeTo(context.filesDir).path, durationMs)
                    } else {
                        recordingFailed = true
                    }
                }
            },
        )
        return
    }

    playingVideo?.let { (path, loop) ->
        VideoPlayerScreen(
            chatName = chat.name,
            relativePath = path,
            loop = loop,
            onClose = { playingVideo = null },
        )
        return
    }

    reactingTo?.let { target ->
        ReactionPickerScreen(
            message = target,
            chatName = chat.name,
            onPick = { emoji ->
                onSendReaction(target.id, emoji)
                reactingTo = null
            },
            onClose = { reactingTo = null },
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
            // Status (see MessageRow) only matters on the message you sent
            // most recently — WhatsApp's own ticks work the same way, older
            // sent messages aren't worth the visual noise. Explicitly null
            // in group chats (rather than relying on message.status being
            // empty there) — otherwise the last own message in a group
            // would still get its header forced visible below, for a
            // status suffix that never renders.
            val lastOwnMessageId = remember(messages, chat.isGroup) {
                if (chat.isGroup) null else messages.lastOrNull { it.fromMe && it.status != null }?.id
            }
            LightLazyScrollView(
                modifier = Modifier
                    .weight(1f)
                    .fillMaxWidth(),
                listState = listState,
                uniformItemHeightGridUnits = MESSAGE_ROW_HEIGHT_GRID_UNITS,
            ) {
                itemsIndexed(messages, key = { _, message -> message.id }) { index, message ->
                    val previous = messages.getOrNull(index - 1)
                    val sameSender = previous != null &&
                        previous.fromMe == message.fromMe &&
                        (!chat.isGroup || previous.senderName == message.senderName)
                    val withinClusterWindow = previous != null &&
                        (message.timestamp - previous.timestamp) <= MESSAGE_CLUSTER_WINDOW_SECONDS
                    val dateChanged = previous == null || !isSameLocalDate(previous.timestamp, message.timestamp)
                    val showHeader = dateChanged || !(sameSender && withinClusterWindow)
                    val showStatus = message.id == lastOwnMessageId
                    Column {
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
                            showHeader = showHeader || showStatus,
                            showStatus = showStatus,
                            onPlayVideo = { path, loop -> playingVideo = path to loop },
                            onReact = { reactingTo = it },
                            modifier = Modifier.padding(
                                start = 24.dp,
                                end = 24.dp,
                                // Must clear Copy's own wrapped-line leading
                                // (fontSize * 1.5 lineHeight, ~10-12dp on this
                                // screen) or separate clustered messages read as
                                // one wrapped message.
                                top = if (showHeader || showStatus) 14.dp else 9.dp,
                            ),
                        )
                    }
                }
            }
        }

        Row(
            verticalAlignment = Alignment.Bottom,
            modifier = Modifier
                .fillMaxWidth()
                .padding(horizontal = 24.dp, vertical = 4.dp),
        ) {
            LightTextField(
                label = null,
                value = "",
                placeholder = "Tap to type a message",
                onClick = { composing = true },
                modifier = Modifier.weight(1f),
            )
            LightIcon(
                icon = LightIcons.MICROPHONE,
                contentDescription = "Record a voice message",
                modifier = Modifier
                    .padding(start = 16.dp, bottom = 2.dp)
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

// Stickers are always small/square — a fixed size well under images' 200dp.
private val STICKER_SIZE_DP = 120.dp

private const val RECORDING_TIMER_TICK_MS = 200L

// Consecutive messages from the same sender within this window are
// clustered under one header; a longer gap starts a fresh one even if the
// sender hasn't changed.
private const val MESSAGE_CLUSTER_WINDOW_SECONDS = 5 * 60L

// Rough average of a clustered (headerless, one-line) message row, used only
// for LightLazyScrollView's scrollbar-geometry estimate — actual rows vary
// (headers, date separators, multi-line/image/audio bodies), so the thumb
// tracks approximately rather than exactly.
private const val MESSAGE_ROW_HEIGHT_GRID_UNITS = 2.2f

private val messageTimeFormatter = java.time.format.DateTimeFormatter.ofPattern("HH:mm")

// message.timestamp is Unix seconds (see core/main.go's handleMessage).
private fun formatMessageTime(timestampSeconds: Long): String =
    java.time.Instant.ofEpochSecond(timestampSeconds)
        .atZone(java.time.ZoneId.systemDefault())
        .format(messageTimeFormatter)

// message.status is only ever "sent" | "delivered" | "read" | null (see
// core/main.go's chatMessage.Status) — anything else (including null)
// renders no suffix at all.
private fun formatMessageStatus(status: String?): String? = when (status) {
    "sent" -> "Sent"
    "delivered" -> "Delivered"
    "read" -> "Read"
    else -> null
}

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
private fun RecordingFailedScreen(
    chatName: String,
    onDismiss: () -> Unit,
) {
    Column(
        modifier = Modifier
            .fillMaxSize()
            .background(LightThemeTokens.colors.background),
    ) {
        LightTopBar(
            leftButton = LightBarButton.LightIcon(icon = LightIcons.CLOSE, onClick = onDismiss),
            center = LightTopBarCenter.Text(chatName),
        )
        Box(
            modifier = Modifier
                .weight(1f)
                .fillMaxWidth(),
            contentAlignment = Alignment.Center,
        ) {
            LightText(
                text = "Voice message wasn't sent",
                variant = LightTextVariant.Copy,
                lighten = true,
            )
        }
    }
}

@Composable
private fun VideoPlayerScreen(
    chatName: String,
    relativePath: String,
    loop: Boolean,
    onClose: () -> Unit,
) {
    val context = LocalContext.current
    var playbackError by remember { mutableStateOf(false) }
    Column(
        modifier = Modifier
            .fillMaxSize()
            .background(LightThemeTokens.colors.background),
    ) {
        LightTopBar(
            leftButton = LightBarButton.LightIcon(icon = LightIcons.CLOSE, onClick = onClose),
            center = LightTopBarCenter.Text(chatName),
        )
        Box(
            modifier = Modifier
                .weight(1f)
                .fillMaxWidth(),
            contentAlignment = Alignment.Center,
        ) {
            if (playbackError) {
                LightText(
                    text = "Couldn't play this video",
                    variant = LightTextVariant.Copy,
                    lighten = true,
                )
            } else {
                AndroidView(
                    factory = { ctx ->
                        VideoView(ctx).apply {
                            setVideoPath(File(context.filesDir, relativePath).absolutePath)
                            setOnPreparedListener { mp ->
                                mp.isLooping = loop
                                start()
                            }
                            setOnErrorListener { _, what, extra ->
                                Log.w("MainActivity", "video playback error: what=$what extra=$extra for $relativePath")
                                playbackError = true
                                // Suppress VideoView's own AOSP error dialog — it would
                                // look foreign against LightOS's design language; the
                                // LightText above is the actual user-facing feedback.
                                true
                            }
                        }
                    },
                    modifier = Modifier.fillMaxSize(),
                )
            }
        }
    }
}

// WhatsApp's own standard quick-reaction set — see
// docs/superpowers/specs/2026-08-05-send-reactions-design.md for why this
// is the agreed breadth rather than a larger or fully open picker.
private val QUICK_REACTIONS = listOf("👍", "❤️", "😂", "😮", "😢", "🙏")

// A one-line summary of message shown atop the reaction picker. Reuses the
// same placeholder labels MessageRow renders for a not-yet-downloaded or
// non-text message, but does NOT mirror MessageRow's permanently-failed
// "unavailable" labels (e.g. "[Photo unavailable]") — a perma-failed photo
// still shows the plain "[Photo]" preview here; only the chat row itself
// distinguishes the failure state.
private fun messagePreviewText(message: Message): String = messagePreviewText(message.type, message.text)

// Same placeholder logic as the Message overload above, but usable on a
// synthetic (type, text) pair — specifically MessageRow's quoted-reply
// preview, which has a quotedType/quotedText but no full Message for the
// message it's quoting.
private fun messagePreviewText(type: String, text: String): String = when (type) {
    "image" -> text.ifBlank { "[Photo]" }
    "sticker" -> "[Sticker]"
    "video" -> text.ifBlank { "[Video]" }
    "gif" -> text.ifBlank { "[GIF]" }
    "audio" -> "[Voice message]"
    "poll" -> "[Poll] $text"
    "unsupported" -> "[Unsupported message]"
    else -> text
}

// WhatsApp reactions can arrive from different clients with or without a
// trailing variation selector (U+FE0F/U+FE0E) on the same visual glyph —
// e.g. a bare "heart" (U+2764) vs. "heart, emoji-style" (U+2764 U+FE0F).
// Comparisons against QUICK_REACTIONS' canonical (variation-selector) forms
// should treat those as equal; strip the selector on both sides before
// comparing. Only comparisons are normalized — QUICK_REACTIONS and whatever
// is passed to onPick/onSendReaction stay exactly as declared.
private fun String.withoutVariationSelectors(): String = filterNot { it == '️' || it == '︎' }

@Composable
private fun ReactionPickerScreen(
    message: Message,
    chatName: String,
    onPick: (emoji: String) -> Unit,
    onClose: () -> Unit,
) {
    val currentReaction = message.reactions.firstOrNull { it.fromMe }?.emoji
    Column(
        modifier = Modifier
            .fillMaxSize()
            .background(LightThemeTokens.colors.background),
    ) {
        LightTopBar(
            leftButton = LightBarButton.LightIcon(icon = LightIcons.CLOSE, onClick = onClose),
            center = LightTopBarCenter.Text(chatName),
        )
        LightText(
            text = messagePreviewText(message),
            variant = LightTextVariant.Copy,
            lighten = true,
            maxLines = 2,
            overflow = TextOverflow.Ellipsis,
            modifier = Modifier
                .fillMaxWidth()
                .padding(horizontal = 24.dp, vertical = 16.dp),
        )
        Row(
            modifier = Modifier
                .weight(1f)
                .fillMaxWidth()
                .padding(horizontal = 16.dp),
            horizontalArrangement = Arrangement.SpaceEvenly,
            verticalAlignment = Alignment.CenterVertically,
        ) {
            for (emoji in QUICK_REACTIONS) {
                val isCurrent = currentReaction != null &&
                    emoji.withoutVariationSelectors() == currentReaction.withoutVariationSelectors()
                Column(
                    horizontalAlignment = Alignment.CenterHorizontally,
                    modifier = Modifier.lightClickable {
                        onPick(if (isCurrent) "" else emoji)
                    },
                ) {
                    // Subtitle rather than Title — at Title size even one row
                    // of all 6 overflows the LP3's screen width (confirmed on
                    // real hardware: the last two, "😢" and "🙏", rendered
                    // entirely off-screen with no scroll available to reach
                    // them).
                    LightText(text = emoji, variant = LightTextVariant.Subtitle)
                    // A bar drawn below the glyph rather than LightText's
                    // built-in `underline` (TextDecoration.Underline): that
                    // decoration sits at a fixed offset from the font's text
                    // baseline, but a color emoji glyph draws much taller
                    // than that baseline expects, so the line cut across the
                    // glyph itself instead of sitting under it (confirmed on
                    // real hardware). Reserving the slot on every emoji
                    // (transparent when unselected) keeps every glyph at the
                    // same vertical position regardless of which one is
                    // marked.
                    Box(
                        modifier = Modifier
                            .padding(top = 4.dp)
                            .width(28.dp)
                            .height(2.dp)
                            .background(
                                if (isCurrent) {
                                    LightThemeTokens.colors.content
                                } else {
                                    Color.Transparent
                                },
                            ),
                    )
                }
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
    showStatus: Boolean,
    onPlayVideo: (path: String, loop: Boolean) -> Unit,
    onReact: (Message) -> Unit,
    modifier: Modifier = Modifier,
) {
    val bodyAlign = if (message.fromMe) TextAlign.End else TextAlign.Start
    Column(
        // Deliberately on the outer Column, so the tap target includes the
        // sender/time header rather than just the body below — a bigger tap
        // target, and the header has no competing action of its own.
        modifier = modifier
            .fillMaxWidth()
            .lightClickable { onReact(message) },
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
            val statusLabel = if (showStatus) formatMessageStatus(message.status) else null
            Row {
                ChatMetaText(text = senderLabel)
                ChatMetaText(text = "  " + formatMessageTime(message.timestamp))
                if (statusLabel != null) {
                    ChatMetaText(text = " · $statusLabel")
                }
            }
        }

        // Quote preview for a reply — inert (no tap-to-jump), inside the
        // row's existing onReact click target. Rendered below the row's own
        // header (if shown) and above its body, matching where a reply's
        // quote reads naturally: who/when this message is, what it's
        // replying to, then its own content.
        if (message.quotedType != null) {
            val quotedSenderLabel = if (message.quotedFromMe) {
                "Replying to you"
            } else {
                "Replying to " + (message.quotedSenderName ?: chatName)
            }
            ChatMetaText(text = quotedSenderLabel)
            MessageBodyText(
                text = messagePreviewText(message.quotedType, message.quotedText),
                lighten = true,
                align = bodyAlign,
                maxLines = 1,
                overflow = TextOverflow.Ellipsis,
                modifier = Modifier.fillMaxWidth(),
            )
        }

        when (message.type) {
            "image" -> {
                val path = message.imagePath
                if (path != null) {
                    val bitmap by rememberDecodedImage(path, targetSize = 200.dp, allowAlpha = false)
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
                } else if (message.imageFailed) {
                    MessageBodyText(text = "[Photo unavailable]", lighten = true, align = bodyAlign)
                } else {
                    MessageBodyText(text = "[Photo]", lighten = true, align = bodyAlign)
                }
                if (message.text.isNotBlank()) {
                    MessageBodyText(text = message.text, align = bodyAlign)
                }
            }

            "sticker" -> {
                val path = message.stickerPath
                if (path != null) {
                    val bitmap by rememberDecodedImage(path, targetSize = STICKER_SIZE_DP)
                    if (bitmap != null) {
                        Image(
                            bitmap = bitmap!!,
                            contentDescription = "Sticker",
                            modifier = Modifier
                                .size(STICKER_SIZE_DP)
                                .padding(bottom = 4.dp),
                        )
                    } else {
                        MessageBodyText(text = "[Sticker]", lighten = true, align = bodyAlign)
                    }
                } else if (message.stickerFailed) {
                    MessageBodyText(text = "[Sticker unavailable]", lighten = true, align = bodyAlign)
                } else {
                    MessageBodyText(text = "[Sticker]", lighten = true, align = bodyAlign)
                }
            }

            "video", "gif" -> {
                val path = message.videoPath
                if (path != null) {
                    val thumbnail by rememberDecodedVideoThumbnail(path)
                    Box(
                        modifier = Modifier
                            .size(200.dp)
                            .padding(bottom = 4.dp)
                            .lightClickable { onPlayVideo(path, message.isGif) },
                        contentAlignment = Alignment.Center,
                    ) {
                        if (thumbnail != null) {
                            Image(
                                bitmap = thumbnail!!,
                                contentDescription = if (message.isGif) "GIF" else "Video",
                                modifier = Modifier.size(200.dp),
                            )
                        }
                        LightIcon(
                            icon = LightIcons.PLAY,
                            size = 3f,
                            contentDescription = "Play",
                        )
                    }
                } else if (message.videoFailed) {
                    MessageBodyText(
                        text = if (message.isGif) "[GIF unavailable]" else "[Video unavailable]",
                        lighten = true,
                        align = bodyAlign,
                    )
                } else {
                    MessageBodyText(
                        text = if (message.isGif) "[GIF]" else "[Video]",
                        lighten = true,
                        align = bodyAlign,
                    )
                }
                if (message.text.isNotBlank()) {
                    MessageBodyText(text = message.text, align = bodyAlign)
                }
            }

            "audio" -> {
                val path = message.audioPath
                if (path != null) {
                    AudioMessageRow(relativePath = path, seconds = message.audioSeconds)
                } else if (message.audioFailed) {
                    MessageBodyText(text = "[Voice message unavailable]", lighten = true, align = bodyAlign)
                } else {
                    MessageBodyText(text = "[Voice message]", lighten = true, align = bodyAlign)
                }
            }

            "poll" -> {
                MessageBodyText(text = message.text, align = bodyAlign)
                if (message.pollOptions.isNotEmpty()) {
                    MessageBodyText(
                        text = formatPollOptions(message.pollOptions, message.pollVotes),
                        lighten = true,
                        align = bodyAlign,
                    )
                    val voterCount = message.pollVotes.size
                    ChatMetaText(text = if (voterCount == 1) "1 vote" else "$voterCount votes")
                }
            }

            "unsupported" -> MessageBodyText(
                text = "[Unsupported message: ${message.text}]",
                lighten = true,
                align = bodyAlign,
            )

            else -> MessageBodyText(text = message.text, align = bodyAlign)
        }

        if (message.reactions.isNotEmpty()) {
            ChatMetaText(text = formatReactions(message.reactions))
        }
    }
}

// Groups reactions by emoji (WhatsApp allows several people to react with
// the same one) and joins them, e.g. "👍 ❤️×2" — who reacted isn't shown,
// matching how WhatsApp itself only reveals that on long-press.
private fun formatReactions(reactions: List<Reaction>): String =
    reactions.groupingBy { it.emoji }.eachCount().entries.joinToString(" ") { (emoji, count) ->
        if (count > 1) "$emoji×$count" else emoji
    }

private const val POLL_BAR_SEGMENTS = 5

// Renders each poll option on its own line with a 5-segment bar scaled to
// the highest-voted option, e.g.:
//   ▸ Pepperoni  ■■■■□ 4
//   ▸ Mushroom   □□□□□ 0
// Counts come from flattening every voter's selectedOptions (a multi-select
// poll lets one voter count toward several options at once) — who voted for
// what isn't shown, matching the approved tally format (counts + bar, not
// per-voter identity).
private fun formatPollOptions(options: List<String>, votes: List<PollVote>): String {
    if (options.isEmpty()) return ""
    val counts = IntArray(options.size)
    for (vote in votes) {
        for (index in vote.selectedOptions) {
            if (index in counts.indices) counts[index]++
        }
    }
    val maxCount = counts.max()
    return options.indices.joinToString("\n") { i ->
        val count = counts[i]
        val filled = if (maxCount == 0) 0 else (count * POLL_BAR_SEGMENTS + maxCount - 1) / maxCount
        val bar = "■".repeat(filled) + "□".repeat(POLL_BAR_SEGMENTS - filled)
        "▸ ${options[i]}  $bar $count"
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
    maxLines: Int = Int.MAX_VALUE,
    overflow: TextOverflow = TextOverflow.Clip,
) {
    val style = scaledStyle(LightThemeTokens.typography.copy, MESSAGE_FONT_SCALE, MESSAGE_LINE_HEIGHT_MULTIPLIER)
        .copy(textAlign = align)
    Text(
        text = text,
        modifier = modifier,
        color = if (lighten) LightThemeTokens.colors.contentSecondary else LightThemeTokens.colors.content,
        style = style,
        maxLines = maxLines,
        overflow = overflow,
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
            } catch (e: Exception) {
                // setDataSource/prepare are documented to throw IOException,
                // IllegalStateException, IllegalArgumentException, and
                // SecurityException depending on the file/codec/device state
                // — any of them means this row just can't play, not a crash.
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

// Shared across every image/sticker row so scrolling a photo off-screen and
// back doesn't re-decode it from disk. Sized against heap budget rather than
// a fixed entry count since decoded size varies a lot between a downsampled
// sticker and a downsampled photo.
private val decodedImageCache = object : LruCache<String, ImageBitmap>(
    (Runtime.getRuntime().maxMemory() / 8).toInt()
) {
    override fun sizeOf(key: String, value: ImageBitmap): Int = value.width * value.height * 4
}

// Two-pass decode: read bounds first, then decode straight to the sample
// size the target display box needs, so a 12MP photo never gets fully
// decoded just to be drawn into a 200dp box.
private fun decodeSampledBitmap(file: File, reqWidthPx: Int, reqHeightPx: Int, allowAlpha: Boolean): Bitmap? {
    val bounds = BitmapFactory.Options().apply { inJustDecodeBounds = true }
    BitmapFactory.decodeFile(file.absolutePath, bounds)
    if (bounds.outWidth <= 0 || bounds.outHeight <= 0) return null

    var sampleSize = 1
    var halfWidth = bounds.outWidth / 2
    var halfHeight = bounds.outHeight / 2
    while (halfWidth / sampleSize >= reqWidthPx && halfHeight / sampleSize >= reqHeightPx) {
        sampleSize *= 2
    }

    val options = BitmapFactory.Options().apply {
        inSampleSize = sampleSize
        inPreferredConfig = if (allowAlpha) Bitmap.Config.ARGB_8888 else Bitmap.Config.RGB_565
    }
    return BitmapFactory.decodeFile(file.absolutePath, options)
}

// Decodes an image message's file off the main thread — core writes it
// relative to context.filesDir, the subprocess's working dir (see
// CoreProcess.kt). Downsamples to targetSize and caches the result in
// decodedImageCache, keyed on path+size so the same file at a different
// display size (shouldn't happen today, but stays correct if it does)
// doesn't collide.
@Composable
private fun rememberDecodedImage(relativePath: String, targetSize: Dp, allowAlpha: Boolean = true): State<ImageBitmap?> {
    val context = LocalContext.current
    val targetPx = with(LocalDensity.current) { targetSize.roundToPx() }
    val cacheKey = "$relativePath@$targetPx"
    return produceState<ImageBitmap?>(initialValue = decodedImageCache.get(cacheKey), cacheKey) {
        if (value == null) {
            value = withContext(Dispatchers.IO) {
                decodeSampledBitmap(File(context.filesDir, relativePath), targetPx, targetPx, allowAlpha)
                    ?.asImageBitmap()
                    ?.also { decodedImageCache.put(cacheKey, it) }
            }
        }
    }
}

// Decodes a video/gif message's first frame off the main thread, the same
// way rememberDecodedImage does for images — core writes the file relative
// to context.filesDir (see CoreProcess.kt).
@Composable
private fun rememberDecodedVideoThumbnail(relativePath: String): State<ImageBitmap?> {
    val context = LocalContext.current
    return produceState<ImageBitmap?>(initialValue = null, relativePath) {
        value = withContext(Dispatchers.IO) {
            val retriever = MediaMetadataRetriever()
            try {
                retriever.setDataSource(File(context.filesDir, relativePath).absolutePath)
                retriever.frameAtTime?.asImageBitmap()
            } catch (e: Exception) {
                Log.w("MainActivity", "failed to decode video thumbnail $relativePath", e)
                null
            } finally {
                retriever.release()
            }
        }
    }
}
