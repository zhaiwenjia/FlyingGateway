// 日期: 2026-11-16 20:06:00.000
// 作者: Zhai Wenjia

package server

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/zhaiwenjia/FlyingGateway/internal/adapter"
	"github.com/zhaiwenjia/FlyingGateway/internal/codec/chat"
	"github.com/zhaiwenjia/FlyingGateway/internal/codec/messages"
	"github.com/zhaiwenjia/FlyingGateway/internal/codec/responses"
	"github.com/zhaiwenjia/FlyingGateway/internal/core"
)

// TestExitFormatUniformAcrossProviders 钉住「发给用户的消息格式统一、与供应商无关」：
// 8 家真实上游响应（internal/adapter/testdata/real/*_responses_basic_real.json，2026-11-16
// 逐家真机直采）经各自适配器 ParseResponse 归一后，三个出口编码器（chat / responses / messages）
// 的输出必须满足：
//
//	① 顶层键集逐家一致（同出口、同内容形态 ⇒ 键集不受供应商影响）；
//	② reasoning item 的键形逐家一致；
//	③ usage 值与原始样本 usage 逐字对账（total_tokens / input_tokens / output_tokens），
//	   chat / responses 出口 usage 顶层键集逐家一致；
//	④ 供应商私有 wire 字段（base_resp / cost_in_usd_ticks / x_details / hit-miss 双字段 /
//	   cached_prompt_text_tokens / chunk_include_usage）⛔ 不得泄漏进任何出口。
//
// 错误实现会红：归一漏字段 ⇒ 键集不一致红；usage 记 0/丢字段 ⇒ 对账红；私有字段直出 ⇒ ④ 红。
// 变异自检（2026-11-16，均红得正确后还原）：M1 zhipu 归一 InputTokens 记 0 ⇒ ③ 红；M2 base_resp 混入 OutputItems 正文 ⇒ ④ 红。
// 手动复核：export PATH="/opt/homebrew/bin:$PATH" && go test ./internal/server/ -run TestExitFormatUniformAcrossProviders -v
func TestExitFormatUniformAcrossProviders(t *testing.T) {
	type provCase struct {
		name string
		ad   adapter.Adapter
		file string
	}
	provs := []provCase{
		{"DeepSeek", adapter.NewDeepSeek(http.DefaultClient), "deepseek_responses_basic_real.json"},
		{"Moonshot", adapter.NewMoonshot(http.DefaultClient), "moonshot_responses_basic_real.json"},
		{"Zhipu", adapter.NewZhipu(http.DefaultClient), "zhipu_responses_basic_real.json"},
		{"Qwen", adapter.NewQwen(http.DefaultClient), "qwen_responses_basic_real.json"},
		{"Xiaomi", adapter.NewXiaomi(http.DefaultClient), "mimo_responses_basic_real.json"},
		{"Ollama", adapter.NewOllamaCompat(http.DefaultClient), "ollama_responses_basic_real.json"},
		{"MiniMax", adapter.NewMiniMax(http.DefaultClient), "minimax_responses_basic_real.json"},
		{"OpenAI", adapter.NewOpenAI(http.DefaultClient), "openai_responses_basic_real.json"},
	}
	// 供应商私有 wire 字段（各家请求/响应树的专有键），⛔ 不得出现在统一出口。
	forbidden := []string{
		"base_resp", "cost_in_usd_ticks", "x_details",
		"prompt_cache_hit_tokens", "prompt_cache_miss_tokens",
		"cached_prompt_text_tokens", "chunk_include_usage",
	}

	type exitKeySet struct{ chatKeys, respKeys, msgKeys, usageChatKeys, usageRespKeys, reasonKeys map[string]bool }
	var ref *exitKeySet
	var refName string

	for _, p := range provs {
		body, err := os.ReadFile("../adapter/testdata/real/" + p.file)
		if err != nil {
			t.Fatalf("%s 读真实样本失败: %v", p.name, err)
		}
		var raw struct {
			Usage map[string]any `json:"usage"`
		}
		if err := json.Unmarshal(body, &raw); err != nil {
			t.Fatalf("%s 样本 usage 解析失败: %v", p.name, err)
		}
		resp, err := p.ad.ParseResponse(&core.Resolution{Model: "m"}, body)
		if err != nil {
			t.Fatalf("%s ParseResponse 报错: %v", p.name, err)
		}
		chatOut, err := chat.EncodeResponse(resp)
		if err != nil {
			t.Fatalf("%s chat.EncodeResponse 报错: %v", p.name, err)
		}
		respOut, err := responses.EncodeResponse(resp)
		if err != nil {
			t.Fatalf("%s responses.EncodeResponse 报错: %v", p.name, err)
		}
		msgOut, err := messages.EncodeResponse(resp)
		if err != nil {
			t.Fatalf("%s messages.EncodeResponse 报错: %v", p.name, err)
		}
		// ④ 私有字段零泄漏（三出口任一出现即红）。
		for _, out := range [][]byte{chatOut, respOut, msgOut} {
			s := string(out)
			for _, f := range forbidden {
				if strings.Contains(s, f) {
					t.Fatalf("%s 出口泄漏供应商私有字段 %q：%s", p.name, f, s)
				}
			}
		}
		var cj, rj, mj map[string]any
		for _, e := range []struct {
			b []byte
			m *map[string]any
		}{{chatOut, &cj}, {respOut, &rj}, {msgOut, &mj}} {
			if err := json.Unmarshal(e.b, e.m); err != nil {
				t.Fatalf("%s 出口 JSON 解析失败: %v", p.name, err)
			}
		}
		ks := &exitKeySet{
			chatKeys: keySet(cj), respKeys: keySet(rj), msgKeys: keySet(mj),
			usageChatKeys: keySet(nested(t, cj, "usage", p.name)),
			usageRespKeys: keySet(nested(t, rj, "usage", p.name)),
			reasonKeys:    itemKeyShape(t, rj, "reasoning", p.name),
		}
		// ③ usage 值对账：出口 usage 与原始样本 usage 逐字一致（chat 形名 prompt/completion、
		// responses 形名 input/output，映射规则供应商无关）。
		wantIn, wantOut := num(raw.Usage["input_tokens"]), num(raw.Usage["output_tokens"])
		wantTotal := num(raw.Usage["total_tokens"])
		cu := nested(t, cj, "usage", p.name)
		if num(cu["prompt_tokens"]) != wantIn || num(cu["completion_tokens"]) != wantOut || num(cu["total_tokens"]) != wantTotal {
			t.Fatalf("%s chat 出口 usage = prompt %v / completion %v / total %v，样本逐字 %v/%v/%v",
				p.name, num(cu["prompt_tokens"]), num(cu["completion_tokens"]), num(cu["total_tokens"]), wantIn, wantOut, wantTotal)
		}
		ru := nested(t, rj, "usage", p.name)
		if num(ru["input_tokens"]) != wantIn || num(ru["output_tokens"]) != wantOut || num(ru["total_tokens"]) != wantTotal {
			t.Fatalf("%s responses 出口 usage = input %v / output %v / total %v，样本逐字 %v/%v/%v",
				p.name, num(ru["input_tokens"]), num(ru["output_tokens"]), num(ru["total_tokens"]), wantIn, wantOut, wantTotal)
		}
		// ①② 跨供应商键集一致性（顶层 + reasoning item 键形 + usage 键集）。
		// ⚠️ x_gateway 是 omitempty 降级槽（内容驱动：reasoning 带 encrypted_content 时 chat 出口
		// 标记「签名丢失」）⇒ 键集比较先剥掉，其出现性单独按 ⇔ 断言（见下）。
		if ref == nil {
			ref, refName = ks, p.name
		} else {
			for _, cmp := range []struct {
				what string
				got  map[string]bool
				want map[string]bool
			}{
				{"chat 出口顶层键集", stripKeys(ks.chatKeys, "x_gateway"), stripKeys(ref.chatKeys, "x_gateway")},
				{"responses 出口顶层键集", stripKeys(ks.respKeys, "x_gateway"), stripKeys(ref.respKeys, "x_gateway")},
				{"messages 出口顶层键集", stripKeys(ks.msgKeys, "x_gateway"), stripKeys(ref.msgKeys, "x_gateway")},
				{"chat 出口 usage 键集", ks.usageChatKeys, ref.usageChatKeys},
				{"responses 出口 usage 键集", ks.usageRespKeys, ref.usageRespKeys},
				{"reasoning item 键形（剥内容驱动键）", stripKeys(ks.reasonKeys, "content", "encrypted_content", "summary", "status"),
					stripKeys(ref.reasonKeys, "content", "encrypted_content", "summary", "status")},
			} {
				if !sameKeys(cmp.got, cmp.want) {
					t.Fatalf("%s 的%s = %v，与 %s 的 %v 不一致（⛔ 出口格式必须与供应商无关）",
						p.name, cmp.what, sortedKeys(cmp.got), refName, sortedKeys(cmp.want))
				}
			}
		}
		// ② reasoning item 忠实性：基准键必在；内容驱动键（content / encrypted_content）的
		// 出现 ⇔ 源样本有值（供应商无关的映射规则，⛔ 不得随供应商增删）。
		if !ks.reasonKeys["type"] || !ks.reasonKeys["id"] {
			t.Fatalf("%s reasoning item 缺基准键 type/id：%v", p.name, sortedKeys(ks.reasonKeys))
		}
		rawRS := rawReasoningItem(t, body, p.name)
		if has, want := ks.reasonKeys["content"], rawNonEmpty(rawRS["content"]); has != want {
			t.Fatalf("%s reasoning item content 出现性 = %v，期望 ⇔ 源样本有值 %v", p.name, has, want)
		}
		if has, want := ks.reasonKeys["encrypted_content"], rawNonEmpty(rawRS["encrypted_content"]); has != want {
			t.Fatalf("%s reasoning item encrypted_content 出现性 = %v，期望 ⇔ 源样本有值 %v", p.name, has, want)
		}

		// x_gateway 出现性 ⇔ 内容驱动（样本 reasoning 含 encrypted_content ⇒ chat 出口丢签名须标记），
		// ⛔ 不得随供应商而变；出现时形状须为 x_gateway.degradations[]（field/reason/detail）。
		hasEnc := rawNonEmpty(rawReasoningItem(t, body, p.name)["encrypted_content"])
		for exitName, out := range map[string][]byte{"chat": chatOut, "responses": respOut, "messages": msgOut} {
			var m map[string]any
			_ = json.Unmarshal(out, &m)
			_, hasXG := m["x_gateway"]
			if exitName == "chat" && hasXG != hasEnc {
				t.Fatalf("%s chat 出口 x_gateway 出现性 = %v，期望 ⇔ 样本含 encrypted_content=%v（内容驱动，⛔ 不随供应商变）",
					p.name, hasXG, hasEnc)
			}
			if hasXG {
				xg, _ := m["x_gateway"].(map[string]any)
				degs, _ := xg["degradations"].([]any)
				if len(degs) == 0 {
					t.Fatalf("%s %s 出口 x_gateway 缺 degradations[]：%v", p.name, exitName, m["x_gateway"])
				}
				for _, d := range degs {
					dm, _ := d.(map[string]any)
					if dm["field"] == nil || dm["reason"] == nil {
						t.Fatalf("%s %s 出口 degradation 元素缺 field/reason：%v", p.name, exitName, d)
					}
				}
			}
		}
	}
}

// rawReasoningItem 取真实样本 output[] 里的 reasoning item（内容驱动键的判定源）。
func rawReasoningItem(t *testing.T, body []byte, who string) map[string]any {
	t.Helper()
	var raw struct {
		Output []map[string]any `json:"output"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("%s 样本 output 解析失败: %v", who, err)
	}
	for _, o := range raw.Output {
		if o["type"] == "reasoning" {
			return o
		}
	}
	t.Fatalf("%s 样本无 reasoning item", who)
	return nil
}

// rawNonEmpty 判断源字段非空（string 非空串 / 数组非空 / 其它非 nil）。
func rawNonEmpty(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case string:
		return x != ""
	case []any:
		return len(x) > 0
	default:
		return true
	}
}

// stripKeys 返回去掉指定键的键集副本（omitempty 条件键的比较口径）。
func stripKeys(m map[string]bool, keys ...string) map[string]bool {
	out := map[string]bool{}
	skip := map[string]bool{}
	for _, k := range keys {
		skip[k] = true
	}
	for k := range m {
		if !skip[k] {
			out[k] = true
		}
	}
	return out
}

func keySet(m map[string]any) map[string]bool {
	s := map[string]bool{}
	for k := range m {
		s[k] = true
	}
	return s
}

func sameKeys(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

func sortedKeys(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

func nested(t *testing.T, m map[string]any, key, who string) map[string]any {
	t.Helper()
	v, _ := m[key].(map[string]any)
	if v == nil {
		t.Fatalf("%s 出口缺 %s 对象：%v", who, key, m)
	}
	return v
}

// itemKeyShape 取 responses 出口 output[] 里指定 type item 的键形（type 一致的内容形态 ⇒ 键形一致）。
func itemKeyShape(t *testing.T, rj map[string]any, typ, who string) map[string]bool {
	t.Helper()
	items, _ := rj["output"].([]any)
	for _, it := range items {
		im, _ := it.(map[string]any)
		if im != nil && im["type"] == typ {
			return keySet(im)
		}
	}
	t.Fatalf("%s responses 出口 output[] 无 %s item：%v", who, typ, rj["output"])
	return nil
}

func num(v any) float64 {
	f, _ := v.(float64)
	return f
}
