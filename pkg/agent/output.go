package agent

import (
	"fmt"
	"os"
	"strings"

	"github.com/valley3405/pi-go/pkg/llm"
)

const DefaultMaxToolOutputChars = 30000

type outputConfig struct {
	dir      string
	maxChars int
}

func captureToolResult(result ToolResult, toolName, callID string, cfg outputConfig) ToolResult {
	maxChars := cfg.maxChars
	if maxChars <= 0 {
		maxChars = DefaultMaxToolOutputChars
	}
	var full strings.Builder
	total := 0
	for _, content := range result.Content {
		if text, ok := content.(*llm.TextContent); ok {
			full.WriteString(text.Text)
			total += len([]rune(text.Text))
		}
	}
	if total <= maxChars {
		return result
	}

	fullText := full.String()
	if cfg.dir != "" {
		path, err := writeFullToolOutput(cfg.dir, toolName, callID, fullText)
		if err == nil {
			head := string([]rune(fullText)[:maxChars])
			captured := fmt.Sprintf("%s\n\n... [%d chars truncated; full output saved to %s] ...", head, total-maxChars, path)
			result.Content = replaceCapturedText(result.Content, captured)
			return result
		}
		captured := truncateToolText(fullText, maxChars) + fmt.Sprintf("\n\n[full output could not be saved: %v]", err)
		result.Content = replaceCapturedText(result.Content, captured)
		return result
	}
	result.Content = replaceCapturedText(result.Content, truncateToolText(fullText, maxChars))
	return result
}

func truncateToolText(text string, maxChars int) string {
	runes := []rune(text)
	head := maxChars / 2
	tail := maxChars - head
	return fmt.Sprintf("%s\n\n... [%d chars truncated] ...\n\n%s", string(runes[:head]), len(runes)-maxChars, string(runes[len(runes)-tail:]))
}

func replaceCapturedText(content []llm.Content, captured string) []llm.Content {
	out := make([]llm.Content, 0, len(content))
	replaced := false
	for _, block := range content {
		if _, ok := block.(*llm.TextContent); ok {
			if !replaced {
				out = append(out, &llm.TextContent{Text: captured})
				replaced = true
			}
			continue
		}
		out = append(out, block)
	}
	return out
}

func writeFullToolOutput(dir, toolName, callID, full string) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	prefix := sanitizeOutputPrefix(toolName)
	if callID != "" {
		prefix += "-" + sanitizeOutputPrefix(callID)
	}
	file, err := os.CreateTemp(dir, prefix+"-*.txt")
	if err != nil {
		return "", err
	}
	path := file.Name()
	if _, err := file.WriteString(full); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return "", err
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return "", err
	}
	return path, nil
}

func sanitizeOutputPrefix(value string) string {
	var b strings.Builder
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
		if b.Len() >= 40 {
			break
		}
	}
	if b.Len() == 0 {
		return "tool"
	}
	return b.String()
}
