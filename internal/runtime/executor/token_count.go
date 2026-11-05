package executor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/jessebldr/kiro-cpa-plugin/internal/modelcapabilities"
	kiroclaude "github.com/jessebldr/kiro-cpa-plugin/internal/translator/kiro/claude"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tiktoken-go/tokenizer"
)

var (
	countTokenizerOnce sync.Once
	countTokenizer     tokenizer.Codec
	countTokenizerErr  error
)

// CountTokens provides the local compatibility estimate required by the
// Anthropic count_tokens route. Kiro has no native count endpoint, so this
// value must not be presented as usage reported by Kiro.
func (e *KiroExecutor) CountTokens(_ context.Context, _ *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	payload := opts.OriginalRequest
	if len(bytes.TrimSpace(payload)) == 0 {
		payload = req.Payload
	}
	count, err := countClaudeInputTokens(payload)
	if err != nil {
		return cliproxyexecutor.Response{}, requestValidationErr{msg: err.Error()}
	}
	body, err := json.Marshal(map[string]int64{"input_tokens": count})
	if err != nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("encode local token count: %w", err)
	}
	return cliproxyexecutor.Response{Payload: body}, nil
}

func countClaudeInputTokens(payload []byte) (int64, error) {
	countTokenizerOnce.Do(func() {
		countTokenizer, countTokenizerErr = tokenizer.Get(tokenizer.O200kBase)
	})
	if countTokenizerErr != nil {
		return 0, fmt.Errorf("initialize local tokenizer: %w", countTokenizerErr)
	}
	if len(bytes.TrimSpace(payload)) == 0 {
		return 0, nil
	}
	if !gjson.ValidBytes(payload) {
		return 0, fmt.Errorf("invalid token-count request JSON")
	}

	root := gjson.ParseBytes(payload)
	segments := make([]string, 0, 32)
	collectCountContent(root.Get("system"), &segments)
	for _, message := range root.Get("messages").Array() {
		appendCountString(&segments, message.Get("role").String())
		collectCountContent(message.Get("content"), &segments)
	}
	for _, tool := range root.Get("tools").Array() {
		appendCountString(&segments, tool.Get("name").String())
		appendCountString(&segments, tool.Get("description").String())
		appendCountJSON(&segments, tool.Get("input_schema"))
	}
	if len(segments) == 0 {
		return 0, nil
	}
	count, err := countTokenizer.Count(strings.Join(segments, "\n"))
	if err != nil {
		return 0, fmt.Errorf("count request tokens locally: %w", err)
	}
	return int64(count), nil
}

func collectCountContent(content gjson.Result, segments *[]string) {
	if !content.Exists() {
		return
	}
	if content.Type == gjson.String {
		appendCountString(segments, content.String())
		return
	}
	if content.IsArray() {
		for _, part := range content.Array() {
			collectCountContent(part, segments)
		}
		return
	}
	if !content.IsObject() {
		return
	}

	switch content.Get("type").String() {
	case "text", "input_text":
		appendCountString(segments, content.Get("text").String())
	case "thinking":
		appendCountString(segments, content.Get("thinking").String())
	case "tool_use":
		appendCountString(segments, content.Get("id").String())
		appendCountString(segments, content.Get("name").String())
		appendCountJSON(segments, content.Get("input"))
	case "tool_result":
		appendCountString(segments, content.Get("tool_use_id").String())
		collectCountContent(content.Get("content"), segments)
	case "document":
		source := content.Get("source")
		if source.Get("type").String() == "text" {
			appendCountString(segments, source.Get("data").String())
		}
	case "image", "redacted_thinking":
		return
	default:
		appendCountJSON(segments, content)
	}
}

func appendCountString(segments *[]string, value string) {
	if value = strings.TrimSpace(value); value != "" {
		*segments = append(*segments, value)
	}
}

func appendCountJSON(segments *[]string, value gjson.Result) {
	if !value.Exists() {
		return
	}
	raw := strings.TrimSpace(value.Raw)
	if raw == "" {
		return
	}
	var compact bytes.Buffer
	if json.Compact(&compact, []byte(raw)) == nil {
		raw = compact.String()
	}
	appendCountString(segments, raw)
}

// completeKiroUsage fills the token usage of one response from what Kiro
// reported. Kiro sends no token counters: measured on 2026-09-26 on every
// catalogue model, and the official kiro-cli records zero tokens for every
// turn. It sends contextUsagePercentage, the share of the model's input window
// the conversation fills once the reply is added; times the window from Kiro's
// own catalogue it is an exact token count (2.0300002% of 200000 is 4060.0).
// So the total is Kiro's, the output is counted locally and the input is the
// total minus the output. The request is never counted to make up an input.
// Counters Kiro does send take precedence over all of this.
func completeKiroUsage(detail usage.Detail, contextPercentage float64, contextWindow int64, content string, reasoning *kiroclaude.KiroReasoningContent, toolUses []kiroclaude.KiroToolUse) usage.Detail {
	output := make([]string, 0, len(toolUses)*2+2)
	appendCountString(&output, content)
	if reasoning != nil && reasoning.ReasoningText != nil {
		appendCountString(&output, reasoning.ReasoningText.Text)
	}
	for _, toolUse := range toolUses {
		appendCountString(&output, toolUse.Name)
		if raw, err := json.Marshal(toolUse.Input); err == nil {
			appendCountString(&output, string(raw))
		}
	}
	return completeKiroUsageFromText(detail, contextPercentage, contextWindow, strings.Join(output, "\n"))
}

func completeKiroUsageFromText(detail usage.Detail, contextPercentage float64, contextWindow int64, output string) usage.Detail {
	if detail.InputTokens < 0 {
		detail.InputTokens = 0
	}
	if detail.OutputTokens < 0 {
		detail.OutputTokens = 0
	}
	if detail.TotalTokens < 0 {
		detail.TotalTokens = 0
	}

	if detail.OutputTokens == 0 && detail.InputTokens > 0 && detail.TotalTokens > detail.InputTokens {
		detail.OutputTokens = detail.TotalTokens - detail.InputTokens
	}
	if detail.OutputTokens == 0 {
		detail.OutputTokens = estimateKiroTokens(output)
	}
	if detail.InputTokens == 0 {
		total := detail.TotalTokens
		if total == 0 {
			total = kiroContextTokens(contextPercentage, contextWindow)
		}
		if total > 0 {
			// The local output count uses another tokenizer, so on a short turn
			// it can exceed Kiro's own total.
			detail.InputTokens = max(total-detail.OutputTokens, 0)
		} else {
			log.Warnf("kiro: no token total from Kiro (context usage %v%% of a %d-token window); input tokens left at zero", contextPercentage, contextWindow)
		}
	}
	minimumTotal := detail.InputTokens + detail.OutputTokens
	if detail.TotalTokens < minimumTotal {
		detail.TotalTokens = minimumTotal
	}
	return detail
}

// kiroContextTokens converts contextUsagePercentage back into the token count
// Kiro derived it from, or 0 when either number is missing.
func kiroContextTokens(percentage float64, window int64) int64 {
	if percentage <= 0 || window <= 0 || math.IsNaN(percentage) || math.IsInf(percentage, 0) {
		return 0
	}
	return int64(math.Round(percentage * float64(window) / 100))
}

// kiroContextWindow is the input window Kiro's catalogue gives this
// credential's model: the denominator of contextUsagePercentage. The catalogue
// registers Kiro's "auto" as "kiro/auto" while requests carry "auto".
func kiroContextWindow(auth *cliproxyauth.Auth, modelID string) int64 {
	authID := ""
	if auth != nil {
		authID = auth.ID
	}
	capability, ok := modelcapabilities.ForAuth(authID, modelID)
	if !ok {
		capability, _ = modelcapabilities.ForAuth(authID, "kiro/"+modelID)
	}
	return capability.InputTokenLimit
}

func estimateKiroTokens(value string) int64 {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	countTokenizerOnce.Do(func() {
		countTokenizer, countTokenizerErr = tokenizer.Get(tokenizer.O200kBase)
	})
	if countTokenizerErr != nil {
		return 0
	}
	count, err := countTokenizer.Count(value)
	if err != nil {
		return 0
	}
	if count < 1 {
		return 1
	}
	return int64(count)
}

const (
	kiroPrefixCacheTTL        = 10 * time.Minute
	kiroPrefixCacheMaxEntries = 4096
)

type kiroPrefixCacheEntry struct {
	tokens    int64
	updatedAt time.Time
}

var (
	kiroPrefixCacheMu      sync.Mutex
	kiroPrefixCacheEntries = map[string]kiroPrefixCacheEntry{}
)

func resetKiroPrefixCacheForTest() {
	kiroPrefixCacheMu.Lock()
	clear(kiroPrefixCacheEntries)
	kiroPrefixCacheMu.Unlock()
}

func kiroPrefixCacheLookup(key string, now time.Time) (int64, bool) {
	kiroPrefixCacheMu.Lock()
	defer kiroPrefixCacheMu.Unlock()
	entry, ok := kiroPrefixCacheEntries[key]
	if !ok {
		return 0, false
	}
	if now.Sub(entry.updatedAt) > kiroPrefixCacheTTL {
		delete(kiroPrefixCacheEntries, key)
		return 0, false
	}
	entry.updatedAt = now
	kiroPrefixCacheEntries[key] = entry
	return entry.tokens, entry.tokens > 0
}

func kiroPrefixCacheStore(key string, tokens int64, now time.Time) {
	if tokens <= 0 || key == "" {
		return
	}
	kiroPrefixCacheMu.Lock()
	defer kiroPrefixCacheMu.Unlock()
	if len(kiroPrefixCacheEntries) >= kiroPrefixCacheMaxEntries {
		var oldestKey string
		var oldestTime time.Time
		for k, v := range kiroPrefixCacheEntries {
			if now.Sub(v.updatedAt) > kiroPrefixCacheTTL {
				delete(kiroPrefixCacheEntries, k)
				continue
			}
			if oldestKey == "" || v.updatedAt.Before(oldestTime) {
				oldestKey = k
				oldestTime = v.updatedAt
			}
		}
		if len(kiroPrefixCacheEntries) >= kiroPrefixCacheMaxEntries && oldestKey != "" {
			delete(kiroPrefixCacheEntries, oldestKey)
		}
	}
	kiroPrefixCacheEntries[key] = kiroPrefixCacheEntry{tokens: tokens, updatedAt: now}
}

// applyKiroPrefixCacheUsage splits Kiro's total input token count into cached
// prefix tokens and uncached new-turn tokens when Kiro omits explicit cache
// counters in metadataEvent (its runtime streams emit only meteringEvent and
// contextUsageEvent). For multi-turn conversations it matches the longest
// non-assistant message prefix against prior turns on the same credential and
// model, falling back to subtracting the newly appended post-assistant turn
// when resuming a conversation after a cold start.
func applyKiroPrefixCacheUsage(detail usage.Detail, authID, model string, requestBody []byte) usage.Detail {
	if detail.InputTokens <= 0 || len(bytes.TrimSpace(requestBody)) == 0 || !gjson.ValidBytes(requestBody) {
		return detail
	}
	if detail.CacheReadTokens > 0 || detail.CachedTokens > 0 || detail.CacheCreationTokens > 0 {
		return detail
	}

	root := gjson.ParseBytes(requestBody)
	messages := root.Get("messages").Array()
	if len(messages) == 0 {
		return detail
	}

	sysToolsHasher := sha256.New()
	hasSystemOrTools := false
	if sys := root.Get("system"); sys.Exists() && sys.Type != gjson.Null {
		if raw := strings.TrimSpace(sys.Raw); raw != "" && raw != `""` && raw != "[]" {
			hasSystemOrTools = true
			sysToolsHasher.Write([]byte("sys:"))
			sysToolsHasher.Write([]byte(raw))
			sysToolsHasher.Write([]byte{0})
		}
	}
	var convMessages []gjson.Result
	for _, msg := range messages {
		role := msg.Get("role").String()
		if role == "system" || role == "developer" {
			hasSystemOrTools = true
			sysToolsHasher.Write([]byte("msg-sys:"))
			sysToolsHasher.Write([]byte(strings.TrimSpace(msg.Get("content").Raw)))
			sysToolsHasher.Write([]byte{0})
			continue
		}
		convMessages = append(convMessages, msg)
	}
	if tools := root.Get("tools"); tools.IsArray() && len(tools.Array()) > 0 {
		hasSystemOrTools = true
		sysToolsHasher.Write([]byte("tools:"))
		sysToolsHasher.Write([]byte(strings.TrimSpace(tools.Raw)))
		sysToolsHasher.Write([]byte{0})
	}

	if len(convMessages) == 0 {
		return detail
	}

	scopePrefix := strings.TrimSpace(authID) + "\x00" + strings.ToLower(strings.TrimSpace(model)) + "\x00" + hex.EncodeToString(sysToolsHasher.Sum(nil))
	systemToolsKey := "sys:" + scopePrefix

	turnHasher := sha256.New()
	turnHasher.Write([]byte(scopePrefix))
	lastAssistantIdx := -1
	var assistantCheckpointKeys []string
	for i, msg := range convMessages {
		role := msg.Get("role").String()
		if role == "assistant" {
			if i < len(convMessages)-1 {
				lastAssistantIdx = i
				assistantCheckpointKeys = append(assistantCheckpointKeys, "turn:"+hex.EncodeToString(turnHasher.Sum(nil)))
			}
			continue
		}
		turnHasher.Write([]byte{0})
		turnHasher.Write([]byte(role))
		turnHasher.Write([]byte{0})
		turnHasher.Write([]byte(strings.TrimSpace(msg.Get("tool_call_id").String())))
		turnHasher.Write([]byte{0})
		turnHasher.Write([]byte(strings.TrimSpace(msg.Get("content").Raw)))
	}
	finalTurnKey := "turn:" + hex.EncodeToString(turnHasher.Sum(nil))

	newTurnMessages := convMessages
	if lastAssistantIdx >= 0 {
		newTurnMessages = convMessages[lastAssistantIdx+1:]
	}
	newTurnTokens := estimateMessagesTokens(newTurnMessages)

	now := time.Now()
	var cachedTokens int64
	if lastAssistantIdx >= 0 {
		for i := len(assistantCheckpointKeys) - 1; i >= 0; i-- {
			if tokens, ok := kiroPrefixCacheLookup(assistantCheckpointKeys[i], now); ok {
				cachedTokens = tokens
				break
			}
		}
		if cachedTokens == 0 && detail.InputTokens > newTurnTokens {
			cachedTokens = detail.InputTokens - newTurnTokens
		}
	} else if hasSystemOrTools {
		if tokens, ok := kiroPrefixCacheLookup(systemToolsKey, now); ok {
			cachedTokens = tokens
		}
	}

	if cachedTokens > 0 {
		maxCached := detail.InputTokens
		if newTurnTokens > 0 && detail.InputTokens > newTurnTokens && cachedTokens > detail.InputTokens-newTurnTokens {
			maxCached = detail.InputTokens - newTurnTokens
		}
		if cachedTokens > maxCached {
			cachedTokens = maxCached
		}
		if cachedTokens > 0 {
			detail.CachedTokens = cachedTokens
			detail.CacheReadTokens = cachedTokens
		}
	}

	kiroPrefixCacheStore(finalTurnKey, detail.InputTokens+detail.OutputTokens, now)
	if lastAssistantIdx == -1 && hasSystemOrTools && detail.InputTokens > newTurnTokens {
		kiroPrefixCacheStore(systemToolsKey, detail.InputTokens-newTurnTokens, now)
	}
	return detail
}

func estimateMessagesTokens(messages []gjson.Result) int64 {
	if len(messages) == 0 {
		return 0
	}
	segments := make([]string, 0, len(messages)*2)
	for _, msg := range messages {
		appendCountString(&segments, msg.Get("role").String())
		appendCountString(&segments, msg.Get("tool_call_id").String())
		collectCountContent(msg.Get("content"), &segments)
		if toolCalls := msg.Get("tool_calls"); toolCalls.IsArray() {
			for _, tc := range toolCalls.Array() {
				appendCountString(&segments, tc.Get("id").String())
				appendCountString(&segments, tc.Get("function.name").String())
				appendCountString(&segments, tc.Get("function.arguments").String())
			}
		}
	}
	return estimateKiroTokens(strings.Join(segments, "\n"))
}
