package com.matheusmortatti.lightwhatsapp

import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent

/**
 * Inert marker so LightOS can discover this app the same way it discovers
 * light-sdk Tools: PackageManager.queryBroadcastReceivers() against
 * ACTION_SDK_MARKER (see sdk/server's LightSdkServer.queryInstalledClients).
 * This app isn't built with the light-sdk Gradle plugin (see PROJECT.md's
 * decision log), so that receiver isn't generated for us automatically —
 * this is the one piece of it we add back by hand. Never actually
 * dispatched to; existence + the manifest declaration below is the whole
 * contract, mirroring sdk/client's own LightSdkReceiver.
 */
class SdkMarkerReceiver : BroadcastReceiver() {
    override fun onReceive(context: Context, intent: Intent?) {
        // No-op — see class doc.
    }
}
