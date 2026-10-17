package tui

import (
	"fmt"
	"strings"

	"goagent/pkg/llm"
)

type usageStats struct {
	last      llm.Usage
	total     llm.Usage
	responses int
}

func (s *usageStats) add(usage llm.Usage) {
	if usage.IsZero() {
		return
	}
	s.last = usage
	s.total.Add(usage)
	s.responses++
}

func (s usageStats) status() string {
	if s.responses == 0 {
		return ""
	}
	return fmt.Sprintf("tokens in %s out %s · total %s",
		formatTokenCount(s.last.Input), formatTokenCount(s.last.Output), formatTokenCount(s.total.Total()))
}

func (s usageStats) detail() string {
	if s.responses == 0 {
		return "暂无 token 用量数据（provider 可能未返回 usage）"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "最近响应：input %d · output %d · total %d", s.last.Input, s.last.Output, s.last.Total())
	writeUsageExtras(&b, s.last)
	fmt.Fprintf(&b, "\n累计（%d 次 assistant 响应）：input %d · output %d · total %d",
		s.responses, s.total.Input, s.total.Output, s.total.Total())
	writeUsageExtras(&b, s.total)
	return b.String()
}

func writeUsageExtras(b *strings.Builder, usage llm.Usage) {
	if usage.CacheRead > 0 {
		fmt.Fprintf(b, " · cacheRead %d", usage.CacheRead)
	}
	if usage.CacheWrite > 0 {
		fmt.Fprintf(b, " · cacheWrite %d", usage.CacheWrite)
	}
	if usage.Cost != 0 {
		fmt.Fprintf(b, " · cost %.6f", usage.Cost)
	}
}

func formatTokenCount(value int) string {
	switch {
	case value >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(value)/1_000_000)
	case value >= 1_000:
		return fmt.Sprintf("%.1fk", float64(value)/1_000)
	default:
		return fmt.Sprintf("%d", value)
	}
}
