package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"goagent/pkg/agent"
	"goagent/pkg/llm"
)

// LS 列目录（对应 pi 的 ls 工具）。
func LS() agent.AgentTool {
	return &lsTool{baseTool{
		name: "ls",
		tool: llm.Tool{
			Name:        "ls",
			Description: "List directory contents with type markers (dir/file) and sizes.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path": stringProp("Directory path (default: current directory)"),
				},
			},
		},
	}}
}

type lsTool struct{ baseTool }

func (t *lsTool) Execute(ctx context.Context, callID string, args json.RawMessage, onUpdate func(agent.ToolResult)) (agent.ToolResult, error) {
	var p struct {
		Path string `json:"path"`
	}
	_ = decodeArgs(args, &p)
	if p.Path == "" {
		p.Path = "."
	}
	entries, err := os.ReadDir(p.Path)
	if err != nil {
		return textResult("error: " + err.Error()), nil
	}
	var b strings.Builder
	for _, e := range entries {
		info, _ := e.Info()
		if e.IsDir() {
			fmt.Fprintf(&b, "d %s/\n", e.Name())
		} else if info != nil {
			fmt.Fprintf(&b, "f %s (%d bytes)\n", e.Name(), info.Size())
		}
	}
	return textResult(b.String()), nil
}

// Grep 在目录里做正则搜索（对应 pi 的 grep 工具），尊重简化的 ignore 规则。
func Grep() agent.AgentTool {
	return &grepTool{baseTool{
		name: "grep",
		tool: llm.Tool{
			Name:        "grep",
			Description: "Search file contents with a regex pattern. Returns matching lines with file:line prefix.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"pattern":     stringProp("Regular expression to search for"),
					"path":        stringProp("Directory or file to search (default: current directory)"),
					"include":     stringProp("Glob filter for file names, e.g. *.go"),
					"max_results": intProp("Max matches (default 100)"),
				},
				"required": []any{"pattern"},
			},
		},
	}}
}

type grepTool struct{ baseTool }

// 默认忽略的目录（pi 里走 .gitignore，这里保持精简）。
var grepSkipDirs = map[string]bool{
	".git": true, "node_modules": true, "vendor": true, ".idea": true, "dist": true,
}

func (t *grepTool) Execute(ctx context.Context, callID string, args json.RawMessage, onUpdate func(agent.ToolResult)) (agent.ToolResult, error) {
	var p struct {
		Pattern    string `json:"pattern"`
		Path       string `json:"path"`
		Include    string `json:"include"`
		MaxResults int    `json:"max_results"`
	}
	if err := decodeArgs(args, &p); err != nil {
		return agent.ToolResult{}, err
	}
	if p.Path == "" {
		p.Path = "."
	}
	if p.MaxResults <= 0 {
		p.MaxResults = 100
	}
	re, err := regexp.Compile(p.Pattern)
	if err != nil {
		return textResult("error: invalid regex: " + err.Error()), nil
	}
	var includeRe *regexp.Regexp
	if p.Include != "" {
		glob := regexp.QuoteMeta(p.Include)
		glob = strings.ReplaceAll(glob, `\*`, ".*")
		glob = strings.ReplaceAll(glob, `\?`, ".")
		includeRe, err = regexp.Compile("^" + glob + "$")
		if err != nil {
			return textResult("error: invalid include glob: " + err.Error()), nil
		}
	}

	var b strings.Builder
	matches := 0
	truncated := false

	info, err := os.Stat(p.Path)
	if err != nil {
		return textResult("error: " + err.Error()), nil
	}
	if !info.IsDir() {
		matches, truncated = grepFile(p.Path, re, p.MaxResults, &b)
	} else {
		_ = filepath.WalkDir(p.Path, func(path string, d os.DirEntry, err error) error {
			if err != nil || truncated {
				if truncated {
					return filepath.SkipAll
				}
				return nil
			}
			if d.IsDir() {
				if grepSkipDirs[d.Name()] && path != p.Path {
					return filepath.SkipDir
				}
				return nil
			}
			if includeRe != nil && !includeRe.MatchString(d.Name()) {
				return nil
			}
			n, full := grepFile(path, re, p.MaxResults-matches, &b)
			matches += n
			truncated = truncated || full
			return nil
		})
	}
	if truncated {
		fmt.Fprintf(&b, "(truncated at %d matches)\n", p.MaxResults)
	}
	if matches == 0 {
		return textResult("no matches"), nil
	}
	return textResult(b.String()), nil
}

func grepFile(path string, re *regexp.Regexp, remaining int, b *strings.Builder) (int, bool) {
	if remaining <= 0 {
		return 0, true
	}
	f, err := os.Open(path)
	if err != nil {
		return 0, false
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 1<<20), 1<<20)
	line := 0
	n := 0
	for scanner.Scan() {
		line++
		text := scanner.Text()
		if re.MatchString(text) {
			if len(text) > 500 {
				text = text[:500] + "..."
			}
			fmt.Fprintf(b, "%s:%d: %s\n", path, line, text)
			n++
			if n >= remaining {
				return n, true
			}
		}
	}
	return n, false
}
