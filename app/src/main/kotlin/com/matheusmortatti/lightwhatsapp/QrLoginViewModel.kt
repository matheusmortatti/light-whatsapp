package com.matheusmortatti.lightwhatsapp

import android.app.Application
import android.graphics.Bitmap
import android.graphics.Color
import androidx.compose.ui.graphics.ImageBitmap
import androidx.compose.ui.graphics.asImageBitmap
import androidx.lifecycle.AndroidViewModel
import androidx.lifecycle.viewModelScope
import com.google.zxing.BarcodeFormat
import com.google.zxing.qrcode.QRCodeWriter
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.launch

sealed class LoginState {
    object Idle : LoginState()
    data class ShowingQr(val bitmap: ImageBitmap) : LoginState()
    data class Connected(val jid: String) : LoginState()
    data class Error(val message: String) : LoginState()
}

/**
 * Starts core/'s subprocess on init and maps its event stream to
 * [LoginState]. The subprocess is scoped to this ViewModel: viewModelScope
 * is cancelled automatically on onCleared(), which cancels the collecting
 * coroutine, which triggers CoreProcess's awaitClose and kills the process.
 */
class QrLoginViewModel(application: Application) : AndroidViewModel(application) {

    // A single instance, kept for this ViewModel's lifetime: openChat() needs
    // to reach the same running subprocess that events() is collecting from.
    private val coreProcess = CoreProcess(application)

    private val _state = MutableStateFlow<LoginState>(LoginState.Idle)
    val state: StateFlow<LoginState> = _state.asStateFlow()

    // Chats can arrive before "connected" (core replays a cached list from a
    // prior run on startup) and keep arriving in chunks after, so it's kept
    // separate from LoginState rather than nested inside Connected.
    private val _chats = MutableStateFlow<List<Chat>>(emptyList())
    val chats: StateFlow<List<Chat>> = _chats.asStateFlow()

    // The chat currently open for viewing, or null when showing the chat list.
    private val _selectedChat = MutableStateFlow<Chat?>(null)
    val selectedChat: StateFlow<Chat?> = _selectedChat.asStateFlow()

    private val _messages = MutableStateFlow<List<Message>>(emptyList())
    val messages: StateFlow<List<Message>> = _messages.asStateFlow()

    init {
        viewModelScope.launch {
            coreProcess.events().collect { event ->
                when (event) {
                    is CoreEvent.Qr -> _state.value = LoginState.ShowingQr(encodeQr(event.code))
                    is CoreEvent.Connected -> _state.value = LoginState.Connected(event.jid)
                    is CoreEvent.LoggedOut -> {
                        _state.value = LoginState.Idle
                        _chats.value = emptyList()
                        _selectedChat.value = null
                        _messages.value = emptyList()
                    }
                    is CoreEvent.Error -> _state.value = LoginState.Error(event.message)
                    is CoreEvent.Chats -> _chats.value = event.chats
                    is CoreEvent.Messages -> {
                        if (event.jid == _selectedChat.value?.jid) {
                            _messages.value = event.messages
                        }
                    }
                }
            }
        }
    }

    /** Opens a chat for viewing: clears any previously shown messages and asks core to (re-)send this one's. */
    fun openChat(chat: Chat) {
        _selectedChat.value = chat
        _messages.value = emptyList()
        coreProcess.openChat(chat.jid)
    }

    /** Returns to the chat list. */
    fun closeChat() {
        _selectedChat.value?.let { coreProcess.closeChat(it.jid) }
        _selectedChat.value = null
        _messages.value = emptyList()
    }

    /** Sends a text message to the currently open chat. A no-op if no chat is open. */
    fun sendMessage(text: String) {
        val jid = _selectedChat.value?.jid ?: return
        coreProcess.sendMessage(jid, text)
    }

    /**
     * Sends a recorded voice message to the currently open chat. [audioPath]
     * must be relative to the app's files dir (see CoreProcess.sendAudio).
     * A no-op if no chat is open.
     */
    fun sendAudio(audioPath: String, durationMs: Long) {
        val jid = _selectedChat.value?.jid ?: return
        coreProcess.sendAudio(jid, audioPath, durationMs)
    }

    private fun encodeQr(text: String, size: Int = 512): ImageBitmap {
        val matrix = QRCodeWriter().encode(text, BarcodeFormat.QR_CODE, size, size)
        val bitmap = Bitmap.createBitmap(size, size, Bitmap.Config.RGB_565)
        for (x in 0 until size) {
            for (y in 0 until size) {
                bitmap.setPixel(x, y, if (matrix[x, y]) Color.BLACK else Color.WHITE)
            }
        }
        return bitmap.asImageBitmap()
    }
}
