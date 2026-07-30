package com.matheusmortatti.lightwhatsapp

import android.os.Bundle
import androidx.activity.ComponentActivity
import androidx.activity.compose.setContent
import androidx.compose.foundation.Image
import androidx.compose.foundation.background
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.runtime.Composable
import androidx.compose.runtime.collectAsState
import androidx.compose.runtime.getValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.unit.dp
import androidx.lifecycle.viewmodel.compose.viewModel
import com.thelightphone.sdk.ui.LightText
import com.thelightphone.sdk.ui.LightTextVariant
import com.thelightphone.sdk.ui.LightTheme
import com.thelightphone.sdk.ui.LightThemeController
import com.thelightphone.sdk.ui.LightThemeTokens

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

        when (val s = state) {
            is LoginState.Idle -> LightText(
                text = "Waiting for QR code...",
                variant = LightTextVariant.Copy,
                lighten = true,
            )

            is LoginState.ShowingQr -> {
                Image(
                    bitmap = s.bitmap,
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

            is LoginState.Connected -> LightText(
                text = "Connected as ${s.jid}",
                variant = LightTextVariant.Copy,
            )

            is LoginState.Error -> LightText(
                text = "Error: ${s.message}",
                variant = LightTextVariant.Copy,
            )
        }
    }
}
