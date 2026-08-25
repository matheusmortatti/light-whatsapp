package com.matheusmortatti.lightwhatsapp

import kotlin.test.Test
import kotlin.test.assertEquals

class PollSelectionTest {

    @Test
    fun `tapping an unselected option adds it`() {
        assertEquals(listOf(0), nextPollSelection(current = emptyList(), tapped = 0, cap = 2))
    }

    @Test
    fun `tapping an already-selected option removes it`() {
        assertEquals(listOf(1), nextPollSelection(current = listOf(0, 1), tapped = 0, cap = 2))
    }

    @Test
    fun `tapping a new option at cap is a no-op`() {
        assertEquals(listOf(0, 1), nextPollSelection(current = listOf(0, 1), tapped = 2, cap = 2))
    }

    @Test
    fun `single-select poll ignores a second option until the first is deselected`() {
        assertEquals(listOf(0), nextPollSelection(current = listOf(0), tapped = 1, cap = 1))
    }

    @Test
    fun `single-select poll allows deselecting the current option`() {
        assertEquals(emptyList(), nextPollSelection(current = listOf(0), tapped = 0, cap = 1))
    }

    @Test
    fun `multi-select poll allows adding up to the cap`() {
        assertEquals(listOf(0, 1, 2), nextPollSelection(current = listOf(0, 1), tapped = 2, cap = 3))
    }

    @Test
    fun `cap of zero means nothing is selectable`() {
        assertEquals(emptyList(), nextPollSelection(current = emptyList(), tapped = 0, cap = 0))
    }
}
