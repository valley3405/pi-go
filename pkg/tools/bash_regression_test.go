package tools_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/valley3405/pi-go/pkg/llm"
	"github.com/valley3405/pi-go/pkg/tools"
)

// 回归：后台进程继承 stdout 管道时，Wait 不得卡住。
// 旧实现只杀 bash 本体，sleep 残留占住管道 → Wait 挂起直到 sleep 退出。
func TestBashBackgroundProcessGroupKill(t *testing.T) {
	b := tools.Bash()
	start := time.Now()
	res, err := b.Execute(context.Background(), "c1", jargs(map[string]any{
		"command": "sleep 30 & echo started", // bash 立即退出，sleep 继承管道
		"timeout": 1,
	}), nil)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if elapsed > 10*time.Second {
		t.Fatalf("bash tool hung for %v (process group not killed)", elapsed)
	}
	out := res.Content[0].(*llm.TextContent).Text
	if !strings.Contains(out, "started") {
		t.Fatalf("missing stdout: %q", out)
	}
	t.Logf("returned in %v", elapsed)
}

// 回归：bash 本身等待子进程时，超时应整组杀死。
func TestBashTimeoutKillsPipeline(t *testing.T) {
	b := tools.Bash()
	start := time.Now()
	res, err := b.Execute(context.Background(), "c1", jargs(map[string]any{
		"command": "sleep 30 | cat", // bash 被 cat 拖住，等管道另一端
		"timeout": 1,
	}), nil)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if elapsed > 10*time.Second {
		t.Fatalf("pipeline not killed in time: %v", elapsed)
	}
	out := res.Content[0].(*llm.TextContent).Text
	if !strings.Contains(out, "timed out") {
		t.Fatalf("expected timeout annotation: %q", out)
	}
	t.Logf("returned in %v", elapsed)
}

// 正常命令不受影响：退出码与输出照常返回。
func TestBashNormalStillWorks(t *testing.T) {
	b := tools.Bash()
	res, err := b.Execute(context.Background(), "c1", jargs(map[string]any{
		"command": "echo ok && sleep 0.1",
		"timeout": 10,
	}), nil)
	if err != nil {
		t.Fatal(err)
	}
	out := res.Content[0].(*llm.TextContent).Text
	if !strings.Contains(out, "ok") || strings.Contains(out, "timed out") {
		t.Fatalf("unexpected output: %q", out)
	}
	details, ok := res.Details.(tools.BashResultDetails)
	if !ok || !details.Success || details.ExitCode != 0 {
		t.Fatalf("unexpected success details: %#v", res.Details)
	}
}

func TestBashResultDetailsFailure(t *testing.T) {
	res, err := tools.Bash().Execute(context.Background(), "c1", jargs(map[string]any{
		"command": "exit 7",
	}), nil)
	if err != nil {
		t.Fatal(err)
	}
	details, ok := res.Details.(tools.BashResultDetails)
	if !ok || details.Success || details.ExitCode != 7 {
		t.Fatalf("unexpected failure details: %#v", res.Details)
	}
}
