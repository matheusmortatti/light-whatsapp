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

    private val _state = MutableStateFlow<LoginState>(LoginState.Idle)
    val state: StateFlow<LoginState> = _state.asStateFlow()

    init {
        viewModelScope.launch {
            CoreProcess(getApplication()).events().collect { event ->
                _state.value = when (event) {
                    is CoreEvent.Qr -> LoginState.ShowingQr(encodeQr(event.code))
                    is CoreEvent.Connected -> LoginState.Connected(event.jid)
                    is CoreEvent.LoggedOut -> LoginState.Idle
                    is CoreEvent.Error -> LoginState.Error(event.message)
                }
            }
        }
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
