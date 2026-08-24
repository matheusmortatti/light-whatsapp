package com.matheusmortatti.lightwhatsapp

import kotlin.test.Test
import kotlin.test.assertEquals

private fun testMessage(id: String, timestamp: Long, text: String = id) = Message(
    id = id,
    timestamp = timestamp,
    fromMe = false,
    status = null,
    senderName = null,
    type = "text",
    text = text,
    imagePath = null,
    audioPath = null,
    audioSeconds = 0,
    videoPath = null,
    videoSeconds = 0,
    isGif = false,
    stickerPath = null,
    stickerIsAnimated = false,
    pollOptions = emptyList(),
    pollSelectableCount = 0,
    pollVotes = emptyList(),
    imageFailed = false,
    audioFailed = false,
    videoFailed = false,
    stickerFailed = false,
    reactions = emptyList(),
    quotedId = null,
    quotedFromMe = false,
    quotedSenderName = null,
    quotedType = null,
    quotedText = "",
)

class MergeMessagesTest {

    @Test
    fun `appends a new message and sorts by timestamp`() {
        val current = listOf(testMessage("a", 1), testMessage("b", 2))
        val result = mergeMessages(current, listOf(testMessage("c", 3)))
        assertEquals(listOf("a", "b", "c"), result.map { it.id })
    }

    @Test
    fun `replaces an existing id in place without reordering`() {
        val current = listOf(testMessage("a", 1), testMessage("b", 2), testMessage("c", 3))
        val result = mergeMessages(current, listOf(testMessage("b", 2, text = "edited")))
        assertEquals(listOf("a", "b", "c"), result.map { it.id })
        assertEquals("edited", result[1].text)
    }

    // Regression: core's full "messages" snapshot (handleOpenChat) is read
    // before per-message mention resolution runs, so it can lag behind a
    // live message that arrived — and was already merged in via its own
    // message_update — while that snapshot was in flight. Applying the
    // snapshot must not erase messages the client already knows about that
    // the snapshot simply hasn't caught up to yet.
    @Test
    fun `stale full snapshot does not erase messages merged in after it was captured`() {
        val current = listOf(
            testMessage("a", 1),
            testMessage("b", 2),
            testMessage("c", 3),
            testMessage("d", 4), // arrived live via message_update, ahead of the snapshot below
        )
        val staleSnapshot = listOf(testMessage("a", 1), testMessage("b", 2), testMessage("c", 3))

        val result = mergeMessages(current, staleSnapshot)

        assertEquals(listOf("a", "b", "c", "d"), result.map { it.id })
    }
}
