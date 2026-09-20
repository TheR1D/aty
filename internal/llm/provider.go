package llm

import "encoding/json"

const (
	llamaName            = "llama"
	openAICompatibleName = "openai-compatible"
	ollamaName           = "ollama"
	reasoningNone        = "none"
)

type providerProtocol struct {
	name        string
	encode      func(chatRequest) ([]byte, error)
	warm        func(chatRequest) chatRequest
	nativeCache bool
	responses   bool
}

func NewLlama(opts Options) *Client {
	return newClient(opts, providerProtocol{
		name: llamaName, encode: encodeLlama,
		warm: llamaCacheRequest,
	})
}

func NewOpenAICompatible(opts Options) *Client {
	return newClient(opts, providerProtocol{
		name: openAICompatibleName, encode: encodeOpenAICompatible,
		warm: openAICompatibleCacheRequest, nativeCache: true,
	})
}

func NewOllama(opts Options) *Client {
	return newClient(opts, providerProtocol{
		name: ollamaName, encode: encodeOllama,
		warm: oneOutputCacheRequest,
	})
}

// chatBody holds only the wire fields shared by every provider. Dialect-specific
// controls stay in the enclosing payload so they cannot leak to other backends.
type chatBody struct {
	Model           string     `json:"model"`
	Messages        []message  `json:"messages"`
	Stream          bool       `json:"stream,omitempty"`
	Temperature     *float64   `json:"temperature,omitempty"`
	ReasoningEffort string     `json:"reasoning_effort,omitempty"`
	Tools           []toolSpec `json:"tools,omitempty"`
}

func requestBody(req chatRequest) chatBody {
	return chatBody{
		Model: req.Model, Messages: req.Messages, Stream: req.Stream,
		Temperature: req.Temperature, ReasoningEffort: req.ReasoningEffort,
		Tools: req.Tools,
	}
}

type llamaBody struct {
	chatBody
	ToolChoice  string `json:"tool_choice,omitempty"`
	CachePrompt bool   `json:"cache_prompt"`
	MaxTokens   *int   `json:"max_tokens,omitempty"`
}

func encodeLlama(req chatRequest) ([]byte, error) {
	return json.Marshal(llamaBody{
		chatBody: requestBody(req), ToolChoice: req.ToolChoice, CachePrompt: true,
		MaxTokens: req.MaxOutputTokens,
	})
}

func llamaCacheRequest(req chatRequest) chatRequest {
	req.MaxOutputTokens = new(0)
	return req
}

type compatibleBody struct {
	chatBody
	ToolChoice     string `json:"tool_choice,omitempty"`
	PromptCacheKey string `json:"prompt_cache_key,omitempty"`
	MaxTokens      *int   `json:"max_completion_tokens,omitempty"`
}

func encodeOpenAICompatible(req chatRequest) ([]byte, error) {
	return json.Marshal(compatibleBody{
		chatBody: requestBody(req), ToolChoice: req.ToolChoice,
		PromptCacheKey: req.PromptCacheKey, MaxTokens: req.MaxOutputTokens,
	})
}

func openAICompatibleCacheRequest(req chatRequest) chatRequest {
	req.MaxOutputTokens = new(1024)
	return req
}

type ollamaBody struct {
	chatBody
	Think     *bool `json:"think,omitempty"`
	MaxTokens *int  `json:"max_tokens,omitempty"`
}

func encodeOllama(req chatRequest) ([]byte, error) {
	return json.Marshal(ollamaBody{
		chatBody: requestBody(req), Think: new(req.Thinking),
		MaxTokens: req.MaxOutputTokens,
	})
}
