package com.matheusmortatti.lightwhatsapp

import android.graphics.BitmapFactory
import android.os.Bundle
import androidx.activity.ComponentActivity
import androidx.activity.compose.BackHandler
import androidx.activity.compose.setContent
import androidx.compose.foundation.Image
import androidx.compose.foundation.background
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.text.input.rememberTextFieldState
import androidx.compose.runtime.Composable
import androidx.compose.runtime.collectAsState
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.setValue
import androidx.compose.runtime.State
import androidx.compose.runtime.produceState
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.ImageBitmap
import androidx.compose.ui.graphics.asImageBitmap
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import androidx.lifecycle.viewmodel.compose.viewModel
import com.thelightphone.sdk.ui.LightBarButton
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
import com.thelightphone.sdk.ui.lightClickable
import java.io.File
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.flow.MutableStateFlow
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
) {
    // Keyed on chat.jid so navigating to a different chat doesn't inherit
    // this one's open-editor state.
    var composing by rememberSaveable(chat.jid) { mutableStateOf(false) }
    BackHandler(onBack = { if (composing) composing = false else onBack() })

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

        LightTextField(
            label = "Message",
            value = "",
            placeholder = "Tap to type a message",
            onClick = { composing = true },
            modifier = Modifier
                .fillMaxWidth()
                .padding(horizontal = 24.dp, vertical = 12.dp),
        )
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

        if (message.type == "image") {
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
        } else {
            LightText(text = message.text, variant = LightTextVariant.Copy)
        }
    }
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
