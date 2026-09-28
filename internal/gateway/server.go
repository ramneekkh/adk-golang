package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"va_adk_pilot/internal/config"
)

type Server struct {
	cfg         config.GatewayConfig
	tokenSource oauth2.TokenSource
}

func NewServer(cfg config.GatewayConfig) (*Server, error) {
	ctx := context.Background()
	ts, err := google.DefaultTokenSource(ctx, "https://www.googleapis.com/auth/cloud-platform")
	if err != nil {
		return nil, err
	}
	return &Server{cfg: cfg, tokenSource: ts}, nil
}

// NewServerWithTokenSource is used by tests (fake Vertex endpoint).
func NewServerWithTokenSource(cfg config.GatewayConfig, ts oauth2.TokenSource) *Server {
	return &Server{cfg: cfg, tokenSource: ts}
}

func (s *Server) Start() error {
	return http.ListenAndServe(":"+s.cfg.Port, s.Handler())
}

// Handler returns the gateway's HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	})
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"object":"list","data":[{"id":"` + config.GetModel() + `","object":"model","owned_by":"google"}]}`))
	})
	mux.HandleFunc("/v1/chat/completions", s.handleChatCompletions)
	mux.HandleFunc("/v1/responses", s.handleResponses)
	mux.HandleFunc("/v1/embeddings", s.handleEmbeddings)
	mux.HandleFunc("/v1/cache/stats", s.handleCacheStats)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		io.ReadAll(r.Body)
		w.WriteHeader(http.StatusNotFound)
	})
	return mux
}

func (s *Server) hitVertex(ctx context.Context, model string, vReq VertexRequest, stream bool) (*http.Response, error) {
	if model == "" {
		model = config.GetModel()
	}
	// reject if bogus model
	if !strings.HasPrefix(model, "gemini-") {
		return nil, fmt.Errorf("bogus model: %s", model)
	}

	reqBody, _ := json.Marshal(vReq)

	url := fmt.Sprintf("%s/v1/projects/%s/locations/%s/publishers/google/models/%s:generateContent",
		s.vertexBase(), s.cfg.VertexProject, s.cfg.VertexLocation, model)

	if stream {
		url = fmt.Sprintf("%s/v1/projects/%s/locations/%s/publishers/google/models/%s:streamGenerateContent?alt=sse",
			s.vertexBase(), s.cfg.VertexProject, s.cfg.VertexLocation, model)
	}

	token, err := s.tokenSource.Token()
	if err != nil {
		fmt.Printf("[Gateway hitVertex Error] Token error: %v\n", err)
		return nil, fmt.Errorf("auth error: %w", err)
	}

	vHttpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewBuffer(reqBody))
	if err != nil {
		fmt.Printf("[Gateway hitVertex Error] NewRequest error: %v\n", err)
		return nil, err
	}
	vHttpReq.Header.Set("Authorization", "Bearer "+token.AccessToken)
	vHttpReq.Header.Set("Content-Type", "application/json")

	client := &http.Client{}
	res, err := client.Do(vHttpReq)
	if err != nil {
		fmt.Printf("[Gateway hitVertex Error] Do error: %v\n", err)
	}
	return res, err
}

func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	fmt.Printf("[Gateway] Received POST /v1/chat/completions\n")
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	bodyBytes, _ := io.ReadAll(r.Body)
	var oReq OpenAIRequest
	if err := json.Unmarshal(bodyBytes, &oReq); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	vReq := VertexRequest{}

	// Convert OpenAI Messages to Vertex Contents
	for _, msg := range oReq.Messages {
		if msg.Role == "system" {
			vReq.SystemInstruction = &VertexContent{
				Role:  "system",
				Parts: []VertexPart{{Text: msg.Content}},
			}
			continue
		}

		role := msg.Role
		if role == "assistant" {
			role = "model"
		} else if role == "tool" {
			role = "user"
		}

		vp := VertexPart{}
		if len(msg.ToolCalls) > 0 {
			var args map[string]any
			json.Unmarshal([]byte(msg.ToolCalls[0].Function.Arguments), &args)
			vp.FunctionCall = &VertexFunctionCall{
				Name: msg.ToolCalls[0].Function.Name,
				Args: args,
			}
			role = "model"
		} else if msg.Role == "tool" {
			var resp map[string]any
			json.Unmarshal([]byte(msg.Content), &resp)
			name := msg.Name
			if name == "" {
				for i := len(oReq.Messages) - 1; i >= 0; i-- {
					for _, tc := range oReq.Messages[i].ToolCalls {
						if tc.ID == msg.ToolCallID {
							name = tc.Function.Name
							break
						}
					}
				}
			}
			vp.FunctionResponse = &VertexFunctionResponse{
				Name:     name,
				Response: resp,
			}
			role = "user"
		} else {
			vp.Text = msg.Content
		}

		vReq.Contents = append(vReq.Contents, VertexContent{
			Role:  role,
			Parts: []VertexPart{vp},
		})
	}

	if len(oReq.Tools) > 0 {
		var decls []VertexFunctionDeclaration
		for _, t := range oReq.Tools {
			decls = append(decls, VertexFunctionDeclaration{
				Name:        t.Function.Name,
				Description: t.Function.Description,
				Parameters:  t.Function.Parameters,
			})
		}
		vReq.Tools = append(vReq.Tools, VertexTool{FunctionDeclarations: decls})
	}

	if len(vReq.Contents) == 0 {
		vReq.Contents = append(vReq.Contents, VertexContent{
			Role:  "user",
			Parts: []VertexPart{{Text: "Hi"}},
		})
	}

	resp, err := s.hitVertex(r.Context(), oReq.Model, vReq, oReq.Stream)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		http.Error(w, string(body), resp.StatusCode)
		return
	}

	var vResp VertexResponse
	if err := json.NewDecoder(resp.Body).Decode(&vResp); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	oResp := OpenAIResponse{
		ID:      "chatcmpl-123",
		Object:  "chat.completion",
		Created: 1677652288,
		Model:   oReq.Model,
	}
	if oResp.Model == "" {
		oResp.Model = config.GetModel()
	}

	if len(vResp.Candidates) > 0 {
		var text string
		var tcs []OpenAIToolCall
		var callIdCounter int
		stop := "stop"

		for _, part := range vResp.Candidates[0].Content.Parts {
			if part.FunctionCall != nil {
				callIdCounter++
				argsBytes, _ := json.Marshal(part.FunctionCall.Args)
				tcs = append(tcs, OpenAIToolCall{
					ID:   fmt.Sprintf("call-%d", callIdCounter),
					Type: "function",
				})
				tcs[len(tcs)-1].Function.Name = part.FunctionCall.Name
				tcs[len(tcs)-1].Function.Arguments = string(argsBytes)
				stop = "tool_calls"
			} else {
				text += part.Text
			}
		}

		oResp.Choices = append(oResp.Choices, OpenAIChoice{
			Index: 0,
			Message: OpenAIMessage{
				Role:      "assistant",
				Content:   text,
				ToolCalls: tcs,
			},
			FinishReason: &stop,
		})
	}

	if vResp.UsageMetadata != nil {
		oResp.Usage = &OpenAIUsage{
			PromptTokens:     vResp.UsageMetadata.PromptTokenCount,
			CompletionTokens: vResp.UsageMetadata.CandidatesTokenCount,
			TotalTokens:      vResp.UsageMetadata.TotalTokenCount,
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(oResp)
}

// === TOOL STRUCT ADDS ===
type ADKTool struct {
	Type        string         `json:"type"`
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

type ADKInputContent struct {
	Text string `json:"text"`
	Type string `json:"type"`
}

type ADKInput struct {
	Role    string            `json:"role,omitempty"`
	Content []ADKInputContent `json:"content,omitempty"`
	Type    string            `json:"type,omitempty"`

	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
	Output    string `json:"output,omitempty"`
	CallID    string `json:"call_id,omitempty"`
}

type ADKRequest struct {
	Instructions string     `json:"instructions"`
	Input        []ADKInput `json:"input"`
	Model        string     `json:"model"`
	Stream       bool
	Tools        []ADKTool `json:"tools,omitempty"`
}

func (s *Server) handleResponses(w http.ResponseWriter, r *http.Request) {
	fmt.Printf("[Gateway] Received POST /v1/responses\n")
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	bodyBytes, _ := io.ReadAll(r.Body)
	s.audit(r, bodyBytes)

	var adkReq ADKRequest
	if err := json.Unmarshal(bodyBytes, &adkReq); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	vReq := VertexRequest{}

	if adkReq.Instructions != "" {
		vReq.SystemInstruction = &VertexContent{
			Role:  "system",
			Parts: []VertexPart{{Text: adkReq.Instructions}},
		}
	}

	for _, msg := range adkReq.Input {
		vc := VertexContent{
			Role: msg.Role,
		}
		if vc.Role == "assistant" {
			vc.Role = "model"
		}
		if msg.Type == "function_call" {
			var args map[string]any
			json.Unmarshal([]byte(msg.Arguments), &args)
			vc.Parts = append(vc.Parts, VertexPart{
				FunctionCall: &VertexFunctionCall{
					Name: msg.Name,
					Args: args,
				},
			})
			vc.Role = "model"
		} else if msg.Type == "function_call_output" {
			var resp map[string]any
			json.Unmarshal([]byte(msg.Output), &resp)
			name := msg.Name
			if name == "" {
				for i := len(adkReq.Input) - 1; i >= 0; i-- {
					if adkReq.Input[i].Type == "function_call" && adkReq.Input[i].CallID == msg.CallID {
						name = adkReq.Input[i].Name
						break
					}
				}
			}
			vc.Parts = append(vc.Parts, VertexPart{
				FunctionResponse: &VertexFunctionResponse{
					Name:     name,
					Response: resp,
				},
			})
			vc.Role = "user"
		} else {
			for _, c := range msg.Content {
				if c.Text != "" {
					vc.Parts = append(vc.Parts, VertexPart{Text: c.Text})
				}
			}
		}
		if len(vc.Parts) > 0 {
			vReq.Contents = append(vReq.Contents, vc)
		}
	}

	if len(adkReq.Tools) > 0 {
		var decls []VertexFunctionDeclaration
		for _, t := range adkReq.Tools {
			decls = append(decls, VertexFunctionDeclaration{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.Parameters,
			})
		}
		vReq.Tools = append(vReq.Tools, VertexTool{FunctionDeclarations: decls})
	}

	if len(vReq.Contents) == 0 {
		// Fallback or skip
		vReq.Contents = append(vReq.Contents, VertexContent{
			Role:  "user",
			Parts: []VertexPart{{Text: "Hi"}},
		})
	}

	cacheStatus := s.applyCache(r, adkReq.Model, &vReq)
	w.Header().Set("X-VA-Cache", cacheStatus)

	resp, err := s.hitVertex(r.Context(), adkReq.Model, vReq, false)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		fmt.Printf("[Gateway Error] Vertex returned status %d: %s\n", resp.StatusCode, string(body))
		http.Error(w, string(body), resp.StatusCode)
		return
	}

	var vResp VertexResponse
	if err := json.NewDecoder(resp.Body).Decode(&vResp); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	type ADKOutputContent struct {
		Text string `json:"text"`
		Type string `json:"type"`
	}
	type inputDetails struct {
		CachedTokens int `json:"cached_tokens"`
	}
	type OpenAIResponseUsage struct {
		InputTokens        int          `json:"input_tokens"`
		OutputTokens       int          `json:"output_tokens"`
		TotalTokens        int          `json:"total_tokens"`
		InputTokensDetails inputDetails `json:"input_tokens_details"`
	}
	type ADKOutputItem struct {
		Type      string             `json:"type"`
		Role      string             `json:"role,omitempty"`
		Content   []ADKOutputContent `json:"content,omitempty"`
		Name      string             `json:"name,omitempty"`
		Arguments string             `json:"arguments,omitempty"`
		CallID    string             `json:"call_id,omitempty"`
	}
	type ADKResponse struct {
		Output []ADKOutputItem      `json:"output"`
		Usage  *OpenAIResponseUsage `json:"usage,omitempty"`
	}

	adkR := ADKResponse{}

	var callIdCounter int
	if len(vResp.Candidates) > 0 {
		for _, part := range vResp.Candidates[0].Content.Parts {
			if part.FunctionCall != nil {
				callIdCounter++
				argsBytes, _ := json.Marshal(part.FunctionCall.Args)
				adkR.Output = append(adkR.Output, ADKOutputItem{
					Type:      "function_call",
					Name:      part.FunctionCall.Name,
					Arguments: string(argsBytes),
					CallID:    fmt.Sprintf("call-%d", callIdCounter),
				})
			} else if part.Text != "" {
				adkR.Output = append(adkR.Output, ADKOutputItem{
					Type:    "message",
					Role:    "assistant",
					Content: []ADKOutputContent{{Text: part.Text, Type: "output_text"}},
				})
			}
		}
	}

	if vResp.UsageMetadata != nil {
		adkR.Usage = &OpenAIResponseUsage{
			InputTokens:        vResp.UsageMetadata.PromptTokenCount,
			OutputTokens:       vResp.UsageMetadata.CandidatesTokenCount,
			TotalTokens:        vResp.UsageMetadata.TotalTokenCount,
			InputTokensDetails: inputDetails{CachedTokens: vResp.UsageMetadata.CachedContentTokenCount},
		}
		s.recordCacheTokens(cacheStatus, vResp.UsageMetadata.CachedContentTokenCount)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(adkR)
}
