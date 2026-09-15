package service

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const (
	openAISilentRefusalMinRequestBodyBytes = 64 * 1024
	openAISilentRefusalErrorCode           = "openai_silent_refusal"
	openAISilentRefusalUpstreamMessage     = "OpenAI upstream returned an empty completion stream with finish_reason=stop and no usage"
	openAISilentRefusalClientMessage       = "Upstream returned an empty completion without usage; no fallback account was available"
	openAIResponsesEmptyCompletedMessage   = "OpenAI upstream returned an empty response.completed stream with no output and no usage"
)

type openAIChatSilentRefusalDetector struct {
	enabled         bool
	sawContent      bool
	sawToolCall     bool
	sawFunctionCall bool
	sawUsage        bool
	sawError        bool
	sawReasoning    bool
	sawFinish       bool
	finishReason    string
}

func newOpenAIChatSilentRefusalDetector(requestBodyLen int) *openAIChatSilentRefusalDetector {
	return &openAIChatSilentRefusalDetector{
		enabled: requestBodyLen >= openAISilentRefusalMinRequestBodyBytes,
	}
}

func (d *openAIChatSilentRefusalDetector) Enabled() bool {
	return d != nil && d.enabled
}

// openAISilentRefusalCommitGrace 是「为静默拒绝检测保留缓冲」的最长时长。
//
// 静默拒绝的正常形态是上游很快返回 finish_reason=stop 且无内容，此时保留缓冲
// 能实现「空响应透明 failover」。但如果上游长时间不吐任何可释放内容，保留缓冲
// 就从保护变成了伤害：客户端在整个等待期收到零字节，按自身空闲超时断开，而
// 服务端仍以 200 收尾，现象是「无任何报错直接断开」，日志无法归因。
//
// 生产实测 claude-cli 请求首字节等待 109s / 146s（>=64KB 请求体触发本检测器），
// 远超客户端容忍度；而未触发本检测器的请求首字节普遍在 1–3s。
// 取 5s：正常完成的静默拒绝通常在数百毫秒到 2s 内返回，仍能保住 failover 语义；
// 超过 5s 即认定为卡顿，转为保活优先。
const openAISilentRefusalCommitGrace = 5 * time.Second

// shouldSuppressKeepaliveForSilentRefusal 报告本轮 keepalive 是否应被抑制。
//
// 仅在「检测器启用、客户端尚未收到任何字节、且仍在 commit grace 之内」时抑制。
// grace 到期后必须放行 keepalive，否则客户端会在上游长停顿期间静默超时断开。
func shouldSuppressKeepaliveForSilentRefusal(
	d *openAIChatSilentRefusalDetector,
	clientOutputStarted bool,
	elapsed time.Duration,
) bool {
	if !d.Enabled() || clientOutputStarted {
		return false
	}
	return elapsed < openAISilentRefusalCommitGrace
}

func (d *openAIChatSilentRefusalDetector) ObserveSSELine(line string) {
	if d == nil || !d.enabled {
		return
	}
	if eventType, ok := extractOpenAISSEEventLine(line); ok {
		d.observeEventType(eventType)
		return
	}
	if payload, ok := extractOpenAISSEDataLine(line); ok {
		d.ObservePayload([]byte(payload))
	}
}

func (d *openAIChatSilentRefusalDetector) ObservePayload(payload []byte) {
	if d == nil || !d.enabled {
		return
	}
	payload = bytes.TrimSpace(payload)
	if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
		return
	}
	if !gjson.ValidBytes(payload) {
		return
	}

	eventType := strings.TrimSpace(gjson.GetBytes(payload, "type").String())
	d.observeEventType(eventType)

	if gjson.GetBytes(payload, "error").Exists() {
		d.sawError = true
	}
	if usage := gjson.GetBytes(payload, "usage"); usage.Exists() && usage.IsObject() {
		d.sawUsage = true
	}
	if usage := gjson.GetBytes(payload, "response.usage"); usage.Exists() && usage.IsObject() {
		d.sawUsage = true
	}

	d.observeChatChoicesPayload(payload)
	d.observeResponsesPayload(payload, eventType)
}

func (d *openAIChatSilentRefusalDetector) ObserveChatChunk(chunk apicompat.ChatCompletionsChunk) {
	if d == nil || !d.enabled {
		return
	}
	if chunk.Usage != nil {
		d.sawUsage = true
	}
	for _, choice := range chunk.Choices {
		if choice.FinishReason != nil {
			d.observeFinishReason(*choice.FinishReason)
		}
		delta := choice.Delta
		if delta.Content != nil && *delta.Content != "" {
			d.sawContent = true
		}
		if delta.ReasoningContent != nil {
			d.sawReasoning = true
		}
		if len(delta.ToolCalls) > 0 {
			d.sawToolCall = true
		}
	}
}

func (d *openAIChatSilentRefusalDetector) ShouldReleaseClientOutput() bool {
	if d == nil || !d.enabled {
		return true
	}
	if d.sawContent || d.sawToolCall || d.sawFunctionCall || d.sawUsage || d.sawError || d.sawReasoning {
		return true
	}
	return d.sawFinish && d.finishReason != "" && d.finishReason != "stop"
}

func (d *openAIChatSilentRefusalDetector) IsSilentRefusal() bool {
	if d == nil || !d.enabled {
		return false
	}
	return !d.sawContent &&
		!d.sawToolCall &&
		!d.sawFunctionCall &&
		!d.sawUsage &&
		!d.sawError &&
		!d.sawReasoning &&
		d.sawFinish &&
		d.finishReason == "stop"
}

func (d *openAIChatSilentRefusalDetector) observeEventType(eventType string) {
	eventType = strings.TrimSpace(eventType)
	if eventType == "" {
		return
	}
	if eventType == "error" || eventType == "response.failed" {
		d.sawError = true
	}
	if strings.Contains(eventType, "reasoning") || strings.Contains(eventType, "reasoning_summary") {
		d.sawReasoning = true
	}
}

func (d *openAIChatSilentRefusalDetector) observeFinishReason(reason string) {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return
	}
	d.sawFinish = true
	d.finishReason = reason
}

func (d *openAIChatSilentRefusalDetector) observeChatChoicesPayload(payload []byte) {
	choices := gjson.GetBytes(payload, "choices")
	if !choices.Exists() || !choices.IsArray() {
		return
	}
	for _, choice := range choices.Array() {
		if finish := choice.Get("finish_reason"); finish.Exists() {
			d.observeFinishReason(finish.String())
		}
		delta := choice.Get("delta")
		if !delta.Exists() {
			continue
		}
		if content := delta.Get("content"); content.Exists() && content.String() != "" {
			d.sawContent = true
		}
		if delta.Get("tool_calls").Exists() {
			d.sawToolCall = true
		}
		if delta.Get("function_call").Exists() {
			d.sawFunctionCall = true
		}
		if delta.Get("reasoning").Exists() ||
			delta.Get("reasoning_content").Exists() ||
			delta.Get("reasoning_summary").Exists() {
			d.sawReasoning = true
		}
	}
}

func (d *openAIChatSilentRefusalDetector) observeResponsesPayload(payload []byte, eventType string) {
	switch eventType {
	case "response.output_text.delta":
		if gjson.GetBytes(payload, "delta").String() != "" {
			d.sawContent = true
		}
	case "response.output_item.added":
		switch strings.TrimSpace(gjson.GetBytes(payload, "item.type").String()) {
		case "function_call":
			d.sawToolCall = true
		case "reasoning":
			d.sawReasoning = true
		}
	case "response.function_call_arguments.delta":
		d.sawToolCall = true
	case "response.reasoning_summary_text.delta", "response.reasoning_summary_text.done":
		d.sawReasoning = true
	case "response.completed", "response.done":
		d.observeFinishReason("stop")
	case "response.incomplete":
		d.observeFinishReason("length")
	case "response.failed":
		d.sawError = true
	}

	if output := gjson.GetBytes(payload, "response.output"); output.Exists() && output.IsArray() {
		for _, item := range output.Array() {
			switch strings.TrimSpace(item.Get("type").String()) {
			case "function_call":
				d.sawToolCall = true
			case "reasoning":
				d.sawReasoning = true
			case "message":
				d.observeResponseMessageItem(item)
			}
		}
	}
}

func (d *openAIChatSilentRefusalDetector) observeResponseMessageItem(item gjson.Result) {
	content := item.Get("content")
	if !content.Exists() || !content.IsArray() {
		return
	}
	for _, part := range content.Array() {
		if part.Get("text").String() != "" {
			d.sawContent = true
			return
		}
	}
}

func newOpenAISilentRefusalFailoverError(c *gin.Context, account *Account, upstreamRequestID string) *UpstreamFailoverError {
	accountID := int64(0)
	accountName := ""
	platform := PlatformOpenAI
	if account != nil {
		accountID = account.ID
		accountName = account.Name
		platform = account.Platform
	}

	setOpsUpstreamError(c, http.StatusBadGateway, openAISilentRefusalUpstreamMessage, "")
	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
		ProxyID:            opsUpstreamProxyID(account),
		ProxyName:          opsUpstreamProxyName(account),
		Platform:           platform,
		AccountID:          accountID,
		AccountName:        accountName,
		UpstreamStatusCode: http.StatusBadGateway,
		UpstreamRequestID:  upstreamRequestID,
		Kind:               "failover",
		Message:            openAISilentRefusalUpstreamMessage,
	})

	headers := http.Header{}
	if strings.TrimSpace(upstreamRequestID) != "" {
		headers.Set("x-request-id", strings.TrimSpace(upstreamRequestID))
	}
	return &UpstreamFailoverError{
		StatusCode:      http.StatusBadGateway,
		ResponseBody:    openAISilentRefusalErrorBody(),
		ResponseHeaders: headers,
	}
}

// newOpenAIResponsesEmptyCompletedFailoverError marks an empty
// response.completed terminal event as a retryable upstream anomaly. OpenAI
// Responses streams that deliver only response.created + response.completed
// with no output, no usage and no error are treated as silent upstream
// refusals rather than successful empty replies (issue #5009).
func newOpenAIResponsesEmptyCompletedFailoverError(c *gin.Context, account *Account, upstreamRequestID string) *UpstreamFailoverError {
	accountID := int64(0)
	accountName := ""
	platform := PlatformOpenAI
	if account != nil {
		accountID = account.ID
		accountName = account.Name
		platform = account.Platform
	}

	setOpsUpstreamError(c, http.StatusBadGateway, openAIResponsesEmptyCompletedMessage, "")
	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
		ProxyID:            opsUpstreamProxyID(account),
		ProxyName:          opsUpstreamProxyName(account),
		Platform:           platform,
		AccountID:          accountID,
		AccountName:        accountName,
		UpstreamStatusCode: http.StatusBadGateway,
		UpstreamRequestID:  upstreamRequestID,
		Kind:               "failover",
		Message:            openAIResponsesEmptyCompletedMessage,
	})

	headers := http.Header{}
	if strings.TrimSpace(upstreamRequestID) != "" {
		headers.Set("x-request-id", strings.TrimSpace(upstreamRequestID))
	}
	return &UpstreamFailoverError{
		StatusCode:      http.StatusBadGateway,
		ResponseBody:    openAISilentRefusalErrorBody(),
		ResponseHeaders: headers,
	}
}

func openAISilentRefusalErrorBody() []byte {
	body, err := json.Marshal(map[string]any{
		"error": map[string]any{
			"type":    "upstream_error",
			"code":    openAISilentRefusalErrorCode,
			"message": openAISilentRefusalUpstreamMessage,
		},
	})
	if err != nil {
		return []byte(`{"error":{"type":"upstream_error","code":"openai_silent_refusal","message":"OpenAI upstream returned an empty completion stream with finish_reason=stop and no usage"}}`)
	}
	return body
}

// IsOpenAISilentRefusalErrorBody reports whether a failover body was produced
// by the OpenAI silent-refusal detector.
func IsOpenAISilentRefusalErrorBody(body []byte) bool {
	return strings.TrimSpace(gjson.GetBytes(body, "error.code").String()) == openAISilentRefusalErrorCode
}

// OpenAISilentRefusalClientMessage returns the exhausted-failover client message
// for OpenAI silent refusals.
func OpenAISilentRefusalClientMessage() string {
	return openAISilentRefusalClientMessage
}
