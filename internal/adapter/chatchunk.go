// 日期: 2026-11-14 19:47:52.381
// 作者: Zhai Wenjia

// chatChunk 是五家 chat chunk 帧解码器（qwen 兼容层 / moonshot / zhipu / deepseek / xiaomi）
// 共用的按需解码层（统一 SDD 3.7 帧解码热路径）。
//
// 为什么不用 openai-go typed ChatCompletionChunk 全量 Unmarshal：
// apijson/gjson 反射链对每个 SSE chunk 全量建 typed 树 + 逐层 RawJSON 留存，alloc 放大严重
//（PERF-PROFILE：decodeOne cum 14.85% alloc_space，apijson 9.77% + gjson 5.86%）；
// 而帧解码真正消费的只有 delta.content / delta.reasoning_content / delta.tool_calls /
// finish_reason / usage 原文五处 ⇒ 轻量 struct 单趟按需解码（usage 走 RawMessage 惰性二段，
// 缺键 / null 不解）。
//
// 两段式探测（帧语义零变化红线）：① 轻量严格解码（热路径，真实 wire 全走这段）；
// ② 严格失败（字段异型 / 畸形帧）⇒ 回落 openai-go typed 全量解码再转轻量结构。
// 为什么留兜底：实测旧 apijson 有宽松强制转换面（数字→字符串、字符串→整数，
// 20261114-opt-batchlog-chunkdecode/raw/probe_old_tolerance.log：content:123 ⇒ 文本帧
// "123"、index:"0" ⇒ 工具帧不断链、finish_reason:7 ⇒ 暂存 "7"），严格解码直接丢帧会
// 杀掉这类宽容上游；不修会怎样：兼容层第三方实现常见的异型字段导致整条工具链静默断流。
// 已知唯一残差：同一对象重复键（RFC 8259 行为未定义）旧口径 first-wins、轻量段 last-wins，
// 真实 wire 无重复键，判定可接受（见交付取舍）。
//
// 帧语义零变化：Frame 承载约定（Text 载函数名、ToolCallID 载调用 id、Arguments 载参数增量）、
// 延后收口 pendingDone、各家 quirks（choices[0].usage、reasoning_content 扩展名）全保留。

package adapter

import (
	"encoding/json"

	openai "github.com/openai/openai-go/v3"
)

// chatChunk 是 chat.completion.chunk 的按需解码结构（只建帧解码消费的字段）。
type chatChunk struct {
	Choices []chatChunkChoice `json:"choices"`
	// Usage 是顶层 usage 原文（含各家扩展字段逐字，⛔ 不经类型重整）：
	// 缺键 ⇒ nil、显式 null ⇒ "null"（chunkUsageRaw 统一判空，与旧口径
	// c.JSON.Usage.Raw() 的 "" / "null" 双判一致）。
	Usage json.RawMessage `json:"usage"`
}

type chatChunkChoice struct {
	Delta        chatChunkDelta `json:"delta"`
	FinishReason string         `json:"finish_reason"`
	// Usage 是 Moonshot quirk 兜底位（choices[0].usage，kimi_stream_real.sse 实测：
	// 顶层 usage 为 null 时 usage 在二级）；其余四家不读此位。
	Usage json.RawMessage `json:"usage"`
}

type chatChunkDelta struct {
	Content string `json:"content"`
	// ReasoningContent 是思考承载扩展名（Qwen / Kimi / MiMo / DeepSeek / 智谱逐字字段名）。
	ReasoningContent string              `json:"reasoning_content"`
	ToolCalls        []chatChunkToolCall `json:"tool_calls"`
}

type chatChunkToolCall struct {
	Index    int64  `json:"index"`
	ID       string `json:"id"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// chunkUsageRaw 返回 usage 原文；缺键或显式 null ⇒ nil（不发 usage 帧）。
func chunkUsageRaw(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	return raw
}

// decodeChatChunk 两段式解码单个 chunk 负载：
// ① 轻量严格解码（热路径）；② 失败回落 typed 兜底（异型字段宽松转换面，见文件头注释）。
// 两段都失败 ⇒ ok=false（decodeOne 按非 JSON 帧容忍跳过，旧口径一致）。
func decodeChatChunk(payload string) (chatChunk, bool) {
	var c chatChunk
	if err := json.Unmarshal([]byte(payload), &c); err == nil {
		return c, true
	}
	var t openai.ChatCompletionChunk
	if err := json.Unmarshal([]byte(payload), &t); err != nil {
		return chatChunk{}, false
	}
	return chatChunkFromTyped(payload, &t), true
}

// chatChunkExtProbe 是兜底段的扩展字段二段探测（typed 树不承载的 quirks 字段）：
// delta.reasoning_content（五家思考扩展名）与 choices[].usage（Moonshot quirk）。
type chatChunkExtProbe struct {
	Choices []struct {
		Delta struct {
			ReasoningContent string `json:"reasoning_content"`
		} `json:"delta"`
		Usage json.RawMessage `json:"usage"`
	} `json:"choices"`
}

// chatChunkFromTyped 把 typed 兜底结果转回轻量结构（帧语义与热路径同构）：
// reasoning_content 与旧口径同为 stdlib 部分填充（类型错该项置空、⛔ 不整帧丢弃）；
// choices[].usage 与旧 moonshot usageOfP 探测同门（整帧解码 err==nil 才认，⛔ 不放宽）。
func chatChunkFromTyped(payload string, t *openai.ChatCompletionChunk) chatChunk {
	var c chatChunk
	if raw := t.JSON.Usage.Raw(); raw != "" {
		c.Usage = json.RawMessage(raw)
	}
	for _, ch := range t.Choices {
		cc := chatChunkChoice{FinishReason: ch.FinishReason}
		cc.Delta.Content = ch.Delta.Content
		for _, tc := range ch.Delta.ToolCalls {
			lt := chatChunkToolCall{Index: tc.Index, ID: tc.ID}
			lt.Function.Name = tc.Function.Name
			lt.Function.Arguments = tc.Function.Arguments
			cc.Delta.ToolCalls = append(cc.Delta.ToolCalls, lt)
		}
		c.Choices = append(c.Choices, cc)
	}
	var ext chatChunkExtProbe
	extErr := json.Unmarshal([]byte(payload), &ext)
	for i := range c.Choices {
		if i >= len(ext.Choices) {
			break
		}
		c.Choices[i].Delta.ReasoningContent = ext.Choices[i].Delta.ReasoningContent
	}
	if extErr == nil {
		for i := range c.Choices {
			if i >= len(ext.Choices) {
				break
			}
			c.Choices[i].Usage = ext.Choices[i].Usage
		}
	}
	return c
}
