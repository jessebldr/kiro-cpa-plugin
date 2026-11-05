// Package claude provides streaming SSE event building for Claude format.
// This package handles the construction of Claude-compatible Server-Sent Events (SSE)
// for streaming responses from Kiro API.
package claude

import (
	"encoding/json"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

// BuildClaudeMessageStartEvent creates the message_start SSE event
func BuildClaudeMessageStartEvent(model string, inputTokens int64) []byte {
	event := map[string]interface{}{
		"type": "message_start",
		"message": map[string]interface{}{
			"id":            "msg_" + uuid.New().String()[:24],
			"type":          "message",
			"role":          "assistant",
			"content":       []interface{}{},
			"model":         model,
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage":         map[string]interface{}{"input_tokens": inputTokens, "output_tokens": 0},
		},
	}
	result, _ := json.Marshal(event)
	return []byte("event: message_start\ndata: " + string(result))
}

// BuildClaudeContentBlockStartEvent creates a content_block_start SSE event
func BuildClaudeContentBlockStartEvent(index int, blockType, toolUseID, toolName string) []byte {
	var contentBlock map[string]interface{}
	switch blockType {
	case "tool_use":
		contentBlock = map[string]interface{}{
			"type":  "tool_use",
			"id":    toolUseID,
			"name":  toolName,
			"input": map[string]interface{}{},
		}
	case "thinking":
		contentBlock = map[string]interface{}{
			"type":     "thinking",
			"thinking": "",
		}
	default:
		contentBlock = map[string]interface{}{
			"type": "text",
			"text": "",
		}
	}

	event := map[string]interface{}{
		"type":          "content_block_start",
		"index":         index,
		"content_block": contentBlock,
	}
	result, _ := json.Marshal(event)
	return []byte("event: content_block_start\ndata: " + string(result))
}

// BuildClaudeRedactedThinkingBlockStartEvent forwards Kiro's opaque redacted
// reasoning payload without attempting to inspect or recreate it.
func BuildClaudeRedactedThinkingBlockStartEvent(index int, data string) []byte {
	event := map[string]interface{}{
		"type":  "content_block_start",
		"index": index,
		"content_block": map[string]interface{}{
			"type": "redacted_thinking",
			"data": data,
		},
	}
	result, _ := json.Marshal(event)
	return []byte("event: content_block_start\ndata: " + string(result))
}

// BuildClaudeStreamEvent creates a text_delta content_block_delta SSE event
func BuildClaudeStreamEvent(contentDelta string, index int) []byte {
	event := map[string]interface{}{
		"type":  "content_block_delta",
		"index": index,
		"delta": map[string]interface{}{
			"type": "text_delta",
			"text": contentDelta,
		},
	}
	result, _ := json.Marshal(event)
	return []byte("event: content_block_delta\ndata: " + string(result))
}

// BuildClaudeInputJsonDeltaEvent creates an input_json_delta event for tool use streaming
func BuildClaudeInputJsonDeltaEvent(partialJSON string, index int) []byte {
	event := map[string]interface{}{
		"type":  "content_block_delta",
		"index": index,
		"delta": map[string]interface{}{
			"type":         "input_json_delta",
			"partial_json": partialJSON,
		},
	}
	result, _ := json.Marshal(event)
	return []byte("event: content_block_delta\ndata: " + string(result))
}

// BuildClaudeContentBlockStopEvent creates a content_block_stop SSE event
func BuildClaudeContentBlockStopEvent(index int) []byte {
	event := map[string]interface{}{
		"type":  "content_block_stop",
		"index": index,
	}
	result, _ := json.Marshal(event)
	return []byte("event: content_block_stop\ndata: " + string(result))
}

// BuildClaudeThinkingBlockStopEvent creates a content_block_stop SSE event for thinking blocks.
func BuildClaudeThinkingBlockStopEvent(index int) []byte {
	event := map[string]interface{}{
		"type":  "content_block_stop",
		"index": index,
	}
	result, _ := json.Marshal(event)
	return []byte("event: content_block_stop\ndata: " + string(result))
}

// BuildClaudeMessageDeltaEvent creates the message_delta event with stop_reason and usage
func BuildClaudeMessageDeltaEvent(stopReason string, usageInfo usage.Detail) []byte {
	deltaEvent := map[string]interface{}{
		"type": "message_delta",
		"delta": map[string]interface{}{
			"stop_reason":   stopReason,
			"stop_sequence": nil,
		},
		"usage": buildClaudeUsageMap(usageInfo),
	}
	deltaResult, _ := json.Marshal(deltaEvent)
	return []byte("event: message_delta\ndata: " + string(deltaResult))
}

func buildClaudeUsageMap(usageInfo usage.Detail) map[string]interface{} {
	cacheRead := usageInfo.CacheReadTokens
	if cacheRead == 0 && usageInfo.CachedTokens > 0 {
		cacheRead = usageInfo.CachedTokens
	}
	cacheCreation := usageInfo.CacheCreationTokens
	uncachedInput := usageInfo.InputTokens - cacheRead - cacheCreation
	if uncachedInput < 0 {
		uncachedInput = 0
	}
	usageMap := map[string]interface{}{
		"input_tokens":  uncachedInput,
		"output_tokens": usageInfo.OutputTokens,
	}
	if cacheRead > 0 {
		usageMap["cache_read_input_tokens"] = cacheRead
	}
	if cacheCreation > 0 {
		usageMap["cache_creation_input_tokens"] = cacheCreation
	}
	if usageInfo.ReasoningTokens > 0 {
		usageMap["output_tokens_details"] = map[string]interface{}{
			"thinking_tokens": usageInfo.ReasoningTokens,
		}
	}
	return usageMap
}

// BuildClaudeMessageStopOnlyEvent creates only the message_stop event
func BuildClaudeMessageStopOnlyEvent() []byte {
	stopEvent := map[string]interface{}{
		"type": "message_stop",
	}
	stopResult, _ := json.Marshal(stopEvent)
	return []byte("event: message_stop\ndata: " + string(stopResult))
}

// BuildClaudeThinkingDeltaEvent creates a thinking_delta event for Claude API compatibility.
// This is used when streaming thinking content wrapped in <thinking> tags.
func BuildClaudeThinkingDeltaEvent(thinkingDelta string, index int) []byte {
	event := map[string]interface{}{
		"type":  "content_block_delta",
		"index": index,
		"delta": map[string]interface{}{
			"type":     "thinking_delta",
			"thinking": thinkingDelta,
		},
	}
	result, _ := json.Marshal(event)
	return []byte("event: content_block_delta\ndata: " + string(result))
}

// BuildClaudeSignatureDeltaEvent forwards the signature supplied by Kiro for
// an official reasoningContentEvent.
func BuildClaudeSignatureDeltaEvent(signature string, index int) []byte {
	event := map[string]interface{}{
		"type":  "content_block_delta",
		"index": index,
		"delta": map[string]interface{}{
			"type":      "signature_delta",
			"signature": signature,
		},
	}
	result, _ := json.Marshal(event)
	return []byte("event: content_block_delta\ndata: " + string(result))
}
