package tui

import (
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/atotto/clipboard"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/mattn/go-runewidth"
)

// selPos is a position in viewport content coordinates: line indexes the
// full (scrolled) transcript content, col is a terminal cell column.
type selPos struct {
	line int
	col  int
}

// selection tracks an in-progress or completed mouse drag selection.
type selection struct {
	anchor   selPos
	end      selPos
	dragging bool
	active   bool
}

func (s selection) normalized() (selPos, selPos) {
	a, b := s.anchor, s.end
	if b.line < a.line || (b.line == a.line && b.col < a.col) {
		a, b = b, a
	}
	return a, b
}

type copiedMsg struct {
	chars int
}

// osc52Output receives the OSC 52 clipboard sequence; a variable so tests
// can capture it.
var osc52Output io.Writer = os.Stdout

// osc52MaxRunes bounds the payload since some terminals cap OSC 52 length.
const osc52MaxRunes = 60000

// handleMouse processes mouse events for transcript text selection. It
// reports whether the event was consumed; unconsumed events (wheel scroll)
// fall through to the viewport.
func (m *Model) handleMouse(msg tea.MouseMsg) (bool, tea.Cmd) {
	switch {
	case msg.Action == tea.MouseActionPress && msg.Button == tea.MouseButtonLeft:
		pos := m.contentPos(msg.X, msg.Y)
		m.sel = selection{anchor: pos, end: pos, dragging: true, active: true}
		return true, nil
	case m.sel.dragging && msg.Action == tea.MouseActionMotion:
		m.sel.end = m.contentPos(msg.X, msg.Y)
		return true, nil
	case m.sel.dragging && msg.Action == tea.MouseActionRelease:
		m.sel.dragging = false
		text := m.selectedText()
		if text == "" {
			m.sel.active = false
			return true, nil
		}
		return true, copyTextCmd(text)
	}
	return false, nil
}

// contentPos maps screen cell coordinates to transcript content coordinates.
func (m Model) contentPos(x, y int) selPos {
	top := 0
	if m.height >= 3 {
		top = 1 // header row
	}
	row := clamp(y, top, top+max(0, m.viewport.Height-1))
	return selPos{line: m.viewport.YOffset + row - top, col: max(0, x)}
}

// selectedText extracts the plain text covered by the current selection.
func (m Model) selectedText() string {
	if !m.sel.active || len(m.plainLines) == 0 {
		return ""
	}
	a, b := m.sel.normalized()
	if a.line < 0 {
		a.line = 0
	}
	if a.line >= len(m.plainLines) {
		return ""
	}
	b.line = min(b.line, len(m.plainLines)-1)
	lines := make([]string, 0, b.line-a.line+1)
	for i := a.line; i <= b.line; i++ {
		line := strings.TrimRight(m.plainLines[i], " ")
		c0 := 0
		if i == a.line {
			c0 = a.col
		}
		c1 := 1 << 30
		if i == b.line {
			c1 = b.col
		}
		lines = append(lines, sliceLineByCells(line, c0, c1))
	}
	return strings.Join(lines, "\n")
}

// viewportView renders the viewport with the active selection highlighted.
func (m Model) viewportView() string {
	view := m.viewport.View()
	if !m.sel.active {
		return view
	}
	a, b := m.sel.normalized()
	if a == b {
		return view
	}
	lines := strings.Split(view, "\n")
	for i := range lines {
		abs := m.viewport.YOffset + i
		if abs < a.line || abs > b.line {
			continue
		}
		c0 := 0
		if abs == a.line {
			c0 = a.col
		}
		c1 := 1 << 30
		if abs == b.line {
			c1 = b.col
		}
		if c1 > c0 {
			lines[i] = highlightCells(lines[i], c0, c1)
		}
	}
	return strings.Join(lines, "\n")
}

// sliceLineByCells returns the runes of a plain line spanning the cell
// column range [c0, c1), accounting for double-width runes.
func sliceLineByCells(line string, c0, c1 int) string {
	if c0 <= 0 && c1 >= runewidth.StringWidth(line) {
		return line
	}
	var b strings.Builder
	col := 0
	for _, r := range line {
		w := runewidth.RuneWidth(r)
		if col < c1 && col+w > c0 {
			b.WriteRune(r)
		}
		col += w
	}
	return b.String()
}

// highlightCells wraps the runes in cell range [c0, c1) with reverse video,
// passing existing ANSI escape sequences through untouched.
func highlightCells(line string, c0, c1 int) string {
	var b strings.Builder
	col := 0
	i := 0
	for i < len(line) {
		if line[i] == 0x1b {
			j := scanEscape(line, i)
			b.WriteString(line[i:j])
			i = j
			continue
		}
		r, size := utf8.DecodeRuneInString(line[i:])
		w := runewidth.RuneWidth(r)
		if col < c1 && col+w > c0 {
			b.WriteString("\x1b[7m")
			b.WriteString(line[i : i+size])
			b.WriteString("\x1b[27m")
		} else {
			b.WriteString(line[i : i+size])
		}
		col += w
		i += size
	}
	return b.String()
}

// stripANSI removes ANSI escape sequences, returning plain text.
func stripANSI(s string) string {
	var b strings.Builder
	i := 0
	for i < len(s) {
		if s[i] == 0x1b {
			i = scanEscape(s, i)
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

// scanEscape returns the index just past the escape sequence starting at i
// (s[i] must be ESC). Handles CSI sequences and two-byte escapes.
func scanEscape(s string, i int) int {
	j := i + 1
	if j < len(s) && s[j] == '[' {
		j++
		for j < len(s) && !(s[j] >= 0x40 && s[j] <= 0x7e) {
			j++
		}
		if j < len(s) {
			j++
		}
		return j
	}
	if j < len(s) {
		j++
	}
	return j
}

// copyTextCmd copies text to the system clipboard, emitting an OSC 52
// sequence (works over SSH) and falling back to platform clipboard tools.
func copyTextCmd(text string) tea.Cmd {
	return func() tea.Msg {
		writeOSC52(osc52Output, text)
		_ = clipboard.WriteAll(text)
		return copiedMsg{chars: len([]rune(text))}
	}
}

func writeOSC52(w io.Writer, text string) {
	runes := []rune(text)
	if len(runes) > osc52MaxRunes {
		text = string(runes[:osc52MaxRunes])
	}
	encoded := base64.StdEncoding.EncodeToString([]byte(text))
	fmt.Fprintf(w, "\x1b]52;c;%s\a", encoded)
}
