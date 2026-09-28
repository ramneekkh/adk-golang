package gateway

type OpenAIFunction struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}
type OpenAITool struct {
	Type     string         `json:"type"`
	Function OpenAIFunction `json:"function"`
}
type OpenAIToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}
type OpenAIMessage struct {
	Role       string           `json:"role"`
	Content    string           `json:"content"`
	ToolCalls  []OpenAIToolCall `json:"tool_calls,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
	Name       string           `json:"name,omitempty"`
}

type OpenAIRequest struct {
	Model    string          `json:"model"`
	Messages []OpenAIMessage `json:"messages"`
	Stream   bool            `json:"stream"`
	Tools    []OpenAITool    `json:"tools,omitempty"`
}

type OpenAIChoice struct {
	Index        int           `json:"index"`
	Message      OpenAIMessage `json:"message,omitempty"`
	Delta        OpenAIMessage `json:"delta,omitempty"`
	FinishReason *string       `json:"finish_reason"`
}

type OpenAIUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type OpenAIResponse struct {
	ID      string         `json:"id"`
	Object  string         `json:"object"`
	Created int64          `json:"created"`
	Model   string         `json:"model"`
	Choices []OpenAIChoice `json:"choices"`
	Usage   *OpenAIUsage   `json:"usage,omitempty"`
}

type VertexFunctionCall struct {
	Name string         `json:"name"`
	Args map[string]any `json:"args"`
}

type VertexFunctionResponse struct {
	Name     string         `json:"name"`
	Response map[string]any `json:"response"`
}

type VertexPart struct {
	Text             string                  `json:"text,omitempty"`
	FunctionCall     *VertexFunctionCall     `json:"functionCall,omitempty"`
	FunctionResponse *VertexFunctionResponse `json:"functionResponse,omitempty"`
}

type VertexContent struct {
	Role  string       `json:"role"`
	Parts []VertexPart `json:"parts"`
}

type VertexTool struct {
	FunctionDeclarations []VertexFunctionDeclaration `json:"functionDeclarations"`
}

type VertexFunctionDeclaration struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

type VertexRequest struct {
	SystemInstruction *VertexContent  `json:"systemInstruction,omitempty"`
	Contents          []VertexContent `json:"contents"`
	Tools             []VertexTool    `json:"tools,omitempty"`
	CachedContent     string          `json:"cachedContent,omitempty"`
}

type VertexCandidate struct {
	Content      VertexContent `json:"content"`
	FinishReason string        `json:"finishReason"`
}

type VertexUsageMetadata struct {
	PromptTokenCount        int `json:"promptTokenCount"`
	CandidatesTokenCount    int `json:"candidatesTokenCount"`
	TotalTokenCount         int `json:"totalTokenCount"`
	CachedContentTokenCount int `json:"cachedContentTokenCount,omitempty"`
}

type VertexResponse struct {
	Candidates    []VertexCandidate    `json:"candidates"`
	UsageMetadata *VertexUsageMetadata `json:"usageMetadata,omitempty"`
}
