// 日期: 2026-11-14 11:47:34.859
// 作者: Zhai Wenjia

// Package telemetry 承载 OTel GenAI 语义约定口径的属性映射与 span / 指标抽象
// （统一 SDD 6.1–6.4）。
//
// ⭐ 6.1 铁律：gen_ai.* / gateway.* 字段名**集中在本文件单一映射层**，
// 业务代码 ⛔ 不散写字符串属性名；语义约定改名时只改这里。
// gen_ai.* 是封闭语义命名空间，自定义属性一律 gateway.* 前缀。
package telemetry

// ---- gen_ai.* 语义约定属性（pin 口径：统一 SDD 6.2 逐字）----

const (
	AttrOperationName = "gen_ai.operation.name"
	AttrProviderName  = "gen_ai.provider.name"
	AttrRequestModel  = "gen_ai.request.model"
	AttrResponseModel = "gen_ai.response.model"
	AttrResponseID    = "gen_ai.response.id"
	AttrRequestStream = "gen_ai.request.stream"
	// AttrStreamCursor 是最后一个已收流式事件的游标（断点续传统一 SDD 6.2）。
	AttrStreamCursor = "gen_ai.request.stream_cursor"

	AttrUsageInputTokens  = "gen_ai.usage.input_tokens"
	AttrUsageOutputTokens = "gen_ai.usage.output_tokens"
	// AttrUsageCacheRead / AttrUsageCacheCreation 在 gen_ai.usage.* 公共命名空间
	// **单独计量**（⛔ 不并入 input_tokens 重复计，统一 SDD 6.2）。
	AttrUsageCacheRead     = "gen_ai.usage.cache_read.input_tokens"
	AttrUsageCacheCreation = "gen_ai.usage.cache_creation.input_tokens"

	AttrResponseStatus = "gen_ai.response.status"
	AttrFinishReasons  = "gen_ai.response.finish_reasons"
	AttrOutputType     = "gen_ai.output.type"
	AttrConversationID = "gen_ai.conversation.id"

	// AttrErrorType 是标准 error.type 键；⛔ 取消场景不设（统一 SDD 6.3）。
	AttrErrorType = "error.type"
)

// gen_ai.operation.name 取值（统一 SDD 6.2；本设计的 chat 入口用 OperationChat）。
const (
	OperationChat = "chat"
)

// gen_ai.response.status 取值（统一 SDD 6.2）。
const (
	ResponseCompleted  = "completed"
	ResponseFailed     = "failed"
	ResponseCancelled  = "cancelled"
	ResponseInProgress = "in_progress"
	ResponseIncomplete = "incomplete"
	ResponseQueued     = "queued"
)

// ---- gateway.* 自定义属性（统一 SDD 2.3 域④ / 6.3 / 3.7）----

const (
	AttrGatewayProvider         = "gateway.provider"
	AttrGatewayShape            = "gateway.shape"
	AttrGatewayDetectConfidence = "gateway.detect_confidence"
	AttrGatewayFallback         = "gateway.fallback"
	// AttrGatewayDegradations 与响应体 x_gateway.degradations[] 内容一致，⛔ 不许只记一边。
	AttrGatewayDegradations     = "gateway.degradations[]"
	AttrGatewayCacheReadTokens  = "gateway.cache_read_tokens"
	AttrGatewayCacheWriteTokens = "gateway.cache_write_tokens"
	AttrGatewayUsageEstimated   = "gateway.usage_estimated"

	// 取消四件套（统一 SDD 6.3）。
	AttrGatewayCancelReason            = "gateway.cancel.reason"
	AttrGatewayCancelDetectedLatency   = "gateway.cancel.detected_latency_ms"
	AttrGatewayCancelUpstreamClosed    = "gateway.cancel.upstream_closed"
	AttrGatewayCancelTokensAfterCancel = "gateway.cancel.tokens_after_cancel"
	// 延迟取消观测（统一 SDD 3.7）。
	AttrGatewayCancelDeferredMs = "gateway.cancel.deferred_ms"
)

// CancelReason 是取消原因四类枚举（统一 SDD 6.3 / 6.6 要求 4）。
const (
	CancelClientDisconnect = "client_disconnect"
	CancelUpstreamError    = "upstream_error"
	CancelTimeout          = "timeout"
	CancelShutdown         = "shutdown"
)

// ---- 指标名（统一 SDD 6.4：取消事件必须有独立计数指标，不受 trace 采样影响）----

const (
	MetricRequests              = "gateway.requests"                // 标签：provider / model / result
	MetricCancelled             = "gateway.cancelled"               // 标签：reason
	MetricTokens                = "gateway.tokens"                  // 标签：direction（input / output）
	MetricCancelDetectedLatency = "gateway.cancel.detected_latency" // 直方图
	// MetricUsageMissingRate 是 usage 缺失率指标（统一 SDD 2.3 域④）。
	MetricUsageMissingRate = "gateway.usage_missing_rate"
)
