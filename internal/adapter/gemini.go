// 日期: 2026-11-14 17:02:24.851
// 作者: Zhai Wenjia

package adapter

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"

	"google.golang.org/genai"

	"github.com/zhaiwenjia/FlyingGateway/internal/core"
	gwerrors "github.com/zhaiwenjia/FlyingGateway/internal/errors"
	"github.com/zhaiwenjia/FlyingGateway/internal/stream"
	"github.com/zhaiwenjia/FlyingGateway/internal/usage"
)

// Gemini 适配器（统一 SDD 4.3.3）：
// 出站原生：POST {base}/models/{model}:generateContent 与 :streamGenerateContent?alt=sse。
// ⚠️ 路径参数正则 ^models/[^/]+$ ⇒ 模型名必须补 models/ 前缀（4.3.3）。
// AuthScheme: x-goog-api-key（8.4 三形态之一）。
// 请求体：contents / generationConfig / systemInstruction / tools（驼峰）。
// 响应体：candidates[].content.parts[] / finishReason 21 值 ⇒ 超集 / usageMetadata 驼峰。
// 流式：SSE 每帧 data: 完整 JSON、无 event: 行、无 [DONE] 哨兵；
// ⚠️ 流结束而无 finishReason ⇒ 按错误处理，⛔ 不冒充正常完成（3.7 / 4.3.3）。
// previous_response_id 支持 = false（3.5 表：未声明即按不支持处理）。
//
// SDK 化（3.6 ① 优先级：官方 google.golang.org/genai，**Tier 2** 兜底级）：
// SDK 负责请求构造（genai.GenerateContentConfig / Content / Part / Tool / ToolConfig typed）
// 与响应 / 流解析（genai.GenerateContentResponse typed），HTTP 执行保留既有 streamRequest。
// 为什么不走 Tier 1（client.Models.GenerateContentStream 经 SDK 客户端执行）——三个行为不变
// 红线在 genai SDK 内不可保：
// ① 错误体只留解析字段（genai.APIError 仅 Code/Message/Status，UpstreamBody 原文丢失）；
// ② 流事件只出 typed 响应、无 raw JSON 槽位（usage.Normalize.Raw 的逐字原文会变成重序列化）；
// ③ 流解析（iterateResponseStream）与 HTTP 执行（sendStreamRequest）耦合在 SDK 内部、不可单独引用。
// ⇒ 构造 / 解析用 SDK typed，执行走 streamRequest ⇒ URL / 鉴权头 / 错误出口 / usage 原文全保持现状。

type geminiAdapter struct {
	hc *http.Client
}

// NewGemini 构造 Gemini 适配器（hc 为出站 HTTP 客户端）。
func NewGemini(hc *http.Client) Adapter { return &geminiAdapter{hc: hc} }

func (a *geminiAdapter) Provider() core.Provider          { return core.ProviderGemini }
func (a *geminiAdapter) OutboundShape() OutboundShape     { return ShapeGeminiNative }
func (a *geminiAdapter) SupportsPreviousResponseID() bool { return false }

// AuthScheme 按 4.3.3：x-goog-api-key 头（原生鉴权亦支持 ?key=，网关走头注入）。
func (a *geminiAdapter) AuthScheme() AuthScheme {
	return AuthScheme{HeaderName: "x-goog-api-key", Prefix: ""}
}

// ParseUsage 委托 usage.Normalize（shape=APIShapeGeminiNative）：
// InputTokens = promptTokenCount + toolUsePromptTokenCount；OutputTokens = candidatesTokenCount + thoughtsTokenCount
// （thoughts 是独立加项，4.4.2 Gemini 行）。
func (a *geminiAdapter) ParseUsage(raw json.RawMessage) *core.Usage {
	return usage.Normalize(core.ProviderGemini, core.APIShapeGeminiNative, raw)
}

// ---- 请求构造（Tier 1 手段：genai SDK typed 参数为构造真值；wire 形状为 v1beta
// GenerateContentRequest：systemInstruction / contents / generationConfig / tools / toolConfig 驼峰）----

// geminiSDKParams 把规范 Request 构造为 genai SDK typed 参数（3.6「SDK 构造」）：
// GenerateContentConfig 承载 systemInstruction / 采样参数（temperature / topP /
// maxOutputTokens / stopSequences）/ tools / toolConfig，contents 为 []*genai.Content。
// 字段映射与语义同旧手写构造（4.3.3）：system ⇒ systemInstruction、assistant ⇒ model、
// tool ⇒ functionResponse、tools ⇒ functionDeclarations。
// schemas 是各 tool 的原始 JSON Schema（typed *genai.Schema 会丢未知键 ⇒ 序列化后原样回填）。
func geminiSDKParams(r *core.Request) (*genai.GenerateContentConfig, []*genai.Content, []json.RawMessage, error) {
	cfg := &genai.GenerateContentConfig{}
	if r.MaxOutputTokens > 0 {
		cfg.MaxOutputTokens = int32(r.MaxOutputTokens)
	}
	if r.Temperature != nil {
		v := float32(*r.Temperature)
		cfg.Temperature = &v
	}
	if r.TopP != nil {
		v := float32(*r.TopP)
		cfg.TopP = &v
	}
	if len(r.Stop) > 0 {
		cfg.StopSequences = r.Stop
	}
	var contents []*genai.Content
	// toolCallNames：tool_call id → 函数名。Gemini 的 FunctionResponse 只按函数名配对，
	// 而回程 role=tool 消息携带的是合成 call id ⇒ 从同请求历史的 assistant.tool_calls
	// 还原「id→名字」映射（解析顺序见 geminiContentOf）。
	toolCallNames := map[string]string{}
	for _, m := range r.Messages {
		for _, tc := range m.ToolCalls {
			if tc.ID != "" {
				toolCallNames[tc.ID] = tc.Name
			}
			if tc.CallID != "" {
				toolCallNames[tc.CallID] = tc.Name
			}
		}
	}
	for _, m := range r.Messages {
		if m.Role == "system" {
			text := contentText(m.Content)
			if cfg.SystemInstruction == nil {
				cfg.SystemInstruction = &genai.Content{Parts: []*genai.Part{{Text: text}}}
			} else {
				cfg.SystemInstruction.Parts = append(cfg.SystemInstruction.Parts, &genai.Part{Text: text})
			}
			continue
		}
		c, err := geminiContentOf(m, toolCallNames)
		if err != nil {
			return nil, nil, nil, err
		}
		contents = append(contents, c)
	}
	var schemas []json.RawMessage
	for _, t := range r.Tools {
		schema := t.Parameters
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object"}`)
		}
		// typed *genai.Schema 只承载标准 JSON Schema 键（未知键会丢）⇒ 这里做形状校验，
		// wire 上的 parameters 由 marshalGeminiRequest 原样回填（⛔ 不重排键序）。
		var typed genai.Schema
		if err := json.Unmarshal(schema, &typed); err != nil {
			return nil, nil, nil, &DetailError{Detail: gwerrors.New(core.ErrInvalidRequest, 400, "invalid_tool_schema",
				"tool parameters 不是合法 JSON Schema，无法映射为 Gemini functionDeclarations.parameters")}
		}
		cfg.Tools = append(cfg.Tools, &genai.Tool{FunctionDeclarations: []*genai.FunctionDeclaration{
			{Name: t.Name, Description: t.Description, Parameters: &typed},
		}})
		schemas = append(schemas, schema)
	}
	if r.ToolChoice != nil {
		// toolConfig.functionCallingConfig（Gemini 枚举：AUTO / NONE / ANY）。
		mode := genai.FunctionCallingConfigModeAuto
		var names []string
		switch r.ToolChoice.Mode {
		case "none":
			mode = genai.FunctionCallingConfigModeNone
		case "required":
			mode = genai.FunctionCallingConfigModeAny
		case "function":
			mode = genai.FunctionCallingConfigModeAny
			if r.ToolChoice.Name != "" {
				names = []string{r.ToolChoice.Name}
			}
		}
		fcc := &genai.FunctionCallingConfig{Mode: mode}
		if len(names) > 0 {
			fcc.AllowedFunctionNames = names
		}
		cfg.ToolConfig = &genai.ToolConfig{FunctionCallingConfig: fcc}
	}
	return cfg, contents, schemas, nil
}

// geminiContentOf 映射非 system 消息（assistant ⇒ model；tool ⇒ functionResponse）。
// toolCallNames 承载「tool_call id → 函数名」历史映射（geminiSDKParams 预扫描）。
func geminiContentOf(m core.Message, toolCallNames map[string]string) (*genai.Content, error) {
	role := "user"
	if m.Role == "assistant" {
		role = "model"
	}
	if m.Role == "tool" {
		// FunctionResponse.Name 解析顺序：显式 Name → 历史映射（合成 call id ⇒ 函数名）
		// → 旧口径兜底（ToolCallID 直接承载函数名的历史形态）。
		name := m.Name
		if name == "" {
			name = toolCallNames[m.ToolCallID]
		}
		if name == "" {
			name = m.ToolCallID
		}
		return &genai.Content{Role: "user", Parts: []*genai.Part{{FunctionResponse: &genai.FunctionResponse{
			Name: name, Response: map[string]any{"content": contentText(m.Content)},
		}}}}, nil
	}
	var parts []*genai.Part
	for _, p := range m.Content {
		switch {
		case p.Data != "" && p.MIMEType != "":
			// genai.Blob.Data 是 []byte（wire 上序列化回标准 base64）⇒ 解码校验；
			// ⚠️ 旧手写构造把 base64 串原样透传，非法 base64 由上游裁决，此处改为显式报错。
			data, err := base64.StdEncoding.DecodeString(p.Data)
			if err != nil {
				data, err = base64.RawStdEncoding.DecodeString(p.Data)
			}
			if err != nil {
				return nil, &DetailError{Detail: gwerrors.New(core.ErrInvalidRequest, 400, "invalid_content_part",
					"inline_data 不是合法 base64，无法映射为 Gemini inlineData")}
			}
			parts = append(parts, &genai.Part{InlineData: &genai.Blob{MIMEType: p.MIMEType, Data: data}})
		case p.Text != "":
			parts = append(parts, &genai.Part{Text: p.Text})
		}
	}
	for _, tc := range m.ToolCalls {
		// genai.FunctionCall.Args 是 map[string]any（wire 上重序列化 ⇒ 键序规范化）。
		args := map[string]any{}
		if tc.Arguments != "" {
			if err := json.Unmarshal([]byte(tc.Arguments), &args); err != nil {
				return nil, &DetailError{Detail: gwerrors.New(core.ErrInvalidRequest, 400, "invalid_tool_arguments",
					"tool_call arguments 不是合法 JSON，无法映射为 Gemini functionCall.args")}
			}
		}
		parts = append(parts, &genai.Part{FunctionCall: &genai.FunctionCall{Name: tc.Name, Args: args}})
	}
	return &genai.Content{Role: role, Parts: parts}, nil
}

// marshalGeminiRequest 把 SDK typed 参数序列化为 v1beta GenerateContentRequest wire 形状。
// 拆分口径与 genai SDK 的 generateContentConfigToMldev 一致：systemInstruction / tools /
// toolConfig 上提根层，采样字段入 generationConfig（⛔ 不是把整个 config 塞进 generationConfig）。
// schemas 原样回填 tools[].functionDeclarations[].parameters（typed *genai.Schema 会丢未知键）。
func marshalGeminiRequest(cfg *genai.GenerateContentConfig, contents []*genai.Content, schemas []json.RawMessage) ([]byte, error) {
	m := map[string]any{"contents": contents}
	if cfg.SystemInstruction != nil {
		m["systemInstruction"] = cfg.SystemInstruction
	}
	if len(cfg.Tools) > 0 {
		m["tools"] = cfg.Tools
	}
	if cfg.ToolConfig != nil {
		m["toolConfig"] = cfg.ToolConfig
	}
	if cfg.Temperature != nil || cfg.TopP != nil || cfg.MaxOutputTokens != 0 || len(cfg.StopSequences) > 0 {
		m["generationConfig"] = &genai.GenerateContentConfig{
			Temperature:     cfg.Temperature,
			TopP:            cfg.TopP,
			MaxOutputTokens: cfg.MaxOutputTokens,
			StopSequences:   cfg.StopSequences,
		}
	}
	raw, err := marshalBody(m)
	if err != nil {
		return nil, err
	}
	if len(schemas) == 0 {
		return raw, nil
	}
	// parameters 原样回填（json.RawMessage 值 ⇒ marshal 写回原始字节）。
	var w map[string]any
	if err := json.Unmarshal(raw, &w); err != nil {
		return nil, &DetailError{Detail: gwerrors.New(core.ErrInvalidRequest, 400, "request_marshal_failed",
			"SDK 请求体反序列化失败（parameters 回填）")}
	}
	tools, _ := w["tools"].([]any)
	for i, tv := range tools {
		if i >= len(schemas) {
			break
		}
		tool, ok := tv.(map[string]any)
		if !ok {
			continue
		}
		decls, _ := tool["functionDeclarations"].([]any)
		if len(decls) == 0 {
			continue
		}
		if decl, ok := decls[0].(map[string]any); ok {
			decl["parameters"] = schemas[i]
		}
	}
	return marshalBody(w)
}

// BuildRequest 构造 generateContent 请求（contents/generationConfig/systemInstruction 映射）。
func (a *geminiAdapter) BuildRequest(ctx context.Context, r *core.Request, res *core.Resolution) (*Outbound, error) {
	if err := checkPreviousResponseID(false, r); err != nil {
		return nil, err
	}
	cfg, contents, schemas, err := geminiSDKParams(r)
	if err != nil {
		return nil, err
	}
	raw, err := marshalGeminiRequest(cfg, contents, schemas)
	if err != nil {
		return nil, err
	}
	// ⚠️ 模型名必须带 models/ 前缀（4.3.3：路径参数正则 ^models/[^/]+$）。
	model := res.Model
	if !strings.HasPrefix(model, "models/") {
		model = "models/" + model
	}
	return &Outbound{
		Method:  "POST",
		URL:     joinURL(res.BaseURL, "/"+model+":generateContent"),
		Headers: map[string]string{"Content-Type": "application/json"},
		Body:    raw,
	}, nil
}

// ---- 响应体（4.3.3：candidates[].content.parts[] / finishReason 21 值 / usageMetadata）----

// geminiFinishMap 是 finishReason 21 值的归一化映射（4.5.1 归一化映射规则 + 超集原值）：
// STOP ⇒ stop；MAX_TOKENS ⇒ 「超上下文 / length」类；内容策略类（SAFETY / RECITATION / BLOCKLIST /
// PROHIBITED_CONTENT / SPII / MODEL_ARMOR / IMAGE_*）⇒ content_filter；MALFORMED_FUNCTION_CALL ⇒ function_call；
// 其余（OTHER / IMAGE_OTHER / UNEXPECTED_TOOL_CALL / NO_IMAGE / MALFORMED_RESPONSE / CONTINUATION /
// TOO_MANY_TOOL_CALLS / FINISH_REASON_UNSPECIFIED）⛔ 不得当作未知值丢弃 ⇒ 超集原值保留。
var geminiFinishMap = map[string]string{
	"STOP":                     core.FinishStop,
	"MAX_TOKENS":               core.FinishLength,
	"SAFETY":                   core.FinishContentFilter,
	"RECITATION":               core.FinishContentFilter,
	"BLOCKLIST":                core.FinishContentFilter,
	"PROHIBITED_CONTENT":       core.FinishContentFilter,
	"SPII":                     core.FinishContentFilter,
	"MODEL_ARMOR":              core.FinishContentFilter,
	"IMAGE_SAFETY":             core.FinishContentFilter,
	"IMAGE_PROHIBITED_CONTENT": core.FinishContentFilter,
	"IMAGE_RECITATION":         core.FinishContentFilter,
	"MALFORMED_FUNCTION_CALL":  core.FinishFunctionCall,
}

// mapGeminiFinish 映射 finishReason（未列取值 ⇒ 原值保留；SDK 的 FinishReason 是字符串类型，
// 21 值超集原样透传）。
func mapGeminiFinish(v string) string {
	if v == "" {
		return core.FinishUnknown
	}
	if m, ok := geminiFinishMap[v]; ok {
		return m
	}
	return v
}

// geminiArgsJSON 把 functionCall.args 转为 JSON 字符串（genai.FunctionCall.Args 是
// map[string]any ⇒ 重序列化；nil ⇒ 空串，与旧 RawMessage 缺字段同口径）。
// newGeminiCallID 为一次 Gemini function call 合成调用级 id（call_ + 16 hex）。
// ⚠️【修复 2026-11-17】
// 现象：Gemini FunctionCall 协议上无调用级 id，旧实现把函数名当 ToolCallID ⇒ 同轮两个
// 同名调用撞 id：chat 流式按 id 取号会把两次调用并成一个 index，role=tool 回配也会串。
// 根因：Gemini 的 functionCall / FunctionResponse 只按函数名配对，协议层不存在 call_id，
// 旧实现拿函数名补位是「单工具单调用」假设下的近似，同名并行一出现就坏。
// 为什么这么修：出站响应侧为每个调用合成唯一 id（OutputItem 的 ID 与 CallID 同值，
// Gemini 无双字段语义），回程 role=tool 消息经「历史 assistant.tool_calls 的 id→名字映射」
// 还原函数名再映射 FunctionResponse（geminiContentOf）；⛔ 不选「让下游自己按名字猜」——
// 配对是运行时相关性，猜错是静默错配。
// 不修会怎样：并行同名工具调用的结果配错对象，harness 的 tool_call_id 配对保证被静默
// 破坏，且错误表现为「模型拿到了别人的工具结果」这类最难归因的形态。
func newGeminiCallID() string {
	var b [8]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read 在现代 Go 上不会失败（失败即 panic 兜底）
	return "call_" + hex.EncodeToString(b[:])
}

func geminiArgsJSON(args map[string]any) string {
	if args == nil {
		return ""
	}
	b, err := json.Marshal(args)
	if err != nil {
		return ""
	}
	return string(b)
}

// ParseResponse 用 SDK 响应类型 genai.GenerateContentResponse 解析（4.3.3 响应体关键字段）：
// finishReason 21 值映射、MAX_TOKENS ⇒ incomplete；usageMetadata 取原始 JSON 字节进
// ParseUsage（usage.Normalize.Raw 逐字保留，⛔ 不用 typed 重序列化）。
func (a *geminiAdapter) ParseResponse(res *core.Resolution, body []byte) (*core.Response, error) {
	var r genai.GenerateContentResponse
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, &DetailError{Detail: gwerrors.New(core.ErrInvalidRequest, 502, "response_parse_failed", err.Error())}
	}
	out := &core.Response{Model: res.Model}
	var text string
	if len(r.Candidates) > 0 && r.Candidates[0] != nil {
		c := r.Candidates[0]
		if c.Content != nil {
			for _, p := range c.Content.Parts {
				if p == nil {
					continue
				}
				switch {
				case p.FunctionCall != nil:
					id := newGeminiCallID()
					out.OutputItems = append(out.OutputItems, core.OutputItem{
						Type: "function_call", ID: id, CallID: id, Name: p.FunctionCall.Name, Arguments: geminiArgsJSON(p.FunctionCall.Args),
					})
				case p.Text != "":
					text += p.Text
					out.OutputItems = append(out.OutputItems, core.OutputItem{
						Type: "message", Role: "model", Content: []core.ContentPart{{Type: "text", Text: p.Text}},
					})
				}
			}
		}
		out.FinishReason = mapGeminiFinish(string(c.FinishReason))
		switch c.FinishReason {
		case "MAX_TOKENS":
			out.Status = core.StatusIncomplete
		default:
			// 其余取值（含空值）按完成处理；空值的 FinishReason 已按 unknown 保留（mapGeminiFinish）。
			out.Status = core.StatusCompleted
		}
	} else {
		out.FinishReason = core.FinishUnknown
		out.Status = core.StatusFailed
	}
	out.Text = text
	if raw := rawJSONTopField(string(body), "usageMetadata"); len(raw) > 0 {
		out.Usage = a.ParseUsage(raw)
	}
	return out, nil
}

// ---- 流式（4.3.3：每帧 data: 完整 JSON、无 event: 行、无 [DONE]；终结帧带 finishReason）----

// geminiStreamDecoder 是 Gemini 原生帧解码器（3.7 Gemini 原生流式口径）：
// 每帧用 SDK 响应类型 genai.GenerateContentResponse 解析，usageMetadata 取帧原始 JSON 字节
// （usage.Normalize.Raw 逐字保留）。
type geminiStreamDecoder struct {
	usageFn   func(json.RawMessage) *core.Usage
	sawFinish bool
}

func (a *geminiAdapter) streamDecoder() *geminiStreamDecoder {
	return &geminiStreamDecoder{usageFn: a.ParseUsage}
}

func (d *geminiStreamDecoder) decode(ev stream.SSEEvent) ([]stream.Frame, bool, error) {
	data := strings.TrimSpace(ev.Data)
	if data == "" {
		return nil, false, nil
	}
	var r genai.GenerateContentResponse
	if err := json.Unmarshal([]byte(data), &r); err != nil {
		return nil, false, nil // 非 JSON 帧容忍跳过
	}
	var frames []stream.Frame
	if len(r.Candidates) > 0 && r.Candidates[0] != nil {
		c := r.Candidates[0]
		if c.Content != nil {
			for _, p := range c.Content.Parts {
				if p == nil {
					continue
				}
				switch {
				case p.FunctionCall != nil:
					frames = append(frames, stream.Frame{
						Kind: stream.FrameToolCallArgsDelta, ToolCallID: newGeminiCallID(),
						// Text 载函数名（帧承载约定）：functionCall part 名字与参数同包。
						// ToolCallID 用合成 id（newGeminiCallID 处有完整修复注释）——
						// ⛔ 不再沿用函数名：同轮同名多调用会撞 id。
						Text:      p.FunctionCall.Name,
						Arguments: geminiArgsJSON(p.FunctionCall.Args),
					})
				case p.Text != "":
					frames = append(frames, stream.Frame{Kind: stream.FrameTextDelta, Text: p.Text})
				}
			}
		}
		if c.FinishReason != "" {
			d.sawFinish = true
			frames = append(frames, stream.Frame{Kind: stream.FrameDone, Status: mapGeminiFinish(string(c.FinishReason))})
			if raw := rawJSONTopField(data, "usageMetadata"); len(raw) > 0 {
				frames = append([]stream.Frame{{Kind: stream.FrameUsage, Usage: d.usageFn(raw)}}, frames...)
			}
			return frames, true, nil
		}
	}
	if raw := rawJSONTopField(data, "usageMetadata"); len(raw) > 0 {
		// 部分中间帧也可能带 usageMetadata（4.3.3）。
		frames = append(frames, stream.Frame{Kind: stream.FrameUsage, Usage: d.usageFn(raw)})
	}
	return frames, false, nil
}

// endCheck：流结束而无 finishReason ⇒ 按错误处理（⛔ 不冒充正常完成，3.7 / 4.3.3）。
func (d *geminiStreamDecoder) endCheck() error {
	if d.sawFinish {
		return nil
	}
	return &DetailError{Detail: gwerrors.New(core.ErrUpstreamError, 502, "gemini_stream_no_finish_reason",
		"Gemini 流式响应结束但无 finishReason ⇒ 按流中断处理，⛔ 不冒充正常完成")}
}

// StreamStart 的流式入口：:streamGenerateContent?alt=sse（4.3.3）。
// Tier 2：HTTP 执行保留 streamRequest（URL / 鉴权注入 / 错误出口不变），
// 流解析用 SDK 响应类型（geminiStreamDecoder）。
func (a *geminiAdapter) StreamStart(ctx context.Context, r *core.Request, res *core.Resolution) (stream.FrameIterator, error) {
	rc := *r
	rc.Stream = true
	out, err := a.BuildRequest(ctx, &rc, res)
	if err != nil {
		return nil, err
	}
	// 流式端点：把 :generateContent 换成 :streamGenerateContent?alt=sse（4.3.3）。
	out.URL = strings.TrimSuffix(out.URL, ":generateContent") + ":streamGenerateContent?alt=sse"
	resp, err := streamRequest(ctx, a.hc, a.Provider(), out)
	if err != nil {
		return nil, err
	}
	dec := a.streamDecoder()
	return newSSEIter(ctx, resp.Body, dec.decode, dec.endCheck), nil
}
