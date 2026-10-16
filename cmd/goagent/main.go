// goagent CLI（对应 pi 的 coding-agent 精简版）：
// TUI、行式 REPL（-repl）与 print（-p）三种模式。
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"goagent/pkg/agent"
	"goagent/pkg/harness"
	"goagent/pkg/llm"
	"goagent/pkg/tools"
	"goagent/pkg/tui"
)

// version 由 Makefile 的 -ldflags "-X main.version=..." 注入。
var version = "dev"

const systemPrompt = `You are an AI coding agent. You help users with software engineering tasks using the available tools.

Guidelines:
- Use tools to inspect, create, and modify files; run commands with bash.
- Prefer edit over rewriting whole files.
- Verify your work (run tests/builds) before reporting completion.
- Be concise in your final answer.`

func main() {
	var (
		prompt      = flag.String("p", "", "print mode: run once with this prompt and exit")
		providerID  = flag.String("provider", envOr("GOAGENT_PROVIDER", ""), "provider id: openai-compatible | anthropic")
		modelID     = flag.String("model", envOr("GOAGENT_MODEL", ""), "model id, e.g. gpt-4o-mini or claude-sonnet-4-5")
		baseURL     = flag.String("base-url", envOr("OPENAI_BASE_URL", "https://api.openai.com/v1"), "base URL for openai-compatible provider")
		sessionDir  = flag.String("session-dir", defaultSessionDir(), "directory for session files")
		noSession   = flag.Bool("no-session", false, "disable session persistence")
		contextSize = flag.Int("context", 128000, "context window size for auto-compaction (0 = off)")
		repl        = flag.Bool("repl", false, "use the line-oriented REPL instead of the TUI")
		showVersion = flag.Bool("version", false, "print version and exit")
	)
	flag.Parse()

	if *showVersion {
		fmt.Println("goagent", version)
		return
	}

	registry, model, err := setupProvider(*providerID, *modelID, *baseURL, *contextSize)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(2)
	}

	a := agent.New(agent.Options{
		SystemPrompt: systemPrompt,
		Model:        model,
		Tools:        tools.CodingTools(),
		StreamFn:     registry.StreamSimple,
	})
	a.GetAPIKey = resolveAPIKey

	var session *harness.Session
	if !*noSession {
		cwd, _ := os.Getwd()
		session, err = harness.CreateSession(*sessionDir, cwd)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error: create session:", err)
			os.Exit(2)
		}
		defer session.Close()
	}
	h := harness.New(a, session, registry.StreamSimple, llm.StreamOptions{})
	if *contextSize > 0 {
		h.AutoCompactWindow = *contextSize
	}

	// 加载技能（对应 pi 的 skills 资源）：全局 ~/.goagent/skills + 项目 ./.agents/skills
	home, _ := os.UserHomeDir()
	cwd, _ := os.Getwd()
	h.ApplySkills(filepath.Join(home, ".goagent", "skills"), filepath.Join(cwd, ".agents", "skills"))

	// Ctrl+C 中止当前 run（对应 pi 的 abort）
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT)
	go func() {
		for range sigCh {
			if a.IsRunning() {
				a.Abort()
			}
		}
	}()

	if *prompt != "" {
		os.Exit(runPrint(h, *prompt))
	}
	if *repl {
		runInteractive(h)
		return
	}
	if err := tui.Run(h); err != nil {
		fmt.Fprintln(os.Stderr, "error: run tui:", err)
		os.Exit(1)
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func defaultSessionDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".goagent", "sessions")
}

func setupProvider(providerID, modelID, baseURL string, ctxSize int) (*llm.Registry, llm.Model, error) {
	reg := llm.NewRegistry()
	switch {
	case providerID == "anthropic" || (providerID == "" && os.Getenv("ANTHROPIC_API_KEY") != ""):
		if modelID == "" {
			modelID = "claude-sonnet-4-5"
		}
		reg.SetProvider(llm.NewAnthropicProvider("anthropic", []llm.Model{
			{ID: modelID, Name: modelID, ContextWindow: ctxSize, MaxTokens: 8192, Reasoning: true},
		}))
		m, err := reg.GetModel("anthropic", modelID)
		return reg, m, err
	default:
		pid := providerID
		if pid == "" {
			pid = "openai"
		}
		if modelID == "" {
			modelID = "gpt-4o-mini"
		}
		reg.SetProvider(llm.NewOpenAIProvider(pid, baseURL, []llm.Model{
			{ID: modelID, Name: modelID, ContextWindow: ctxSize, MaxTokens: 8192},
		}))
		m, err := reg.GetModel(pid, modelID)
		return reg, m, err
	}
}

func resolveAPIKey(provider string) string {
	switch provider {
	case "anthropic":
		return os.Getenv("ANTHROPIC_API_KEY")
	default:
		return os.Getenv("OPENAI_API_KEY")
	}
}

// ---- print 模式 ----

func runPrint(h *harness.Harness, prompt string) int {
	unsub := h.Agent.Subscribe(printSubscriber(true)) // -p 模式输出思考过程
	defer unsub()
	if err := h.Prompt(context.Background(), prompt); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	h.Agent.WaitForIdle()
	if s := h.Agent.Snapshot(); s.ErrorMessage != "" {
		return 1
	}
	fmt.Println()
	return 0
}

// printSubscriber 流式打印文本增量 + 工具执行摘要。
// showThinking 为 true 时额外输出模型的思考过程（thinking_delta）。
func printSubscriber(showThinking bool) func(agent.Event) {
	return func(ev agent.Event) {
		switch ev.Type {
		case agent.EventMessageUpdate:
			if ev.StreamEvent == nil {
				return
			}
			switch ev.StreamEvent.Type {
			case llm.EventTextDelta:
				fmt.Print(ev.StreamEvent.Delta)
			case llm.EventThinkingStart:
				if showThinking {
					fmt.Print("\n\x1b[2m[thinking] ")
				}
			case llm.EventThinkingDelta:
				if showThinking {
					fmt.Print(ev.StreamEvent.Delta)
				}
			case llm.EventThinkingEnd:
				if showThinking {
					fmt.Print("\x1b[0m\n")
				}
			}
		case agent.EventToolExecutionStart:
			fmt.Printf("\n[tool] %s %s\n", ev.ToolName, truncate(string(ev.Args), 120))
		case agent.EventToolExecutionEnd:
			mark := "ok"
			if ev.IsError {
				mark = "ERR"
			}
			fmt.Printf("[tool:%s] %s done\n", ev.ToolName, mark)
		}
	}
}

// ---- interactive 模式（行式 REPL）----

func runInteractive(h *harness.Harness) {
	fmt.Println("goagent interactive. Commands: /skills /branch <id> /tree /compact /resume /quit; !cmd runs bash directly.")
	if h.Session != nil {
		fmt.Println("session:", h.Session.Path())
	}

	unsub := h.Agent.Subscribe(printSubscriber(false))
	defer unsub()

	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 1<<20), 1<<20)
	for {
		fmt.Print("\nyou> ")
		if !scanner.Scan() {
			fmt.Println()
			return
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		switch {
		case line == "/quit" || line == "/exit":
			return
		case line == "/tree":
			printTree(h)
		case line == "/skills":
			printSkills(h)
		case line == "/compact":
			fmt.Println("compacting...")
			summary, err := h.Compact(context.Background())
			if err != nil {
				fmt.Println("compact error:", err)
			} else {
				fmt.Printf("compacted (%d chars summary)\n", len(summary))
			}
		case strings.HasPrefix(line, "/branch "):
			id := strings.TrimSpace(strings.TrimPrefix(line, "/branch "))
			if err := h.Branch(id); err != nil {
				fmt.Println("branch error:", err)
			} else {
				fmt.Println("branched to", id)
			}
		case line == "/resume":
			if err := h.Resume(); err != nil {
				fmt.Println("resume error:", err)
			} else {
				fmt.Println("resumed, messages:", len(h.Agent.Messages()))
			}
		case strings.HasPrefix(line, "!"):
			runUserBash(strings.TrimPrefix(line, "!"))
		default:
			if err := h.Prompt(context.Background(), line); err != nil {
				fmt.Println("error:", err)
				continue
			}
			h.Agent.WaitForIdle()
			fmt.Println()
		}
	}
}

func printTree(h *harness.Harness) {
	if h.Session == nil {
		fmt.Println("(no session)")
		return
	}
	for _, e := range h.Session.Entries() {
		mark := "  "
		if e.ID == h.Session.LeafID() {
			mark = "* "
		}
		fmt.Printf("%s%s %s (parent=%s)\n", mark, e.ID, e.Type, e.ParentID)
	}
}

func printSkills(h *harness.Harness) {
	if len(h.Skills) == 0 {
		fmt.Println("(no skills loaded)")
		return
	}
	for _, s := range h.Skills {
		fmt.Printf("%s [%s] %s\n  %s\n", s.Name, s.Source, s.Description, s.Dir)
	}
}

func runUserBash(cmd string) {
	t := tools.Bash()
	res, err := t.Execute(context.Background(), "user-bash",
		jsonArgs(map[string]any{"command": cmd}), nil)
	if err != nil {
		fmt.Println("bash error:", err)
		return
	}
	for _, c := range res.Content {
		if tc, ok := c.(*llm.TextContent); ok {
			fmt.Print(tc.Text)
		}
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
