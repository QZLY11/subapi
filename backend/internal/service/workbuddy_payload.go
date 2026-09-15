// Package service — WorkBuddy 上游请求体改写。
//
// 对齐 workbuddy2api internal/upstream/payload.go + thinking.go：
// 强制 stream、tool_choice 归一化、developer→system、thinking 注入、reasoning 降级。
package service

import (
	"encoding/json"
	"strings"
	"sync"
)

// defaultWorkBuddyDeepSeekEffort 官方客户端默认档兜底
// （configure thinking 无来源时 warn fallback to 'high'）。
// 补入后走 normalizeWorkBuddyReasoningEffort 降级管线，模型不支持 high 时
// 自动落到 ≤high 的最高支持档。
const defaultWorkBuddyDeepSeekEffort = "high"

// PrepareWorkBuddyChatPayload 改写发往 CodeBuddy 上游的 chat 请求体：
//  1. 强制 stream:true（上游拒绝非流式）
//  2. tool_choice 归一化（上游该字段是 string，对象形式会 400 code=11101）
//  3. developer role 归一为 system（上游 role 白名单不含 developer，否则 400 code=11-128）
//  4. deepseek 系模型注入 thinking.type=enabled + 缺档补默认档
//  5. 按已缓存的模型 supportedEfforts 降级 reasoning_effort
//  6. backfill assistant reasoning_content（deepseek 多轮一致性）
//
// 解析失败时原样返回（不做破坏性改写）。
func PrepareWorkBuddyChatPayload(src []byte) []byte {
	if len(src) == 0 {
		return src
	}
	var obj map[string]any
	if err := json.Unmarshal(src, &obj); err != nil {
		return src
	}
	obj["stream"] = true
	normalizeWorkBuddyToolChoice(obj)
	normalizeWorkBuddyRoles(obj)
	normalizeWorkBuddyArrayContent(obj)
	// 顺序敏感：注入的默认档也要走降级管线（模型不支持默认档时落到 ≤ 默认档的
	// 最高支持档，保证不出站不合规档位）。
	injectWorkBuddyThinking(obj)
	normalizeWorkBuddyReasoningEffort(obj, cachedWorkBuddyModelEfforts())
	backfillWorkBuddyReasoningContent(obj)
	out, err := json.Marshal(obj)
	if err != nil {
		return src
	}
	return out
}

// normalizeWorkBuddyRoles 把 messages 里的 developer 角色归一为 system。
func normalizeWorkBuddyRoles(obj map[string]any) {
	msgs, ok := obj["messages"].([]any)
	if !ok {
		return
	}
	for _, m := range msgs {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		role, ok := msg["role"].(string)
		if !ok {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(role), "developer") {
			msg["role"] = "system"
		}
	}
}

// normalizeWorkBuddyArrayContent 把 messages[].content 为数组（Anthropic 风格
// content blocks）的请求归一为纯字符串 content。ccswitch / Claude Code 等客户端
// 走 /v1/chat/completions 时会发 OpenAI 顶层结构 + Anthropic content 数组的混合
// body（例如 user 消息 content = [{"type":"text","text":"..."}]），腾讯
// /v2/chat/completions 严格按 OpenAI 协议解析，content 数组会触发 400
// （code=11101 "unsupported content type" / 11128 "unapproved channel"）。
//
// 折叠规则（对齐 apicompat.AnthropicToChatCompletionsRequest 的 array→string）：
//   - text 块：拼接为单个字符串，以 "\n\n" 分隔
//   - tool_use 块：转为 assistant tool_calls（罕见，安当前仅防御式跳过）
//   - image / 其他未知块：跳过（不在文本通道强塞，避免二次 400）
//   - 全部块都不是 text（空）时：内容设为空字符串
func normalizeWorkBuddyArrayContent(obj map[string]any) {
	msgs, ok := obj["messages"].([]any)
	if !ok {
		return
	}
	for _, m := range msgs {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		content, ok := msg["content"]
		if !ok {
			continue
		}
		blocks, isArray := content.([]any)
		if !isArray {
			continue
		}
		var parts []string
		for _, b := range blocks {
			blk, ok := b.(map[string]any)
			if !ok {
				continue
			}
			typ, _ := blk["type"].(string)
			if typ == "text" {
				if t, ok := blk["text"].(string); ok && strings.TrimSpace(t) != "" {
					parts = append(parts, t)
				}
			}
			// 其他类型（tool_use/tool_result/image/thinking）不并入文本通道，
			// 避免把结构化对象拼进 content 造成上游二次 400。
		}
		msg["content"] = strings.Join(parts, "\n\n")
	}
}

// normalizeWorkBuddyToolChoice 按上游 Go struct（string 类型）改写 OpenAI tool_choice。
//   - "none" / {"type":"none"} → 删 tool_choice + 删 tools/functions
//   - {"type":"auto"/"required"} → 字符串 "auto"/"required"
//   - {"type":"function","name":"x"} → 字符串 "x"
//   - 其他对象/非标量 → 删 tool_choice
func normalizeWorkBuddyToolChoice(obj map[string]any) {
	suppress := func() {
		delete(obj, "tools")
		delete(obj, "functions")
	}
	tc, present := obj["tool_choice"]
	if !present {
		return
	}
	switch v := tc.(type) {
	case string:
		if strings.EqualFold(strings.TrimSpace(v), "none") {
			delete(obj, "tool_choice")
			suppress()
		}
	case map[string]any:
		typ, _ := v["type"].(string)
		typ = strings.ToLower(strings.TrimSpace(typ))
		switch typ {
		case "none":
			delete(obj, "tool_choice")
			suppress()
		case "auto", "required":
			obj["tool_choice"] = typ
		case "function":
			name := ""
			if fn, ok := v["function"].(map[string]any); ok {
				name, _ = fn["name"].(string)
			}
			if name == "" {
				name, _ = v["name"].(string)
			}
			if name = strings.TrimSpace(name); name != "" {
				obj["tool_choice"] = name
			} else {
				obj["tool_choice"] = "auto"
			}
		default:
			delete(obj, "tool_choice")
		}
	default:
		delete(obj, "tool_choice")
	}
}

// ============================================================================
// thinking / reasoning（对齐 workbuddy2api internal/upstream/thinking.go）
// ============================================================================

// isWorkBuddyDeepSeekModel 报告模型名是否以 deepseek 为前缀（不区分大小写）。
// 覆盖 deepseek-v4.1-flash / deepseek-v4-pro / deepseek-r1 等变体。
func isWorkBuddyDeepSeekModel(model string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(model)), "deepseek")
}

// injectWorkBuddyThinking 按 DeepSeek 思维链开关规则改写请求体，非 deepseek 零改动。
//
// 根因：官方客户端对 deepseek 系模型「开思考」必须显式带 thinking:{type:"enabled"}
// 且有 effort 档位，否则上游按不思考应答（思维链不返回）。
//
//   - thinking.type 已显式 enabled/disabled → 客户端显式控制，绝不覆盖；
//     disabled 时删 reasoning_effort（snake/camel 双字段）。
//   - enabled 但缺 effort → 补默认档。
//   - 无 thinking / type 空 / 已有 effort → 注入 enabled 并补默认档（不覆盖已有档）。
func injectWorkBuddyThinking(obj map[string]any) {
	model, _ := obj["model"].(string)
	if !isWorkBuddyDeepSeekModel(model) {
		return
	}
	th, ok := obj["thinking"].(map[string]any)
	typ := ""
	if ok {
		typ, _ = th["type"].(string)
		typ = strings.TrimSpace(typ)
	}
	if typ != "" {
		if strings.EqualFold(typ, "disabled") {
			delete(obj, "reasoning_effort")
			delete(obj, "reasoningEffort")
			return
		}
		ensureWorkBuddyDeepSeekEffort(obj)
		return
	}
	if !ok {
		obj["thinking"] = map[string]any{"type": "enabled"}
	} else {
		th["type"] = "enabled"
	}
	ensureWorkBuddyDeepSeekEffort(obj)
}

// ensureWorkBuddyDeepSeekEffort 缺 effort 档位时补默认档（snake 优先，camel 兜底）。
// 已有任一 effort → 不覆盖（显式档位不做改写，降级交给
// normalizeWorkBuddyReasoningEffort）。
func ensureWorkBuddyDeepSeekEffort(obj map[string]any) {
	if _, has := obj["reasoning_effort"]; has {
		return
	}
	if _, has := obj["reasoningEffort"]; has {
		return
	}
	obj["reasoning_effort"] = defaultWorkBuddyDeepSeekEffort
}

// backfillWorkBuddyReasoningContent DeepSeek 多轮一致性：历史 assistant 消息带
// reasoning 痕迹时，上游要求后续请求所有 assistant 消息都带 reasoning_content
// （string，可为空串）——即 requiresReasoningContentOnAssistantMessages。
// 仅 deepseek 模型生效；任何 assistant 均无 reasoning 痕迹时零改动。
func backfillWorkBuddyReasoningContent(obj map[string]any) {
	model, _ := obj["model"].(string)
	if !isWorkBuddyDeepSeekModel(model) {
		return
	}
	msgs, ok := obj["messages"].([]any)
	if !ok || len(msgs) == 0 {
		return
	}
	hasTrace := false
	for _, mm := range msgs {
		msg, ok := mm.(map[string]any)
		if !ok {
			continue
		}
		if r, ok := msg["reasoning"].(string); ok && r != "" {
			hasTrace = true
			break
		}
		if _, ok := msg["reasoning_content"]; ok {
			hasTrace = true
			break
		}
	}
	if !hasTrace {
		return
	}
	for _, mm := range msgs {
		msg, ok := mm.(map[string]any)
		if !ok {
			continue
		}
		if role, _ := msg["role"].(string); role != "assistant" {
			continue
		}
		if _, ok := msg["reasoning_content"]; ok {
			continue // 已有 → 不覆盖
		}
		if r, ok := msg["reasoning"].(string); ok {
			msg["reasoning_content"] = r
		} else {
			msg["reasoning_content"] = ""
		}
	}
}

// workBuddyEffortRank 档位从低到高。
var workBuddyEffortRank = map[string]int{
	"off": 0, "minimal": 1, "low": 2, "medium": 3, "high": 4, "xhigh": 5, "max": 6,
}

// normalizeWorkBuddyReasoningEffort 按模型 supportedEfforts 降级 reasoning_effort
// （snake/camel 双字段兼容）。
//   - 请求档位模型支持 → 原样透传
//   - 请求档位不支持 → 改为 ≤请求档位的最高支持档
//   - 支持档全部高于请求档 → 取最低支持档（偏离最小）
//   - 未知模型/未知档位/未携带字段/模型未缓存 → 一律透传
func normalizeWorkBuddyReasoningEffort(obj map[string]any, efforts map[string][]string) {
	if len(efforts) == 0 {
		return
	}
	model, _ := obj["model"].(string)
	if model == "" {
		return
	}
	supported, ok := efforts[model]
	if !ok || len(supported) == 0 {
		return
	}
	key := ""
	if _, present := obj["reasoning_effort"]; present {
		key = "reasoning_effort"
	} else if _, present := obj["reasoningEffort"]; present {
		key = "reasoningEffort"
	} else {
		return
	}
	reqStr, ok := obj[key].(string)
	if !ok {
		return
	}
	reqStr = strings.TrimSpace(strings.ToLower(reqStr))
	reqIdx, known := workBuddyEffortRank[reqStr]
	if !known {
		return
	}
	best, bestIdx := "", -1
	for _, s := range supported {
		idx, k := workBuddyEffortRank[strings.TrimSpace(strings.ToLower(s))]
		if k && idx <= reqIdx && idx > bestIdx {
			best, bestIdx = s, idx
		}
	}
	if best != "" {
		if !strings.EqualFold(best, reqStr) {
			obj[key] = best
		}
		return
	}
	// 支持档全部高于请求档：取最低支持档。
	lowest, lowestIdx := "", 1<<30
	for _, s := range supported {
		idx, k := workBuddyEffortRank[strings.TrimSpace(strings.ToLower(s))]
		if k && idx < lowestIdx {
			lowest, lowestIdx = s, idx
		}
	}
	if lowest != "" {
		obj[key] = lowest
	}
}

// ============================================================================
// 模型 supportedEfforts 缓存
// ============================================================================

// workBuddyModelEffortsCache 缓存上游动态模型接口返回的 supportedEfforts，
// 供请求改写做 reasoning_effort 降级。未缓存（无同步或进程刚启动）时降级
// 自动退化为透传，与 workbuddy2api 的 nil-map 语义一致。
var workBuddyModelEffortsCache sync.Map // map[string][]string（key=model id）

// cacheWorkBuddyModelEfforts 用一次模型列表同步的结果刷新缓存（覆盖式）。
// 空结果不覆盖，避免一次失败同步清空既有能力表。
func cacheWorkBuddyModelEfforts(efforts map[string][]string) {
	if len(efforts) == 0 {
		return
	}
	workBuddyModelEffortsCache.Range(func(key, _ any) bool {
		if _, ok := efforts[key.(string)]; !ok {
			workBuddyModelEffortsCache.Delete(key)
		}
		return true
	})
	for model, levels := range efforts {
		if len(levels) == 0 {
			continue
		}
		workBuddyModelEffortsCache.Store(model, levels)
	}
}

// cachedWorkBuddyModelEfforts 返回缓存快照；缓存为空时返回 nil（不降级）。
func cachedWorkBuddyModelEfforts() map[string][]string {
	out := make(map[string][]string)
	workBuddyModelEffortsCache.Range(func(key, value any) bool {
		model, ok := key.(string)
		if !ok {
			return true
		}
		if levels, ok := value.([]string); ok && len(levels) > 0 {
			out[model] = levels
		}
		return true
	})
	if len(out) == 0 {
		return nil
	}
	return out
}
