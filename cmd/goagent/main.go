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

	"github.com/valley3405/pi-go/internal/tui"
	"github.com/valley3405/pi-go/pkg/agent"
	"github.com/valley3405/pi-go/pkg/harness"
	"github.com/valley3405/pi-go/pkg/llm"
	"github.com/valley3405/pi-go/pkg/tools"
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
		prompt          = flag.String("p", "", "print mode: run once with this prompt and exit")
		providerID      = flag.String("provider", envOr("GOAGENT_PROVIDER", ""), "provider id: openai-compatible | anthropic")
		modelID         = flag.String("model", envOr("GOAGENT_MODEL", ""), "model id, e.g. gpt-4o-mini or claude-sonnet-4-5")
		baseURL         = flag.String("base-url", envOr("OPENAI_BASE_URL", "https://api.openai.com/v1"), "base URL for openai-compatible provider")
		workspaceDir    = flag.String("workspace", ".", "workspace directory for file tools and shell working directory")
		sessionDir      = flag.String("session-dir", defaultSessionDir(), "directory for session files")
		noSession       = flag.Bool("no-session", false, "disable session persistence")
		contextSize     = flag.Int("context", 128000, "context window size for auto-compaction (0 = off)")
		permissionMode  = flag.String("permission", "default", "permission mode: default | acceptEdits | auto | plan | dontAsk | bypass")
		maxTurns        = flag.Int("max-turns", 50, "max LLM responses per prompt (0 = unlimited)")
		maxDuration     = flag.Duration("max-duration", 0, "max wall-clock duration per prompt (0 = unlimited)")
		toolOutputDir   = flag.String("tool-output-dir", defaultToolOutputDir(), "directory for full oversized tool output (empty = disable spilling)")
		maxToolOutput   = flag.Int("max-tool-output", agent.DefaultMaxToolOutputChars, "max characters returned to the model per tool result")
		yes             = flag.Bool("yes", false, "auto-approve tools (deny rules still apply)")
		repl            = flag.Bool("repl", false, "use the line-oriented REPL instead of the TUI")
		showVersion     = flag.Bool("version", false, "print version and exit")
		allowedTools    ruleList
		deniedTools     ruleList
		allowedCommands commandRuleList
		deniedCommands  commandRuleList
	)
	flag.Var(&allowedTools, "allow", "allow a tool name (repeatable or comma-separated)")
	flag.Var(&deniedTools, "deny", "deny a tool name (repeatable or comma-separated; highest priority)")
	flag.Var(&allowedCommands, "allow-command", "allow a bash command regex (repeatable; full-command match)")
	flag.Var(&deniedCommands, "deny-command", "deny a bash command regex (repeatable; partial match, highest priority)")
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
	policy, err := newPermissionPolicy(*permissionMode, *yes, allowedTools, deniedTools, allowedCommands, deniedCommands)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(2)
	}
	workspace, err := tools.NewWorkspace(*workspaceDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error: workspace:", err)
		os.Exit(2)
	}
	defer workspace.Close()
	policy.WorkingDir = workspace.Root()
	if err := os.Chdir(workspace.Root()); err != nil {
		fmt.Fprintln(os.Stderr, "error: workspace:", err)
		os.Exit(2)
	}

	a := agent.New(agent.Options{
		SystemPrompt:       systemPrompt,
		Model:              model,
		Tools:              workspace.CodingTools(),
		StreamFn:           registry.StreamSimple,
		Permission:         policy,
		MaxTurns:           *maxTurns,
		MaxDuration:        *maxDuration,
		ToolOutputDir:      *toolOutputDir,
		MaxToolOutputChars: *maxToolOutput,
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

	// 加载技能（对应 pi 的 skills 资源）：全局 ~/.goagent/skills + 项目
	// ./.agents/skills（规范）与 ./agents/skills（兼容别名），后者同名覆盖前者。
	// 注意此时工作目录已切到 workspace 根，项目技能锚定在 workspace 下。
	home, _ := os.UserHomeDir()
	cwd, _ := os.Getwd()
	h.ApplySkills(
		harness.SkillDir{Root: filepath.Join(home, ".goagent", "skills"), Source: "global"},
		harness.SkillDir{Root: filepath.Join(cwd, "agents", "skills"), Source: "project"},
		harness.SkillDir{Root: filepath.Join(cwd, ".agents", "skills"), Source: "project"},
	)
	for _, w := range h.SkillWarnings() {
		fmt.Fprintln(os.Stderr, "skill warning:", w)
	}

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
		policy.Approver = nonInteractiveApprover
		os.Exit(runPrint(h, *prompt))
	}
	if *repl {
		reader := bufio.NewReader(os.Stdin)
		policy.Approver = terminalApprover(reader)
		runInteractive(h, reader)
		return
	}
	if err := tui.RunWithPolicy(h, policy); err != nil {
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

func defaultToolOutputDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".goagent", "tool-output")
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
	code := terminationExitCode(h.Agent.Snapshot().TerminationReason)
	fmt.Println()
	return code
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
		case agent.EventAgentEnd:
			if ev.Reason != "" && ev.Reason != agent.TerminationCompleted {
				fmt.Printf("\n[agent] terminated: %s\n", ev.Reason)
			}
		}
	}
}

// ---- interactive 模式（行式 REPL）----

func runInteractive(h *harness.Harness, reader *bufio.Reader) {
	fmt.Println("goagent interactive. Commands: /skills /branch <id> /tree /compact /resume /quit; !cmd runs bash directly.")
	if h.Session != nil {
		fmt.Println("session:", h.Session.Path())
	}

	unsub := h.Agent.Subscribe(printSubscriber(false))
	defer unsub()

	for {
		fmt.Print("\nyou> ")
		line, err := reader.ReadString('\n')
		if err != nil {
			fmt.Println()
			return
		}
		line = strings.TrimSpace(line)
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
		case line == "/skills reload":
			h.ReloadSkills()
			fmt.Printf("skills reloaded: %d loaded, %d warnings\n", len(h.SkillsList()), len(h.SkillWarnings()))
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
	skills := h.SkillsList()
	if len(skills) == 0 {
		fmt.Println("(no skills loaded)")
	}
	for _, s := range skills {
		fmt.Printf("%s [%s] %s\n  %s\n", s.Name, s.Source, s.Description, s.Dir)
	}
	for _, w := range h.SkillWarnings() {
		fmt.Println("warning:", w)
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
