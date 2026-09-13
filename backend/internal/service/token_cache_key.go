package service

import "strconv"

// OpenAITokenCacheKey 生成 OpenAI OAuth 账号的缓存键
// 格式: "openai:account:{account_id}"
func OpenAITokenCacheKey(account *Account) string {
	return "openai:account:" + strconv.FormatInt(account.ID, 10)
}

// ClaudeTokenCacheKey 生成 Claude (Anthropic) OAuth 账号的缓存键
// 格式: "claude:account:{account_id}"
func ClaudeTokenCacheKey(account *Account) string {
	return "claude:account:" + strconv.FormatInt(account.ID, 10)
}

// WorkBuddyTokenCacheKey 生成 WorkBuddy OAuth 账号的缓存键
// 格式: "workbuddy:account:{account_id}"
func WorkBuddyTokenCacheKey(account *Account) string {
	return "workbuddy:account:" + strconv.FormatInt(account.ID, 10)
}
