package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"goagent/pkg/agent"
	"goagent/pkg/llm"
)

// Glob 按 glob 模式查找文件，支持 *、? 和 **。
func Glob() agent.AgentTool {
	return &globTool{baseTool{
		name:     "glob",
		readOnly: true,
		tool: llm.Tool{
			Name:        "glob",
			Description: "Find files by a glob pattern. Supports *, ? and ** for recursive matching.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"pattern":     stringProp("Glob pattern such as *.go or **/*.go"),
					"path":        stringProp("Directory to search (default current directory)"),
					"max_results": intProp("Maximum files to return (default 100)"),
				},
				"required": []any{"pattern"},
			},
		},
	}}
}

type globTool struct{ baseTool }

func (t *globTool) Execute(ctx context.Context, callID string, args json.RawMessage, onUpdate func(agent.ToolResult)) (agent.ToolResult, error) {
	var p struct {
		Pattern    string
		Path       string
		MaxResults int `json:"max_results"`
	}
	if err := decodeArgs(args, &p); err != nil {
		return agent.ToolResult{}, err
	}
	if strings.TrimSpace(p.Pattern) == "" {
		return textResult("error: pattern is required"), nil
	}
	if p.Path == "" {
		p.Path = "."
	}
	p.Path = filepath.Clean(p.Path)
	if p.MaxResults <= 0 {
		p.MaxResults = 100
	}
	re, err := compileGlobPattern(filepath.ToSlash(p.Pattern))
	if err != nil {
		return fileAccessError(ctx, err)
	}

	var matches []string
	truncated := false
	err = walkDirectory(ctx, p.Path, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() {
			if grepSkipDirs[entry.Name()] && path != p.Path {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(p.Path, path)
		if err != nil {
			return err
		}
		if !re.MatchString(filepath.ToSlash(rel)) {
			return nil
		}
		if len(matches) >= p.MaxResults {
			truncated = true
			return filepath.SkipAll
		}
		matches = append(matches, filepath.ToSlash(path))
		return nil
	})
	if err != nil {
		return fileAccessError(ctx, err)
	}
	if len(matches) == 0 {
		return textResult("no matches"), nil
	}
	sort.Strings(matches)
	var b strings.Builder
	b.WriteString(strings.Join(matches, "\n"))
	if truncated {
		fmt.Fprintf(&b, "\n(truncated at %d matches)", p.MaxResults)
	}
	return textResult(b.String()), nil
}

func compileGlobPattern(pattern string) (*regexp.Regexp, error) {
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(pattern); {
		switch {
		case strings.HasPrefix(pattern[i:], "**/"):
			b.WriteString("(?:.*/)?")
			i += 3
		case strings.HasPrefix(pattern[i:], "**"):
			b.WriteString(".*")
			i += 2
		case pattern[i] == '*':
			b.WriteString("[^/]*")
			i++
		case pattern[i] == '?':
			b.WriteString("[^/]")
			i++
		default:
			r, size := utf8.DecodeRuneInString(pattern[i:])
			b.WriteString(regexp.QuoteMeta(string(r)))
			i += size
		}
	}
	b.WriteString("$")
	return regexp.Compile(b.String())
}
