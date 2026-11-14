// 日期: 2026-11-14 14:29:38.443
// 作者: Zhai Wenjia

package adapter

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

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

// ---- 请求体（4.3.3：contents / generationConfig / systemInstruction / tools / toolConfig，驼峰）----

type geminiReq struct {
	SystemInstruction *geminiContent   `json:"systemInstruction,omitempty"`
	Contents          []geminiContent  `json:"contents"`
	GenerationConfig  *geminiGenConfig `json:"generationConfig,omitempty"`
	Tools             []geminiTool     `json:"tools,omitempty"`
	ToolConfig        any              `json:"toolConfig,omitempty"`
}

type geminiContent struct {
	Role  string       `json:"role,omitempty"`
	Parts []geminiPart `json:"parts"`
}

type geminiPart struct {
	Text       string `json:"text,omitempty"`
	InlineData *struct {
		MimeType string `json:"mimeType"`
		Data     string `json:"data"`
	} `json:"inlineData,omitempty"`
	FunctionCall *struct {
		Name string          `json:"name"`
		Args json.RawMessage `json:"args"`
	} `json:"functionCall,omitempty"`
	FunctionResponse *struct {
		Name     string `json:"name"`
		Response any    `json:"response"`
	} `json:"functionResponse,omitempty"`
}

// geminiGenConfig 是 generationConfig（4.3.3：驼峰命名，⛔ 与 OpenAI 蛇形不通用）。
type geminiGenConfig struct {
	Temperature     *float64 `json:"temperature,omitempty"`
	TopP            *float64 `json:"topP,omitempty"`
	MaxOutputTokens int      `json:"maxOutputTokens,omitempty"`
	StopSequences   []string `json:"stopSequences,omitempty"`
}

type geminiTool struct {
	FunctionDeclarations []geminiFuncDecl `json:"functionDeclarations"`
}

type geminiFuncDecl struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// BuildRequest 构造 generateContent 请求（contents/generationConfig/systemInstruction 映射）。
func (a *geminiAdapter) BuildRequest(ctx context.Context, r *core.Request, res *core.Resolution) (*Outbound, error) {
	if err := checkPreviousResponseID(false, r); err != nil {
		return nil, err
	}
	body := geminiReq{}
	if r.MaxOutputTokens > 0 || r.Temperature != nil || r.TopP != nil || len(r.Stop) > 0 {
		body.GenerationConfig = &geminiGenConfig{
			Temperature:     r.Temperature,
			TopP:            r.TopP,
			MaxOutputTokens: r.MaxOutputTokens,
			StopSequences:   r.Stop,
		}
	}
	for _, m := range r.Messages {
		if m.Role == "system" {
			text := contentText(m.Content)
			if body.SystemInstruction == nil {
				body.SystemInstruction = &geminiContent{Parts: []geminiPart{{Text: text}}}
			} else {
				body.SystemInstruction.Parts = append(body.SystemInstruction.Parts, geminiPart{Text: text})
			}
			continue
		}
		body.Contents = append(body.Contents, geminiContentOf(m))
	}
	for _, t := range r.Tools {
		body.Tools = append(body.Tools, geminiTool{FunctionDeclarations: []geminiFuncDecl{{
			Name: t.Name, Description: t.Description, Parameters: t.Parameters,
		}}})
	}
	if r.ToolChoice != nil {
		// toolConfig.functionCallingConfig（Gemini 枚举：AUTO / NONE / ANY）。
		mode := "AUTO"
		var names []string
		switch r.ToolChoice.Mode {
		case "none":
			mode = "NONE"
		case "required":
			mode = "ANY"
		case "function":
			mode = "ANY"
			if r.ToolChoice.Name != "" {
				names = []string{r.ToolChoice.Name}
			}
		}
		cfg := map[string]any{"mode": mode}
		if len(names) > 0 {
			cfg["allowedFunctionNames"] = names
		}
		body.ToolConfig = map[string]any{"functionCallingConfig": cfg}
	}
	raw, err := marshalBody(body)
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

// geminiContentOf 映射非 system 消息（assistant ⇒ model；tool ⇒ functionResponse）。
func geminiContentOf(m core.Message) geminiContent {
	role := "user"
	if m.Role == "assistant" {
		role = "model"
	}
	var parts []geminiPart
	if m.Role == "tool" {
		name := m.Name
		if name == "" {
			name = m.ToolCallID
		}
		parts = append(parts, geminiPart{FunctionResponse: &struct {
			Name     string `json:"name"`
			Response any    `json:"response"`
		}{Name: name, Response: map[string]any{"content": contentText(m.Content)}}})
		return geminiContent{Role: "user", Parts: parts}
	}
	for _, p := range m.Content {
		switch {
		case p.Data != "" && p.MIMEType != "":
			parts = append(parts, geminiPart{InlineData: &struct {
				MimeType string `json:"mimeType"`
				Data     string `json:"data"`
			}{MimeType: p.MIMEType, Data: p.Data}})
		case p.Text != "":
			parts = append(parts, geminiPart{Text: p.Text})
		}
	}
	for _, tc := range m.ToolCalls {
		args := json.RawMessage(tc.Arguments)
		if len(args) == 0 {
			args = json.RawMessage(`{}`)
		}
		parts = append(parts, geminiPart{FunctionCall: &struct {
			Name string          `json:"name"`
			Args json.RawMessage `json:"args"`
		}{Name: tc.Name, Args: args}})
	}
	return geminiContent{Role: role, Parts: parts}
}

// ---- 响应体（4.3.3：candidates[].content.parts[] / finishReason 21 值 / usageMetadata）----

type geminiResp struct {
	Candidates []struct {
		Content struct {
			Role  string `json:"role"`
			Parts []struct {
				Text         string `json:"text"`
				FunctionCall *struct {
					Name string          `json:"name"`
					Args json.RawMessage `json:"args"`
				} `json:"functionCall"`
			} `json:"parts"`
		} `json:"content"`
		FinishReason string `json:"finishReason"`
		Index        int    `json:"index"`
	} `json:"candidates"`
	PromptFeedback json.RawMessage `json:"promptFeedback"`
	UsageMetadata  json.RawMessage `json:"usageMetadata"`
}

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

// mapGeminiFinish 映射 finishReason（未列取值 ⇒ 原值保留）。
func mapGeminiFinish(v string) string {
	if v == "" {
		return core.FinishUnknown
	}
	if m, ok := geminiFinishMap[v]; ok {
		return m
	}
	return v
}

// ParseResponse 解析 GenerateContentResponse（4.3.3 响应体关键字段）。
func (a *geminiAdapter) ParseResponse(res *core.Resolution, body []byte) (*core.Response, error) {
	var r geminiResp
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, &DetailError{Detail: gwerrors.New(core.ErrInvalidRequest, 502, "response_parse_failed", err.Error())}
	}
	out := &core.Response{Model: res.Model}
	var text string
	if len(r.Candidates) > 0 {
		c := r.Candidates[0]
		for _, p := range c.Content.Parts {
			switch {
			case p.FunctionCall != nil:
				out.OutputItems = append(out.OutputItems, core.OutputItem{
					Type: "function_call", Name: p.FunctionCall.Name, Arguments: string(p.FunctionCall.Args),
				})
			case p.Text != "":
				text += p.Text
				out.OutputItems = append(out.OutputItems, core.OutputItem{
					Type: "message", Role: "model", Content: []core.ContentPart{{Type: "text", Text: p.Text}},
				})
			}
		}
		out.FinishReason = mapGeminiFinish(c.FinishReason)
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
	if len(r.UsageMetadata) > 0 {
		out.Usage = a.ParseUsage(r.UsageMetadata)
	}
	return out, nil
}

// ---- 流式（4.3.3：每帧 data: 完整 JSON、无 event: 行、无 [DONE]；终结帧带 finishReason）----

// geminiStreamDecoder 是 Gemini 原生帧解码器（3.7 Gemini 原生流式口径）。
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
	var r geminiResp
	if err := json.Unmarshal([]byte(data), &r); err != nil {
		return nil, false, nil // 非 JSON 帧容忍跳过
	}
	var frames []stream.Frame
	if len(r.Candidates) > 0 {
		c := r.Candidates[0]
		for _, p := range c.Content.Parts {
			switch {
			case p.FunctionCall != nil:
				frames = append(frames, stream.Frame{
					Kind: stream.FrameToolCallArgsDelta, ToolCallID: p.FunctionCall.Name,
					// Text 载函数名（帧承载约定）：functionCall part 名字与参数同包、
					// 无独立 call_id ⇒ ToolCallID 沿用函数名，名字同时进 Text 供消费端装配。
					Text:      p.FunctionCall.Name,
					Arguments: string(p.FunctionCall.Args),
				})
			case p.Text != "":
				frames = append(frames, stream.Frame{Kind: stream.FrameTextDelta, Text: p.Text})
			}
		}
		if c.FinishReason != "" {
			d.sawFinish = true
			frames = append(frames, stream.Frame{Kind: stream.FrameDone, Status: mapGeminiFinish(c.FinishReason)})
			if len(r.UsageMetadata) > 0 {
				frames = append([]stream.Frame{{Kind: stream.FrameUsage, Usage: d.usageFn(r.UsageMetadata)}}, frames...)
			}
			return frames, true, nil
		}
	}
	if len(r.UsageMetadata) > 0 {
		// 部分中间帧也可能带 usageMetadata（4.3.3）。
		frames = append(frames, stream.Frame{Kind: stream.FrameUsage, Usage: d.usageFn(r.UsageMetadata)})
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

// ParseResponse 的流式入口：:streamGenerateContent?alt=sse（4.3.3）。
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
