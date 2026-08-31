package main

import "encoding/json"

// jsonArgs 把 map 编码为工具调用的 args。
func jsonArgs(m map[string]any) json.RawMessage {
	b, _ := json.Marshal(m)
	return b
}
