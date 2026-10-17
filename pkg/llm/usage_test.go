package llm

import (
	"strings"
	"testing"
)

func TestUsageTotalAndAdd(t *testing.T) {
	if !(Usage{}).IsZero() {
		t.Fatal("zero usage reported as non-zero")
	}
	if got := (Usage{Input: 10, Output: 5, TotalTokens: 99}).Total(); got != 99 {
		t.Fatalf("provider total was not preserved: %d", got)
	}
	if got := (Usage{Input: 10, Output: 5, CacheRead: 4}).Total(); got != 15 {
		t.Fatalf("fallback total = %d, want 15", got)
	}

	var total Usage
	total.Add(Usage{Input: 100, Output: 20, CacheRead: 40})
	total.Add(Usage{Input: 5, Output: 3, TotalTokens: 10, CacheWrite: 2, Cost: 0.25})
	if total.Input != 105 || total.Output != 23 || total.Total() != 130 {
		t.Fatalf("unexpected aggregate: %+v", total)
	}
	if total.CacheRead != 40 || total.CacheWrite != 2 || total.Cost != 0.25 {
		t.Fatalf("unexpected cache/cost aggregate: %+v", total)
	}
}

func TestConsumeOAISSEUsage(t *testing.T) {
	for _, test := range []struct {
		name      string
		usageJSON string
		want      Usage
	}{
		{
			name:      "openai cached tokens",
			usageJSON: `{"prompt_tokens":100,"completion_tokens":20,"total_tokens":120,"prompt_tokens_details":{"cached_tokens":40}}`,
			want:      Usage{Input: 100, Output: 20, CacheRead: 40, TotalTokens: 120},
		},
		{
			name:      "deepseek cache hit and fallback total",
			usageJSON: `{"prompt_tokens":60,"completion_tokens":10,"prompt_cache_hit_tokens":25}`,
			want:      Usage{Input: 60, Output: 10, CacheRead: 25, TotalTokens: 70},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			sse := "data: {\"choices\":[],\"usage\":" + test.usageJSON + "}\n\ndata: [DONE]\n\n"
			stream := NewEventStream()
			if err := consumeOAISSE(strings.NewReader(sse), Model{}, stream); err != nil {
				t.Fatal(err)
			}
			result, err := stream.Result()
			if err != nil {
				t.Fatal(err)
			}
			if result.Usage != test.want {
				t.Fatalf("usage = %+v, want %+v", result.Usage, test.want)
			}
		})
	}
}

func TestConsumeAntSSEUsage(t *testing.T) {
	sse := `data: {"type":"message_start","message":{"usage":{"input_tokens":100,"output_tokens":1,"cache_creation_input_tokens":10,"cache_read_input_tokens":40}}}

data: {"type":"content_block_start","index":0,"content_block":{"type":"text"}}

data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}

data: {"type":"content_block_stop","index":0}

data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":20}}

`
	stream := NewEventStream()
	if err := consumeAntSSE(strings.NewReader(sse), stream); err != nil {
		t.Fatal(err)
	}
	result, err := stream.Result()
	if err != nil {
		t.Fatal(err)
	}
	want := Usage{Input: 100, Output: 20, CacheRead: 40, CacheWrite: 10, TotalTokens: 170}
	if result.Usage != want {
		t.Fatalf("usage = %+v, want %+v", result.Usage, want)
	}
	if result.Text() != "hi" {
		t.Fatalf("text = %q, want hi", result.Text())
	}
}
