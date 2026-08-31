// fauxdemo 用脚本化的 FauxProvider 演示一次完整的编码会话：
// agent 循环 → 工具调用（write + bash）→ 会话持久化 → 分叉。
// 无需任何 API key。运行：go run ./examples/fauxdemo
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"goagent/pkg/agent"
	"goagent/pkg/harness"
	"goagent/pkg/llm"
	"goagent/pkg/tools"
)

func toolCall(id, name string, args any) *llm.ToolCallContent {
	b, _ := json.Marshal(args)
	return &llm.ToolCallContent{ID: id, Name: name, Arguments: b}
}

func main() {
	dir, _ := os.MkdirTemp("", "goagent-demo")
	defer os.RemoveAll(dir)

	// 脚本化模型：第一轮写文件并运行，第二轮总结
	faux := llm.NewFauxProvider("faux", []llm.Message{
		llm.FauxToolCall("我来创建并运行一个 hello 程序。",
			toolCall("c1", "write", map[string]any{
				"path": dir + "/hello.sh", "content": "echo 'hello from goagent'",
			}),
			toolCall("c2", "bash", map[string]any{"command": "bash " + dir + "/hello.sh"}),
		),
		llm.FauxAssistant("完成：已创建 hello.sh 并运行成功。"),
	}, 16)

	reg := llm.NewRegistry()
	reg.SetProvider(faux)
	model, _ := reg.GetModel("faux", "faux-1")

	a := agent.New(agent.Options{
		SystemPrompt: "You are a coding agent.",
		Model:        model,
		Tools:        tools.CodingTools(),
		StreamFn:     reg.StreamSimple,
	})

	session, err := harness.CreateSession(dir, dir)
	if err != nil {
		panic(err)
	}
	defer session.Close()
	h := harness.New(a, session, reg.StreamSimple, llm.StreamOptions{})

	// 订阅事件，打印生命周期（对应 pi 的 subscribe 用法）
	a.Subscribe(func(ev agent.Event) {
		switch ev.Type {
		case agent.EventTurnStart:
			fmt.Println("── turn ──")
		case agent.EventMessageUpdate:
			if ev.StreamEvent != nil && ev.StreamEvent.Type == llm.EventTextDelta {
				fmt.Print(ev.StreamEvent.Delta)
			}
		case agent.EventToolExecutionStart:
			fmt.Printf("\n[tool→] %s %s\n", ev.ToolName, string(ev.Args))
		case agent.EventToolExecutionEnd:
			status := "ok"
			if ev.IsError {
				status = "error"
			}
			fmt.Printf("[tool✓] %s → %s\n", ev.ToolName, status)
		}
	})

	fmt.Println("== prompt ==")
	if err := h.Prompt(context.Background(), "写一个 hello 脚本并运行"); err != nil {
		panic(err)
	}
	a.WaitForIdle()

	fmt.Println("\n== session tree ==")
	for _, e := range session.Entries() {
		fmt.Printf("  %s %-10s parent=%s\n", e.ID, e.Type, e.ParentID)
	}

	// 演示分叉：回到第一条消息后走新路
	fmt.Println("\n== branch ==")
	first := session.Entries()[0].ID
	if err := session.BranchWithSummary(first, "原路径创建了 hello.sh"); err != nil {
		panic(err)
	}
	if err := h.Resume(); err != nil {
		panic(err)
	}
	fmt.Printf("branched; context now has %d message(s)\n", len(a.Messages()))
	fmt.Println("session file:", session.Path())
}
