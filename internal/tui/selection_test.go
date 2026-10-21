package tui

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestSliceLineByCellsASCII(t *testing.T) {
	if got := sliceLineByCells("hello world", 0, 5); got != "hello" {
		t.Fatalf("got %q", got)
	}
	if got := sliceLineByCells("hello world", 6, 11); got != "world" {
		t.Fatalf("got %q", got)
	}
	if got := sliceLineByCells("hello world", 0, 1<<30); got != "hello world" {
		t.Fatalf("got %q", got)
	}
}

func TestSliceLineByCellsWideRunes(t *testing.T) {
	// 你好 = 4 cells, world starts at cell 5.
	if got := sliceLineByCells("你好 world", 0, 4); got != "你好" {
		t.Fatalf("got %q", got)
	}
	if got := sliceLineByCells("你好 world", 5, 10); got != "world" {
		t.Fatalf("got %q", got)
	}
	// A cut through the middle of a wide rune includes it.
	if got := sliceLineByCells("你好", 1, 2); got != "你" {
		t.Fatalf("got %q", got)
	}
}

func TestStripANSI(t *testing.T) {
	styled := "\x1b[1m\x1b[38;5;212mAgent\x1b[0m plain"
	if got := stripANSI(styled); got != "Agent plain" {
		t.Fatalf("got %q", got)
	}
}

func TestHighlightCells(t *testing.T) {
	plain := "hello world"
	highlighted := highlightCells(plain, 0, 5)
	if stripANSI(highlighted) != plain {
		t.Fatalf("strip changed text: %q", stripANSI(highlighted))
	}
	if !strings.Contains(highlighted, "\x1b[7m") {
		t.Fatal("missing reverse-video sequence")
	}
	if got := stripANSI(highlightCells("", 0, 5)); got != "" {
		t.Fatalf("got %q", got)
	}
}

func TestHighlightCellsPreservesStyles(t *testing.T) {
	styled := "\x1b[38;5;114m工具输出\x1b[0m tail"
	highlighted := highlightCells(styled, 0, 6)
	if stripANSI(highlighted) != "工具输出 tail" {
		t.Fatalf("strip changed text: %q", stripANSI(highlighted))
	}
	if !strings.Contains(highlighted, "\x1b[38;5;114m") {
		t.Fatal("original SGR sequence was dropped")
	}
}

func TestMouseDragSelectsAndCopies(t *testing.T) {
	var clip bytes.Buffer
	restore := osc52Output
	osc52Output = &clip
	defer func() { osc52Output = restore }()

	h := newTestHarness(t, nil, false)
	m := New(h)
	m.appendSystem("hello world", false)
	m.refreshTranscript(true)

	press := tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonLeft, X: 0, Y: 1}
	m, _ = updateModel(t, m, press)
	if !m.sel.dragging {
		t.Fatal("expected dragging state after left press")
	}

	motion := tea.MouseMsg{Action: tea.MouseActionMotion, Button: tea.MouseButtonLeft, X: 5, Y: 2}
	m, _ = updateModel(t, m, motion)

	release := tea.MouseMsg{Action: tea.MouseActionRelease, Button: tea.MouseButtonLeft, X: 5, Y: 2}
	m, cmd := updateModel(t, m, release)
	if cmd == nil {
		t.Fatal("release did not return a copy command")
	}
	if m.sel.dragging {
		t.Fatal("dragging state not cleared after release")
	}

	msg, ok := cmd().(copiedMsg)
	if !ok {
		t.Fatalf("expected copiedMsg, got %T", cmd())
	}
	want := "System\nhello"
	if msg.chars != len([]rune(want)) {
		t.Fatalf("copied %d chars, want %d (%q)", msg.chars, len([]rune(want)), want)
	}

	seq := clip.String()
	if !strings.HasPrefix(seq, "\x1b]52;c;") || !strings.HasSuffix(seq, "\a") {
		t.Fatalf("malformed OSC 52 sequence: %q", seq)
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSuffix(strings.TrimPrefix(seq, "\x1b]52;c;"), "\a"))
	if err != nil {
		t.Fatal(err)
	}
	if string(decoded) != want {
		t.Fatalf("clipboard payload %q, want %q", decoded, want)
	}

	m, _ = updateModel(t, m, msg)
	if !strings.Contains(m.notice, "已复制") {
		t.Fatalf("notice not set: %q", m.notice)
	}

	if !strings.Contains(stripANSI(m.viewportView()), "System") {
		t.Fatal("viewport view missing content")
	}
	if !strings.Contains(m.viewportView(), "\x1b[7m") {
		t.Fatal("selection not highlighted in view")
	}
}

func TestMouseClickWithoutDragClearsSelection(t *testing.T) {
	h := newTestHarness(t, nil, false)
	m := New(h)
	m.appendSystem("hello world", false)
	m.refreshTranscript(true)

	press := tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonLeft, X: 2, Y: 2}
	m, _ = updateModel(t, m, press)
	release := tea.MouseMsg{Action: tea.MouseActionRelease, Button: tea.MouseButtonLeft, X: 2, Y: 2}
	m, cmd := updateModel(t, m, release)
	if cmd != nil {
		t.Fatal("zero-width selection should not copy")
	}
	if m.sel.active {
		t.Fatal("selection should be inactive after a plain click")
	}
}

func TestMouseWheelStillScrolls(t *testing.T) {
	h := newTestHarness(t, nil, false)
	m := New(h)
	for i := 0; i < 100; i++ {
		m.appendSystem(strings.Repeat("line\n", 3), false)
	}
	m.refreshTranscript(true)

	wheel := tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonWheelUp, X: 0, Y: 5}
	before := m.viewport.YOffset
	m, _ = updateModel(t, m, wheel)
	if m.viewport.YOffset >= before {
		t.Fatal("wheel up did not scroll the viewport")
	}
}
