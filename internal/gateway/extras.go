package gateway

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"va_adk_pilot/internal/killswitch"
)

// ---- base URL ---------------------------------------------------------------

func (s *Server) vertexBase() string {
	if b := os.Getenv("VA_VERTEX_BASE_URL"); b != "" {
		return strings.TrimRight(b, "/")
	}
	return fmt.Sprintf("https://%s-aiplatform.googleapis.com", s.cfg.VertexLocation)
}

// ---- audit log (FR-E1 / FR-H1 evidence) -------------------------------------

var auditMu sync.Mutex

// AuditPath is where every inbound inference request body is appended.
func AuditPath() string {
	if p := os.Getenv("VA_GATEWAY_AUDIT_LOG"); p != "" {
		return p
	}
	return ".out/gateway_audit.jsonl"
}

func (s *Server) audit(r *http.Request, body []byte) {
	rec := map[string]any{
		"ts":   time.Now().UTC().Format(time.RFC3339Nano),
		"path": r.URL.Path,
		"body": json.RawMessage(body),
	}
	if !json.Valid(body) {
		rec["body"] = string(body)
	}
	b, _ := json.Marshal(rec)
	auditMu.Lock()
	defer auditMu.Unlock()
	p := AuditPath()
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(b, '\n'))
	fmt.Printf("[Gateway] %s %s (%d bytes) -> audit %s\n", r.Method, r.URL.Path, len(body), p)
}

// ---- /v1/embeddings -> Vertex text embeddings -------------------------------

func (s *Server) handleEmbeddings(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, _ := io.ReadAll(r.Body)
	s.audit(r, body)
	var req struct {
		Model     string   `json:"model"`
		Input     []string `json:"input"`
		InputType string   `json:"input_type"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.Model == "" {
		req.Model = "text-multilingual-embedding-002"
	}
	task := "RETRIEVAL_DOCUMENT"
	if req.InputType == "query" {
		task = "RETRIEVAL_QUERY"
	}
	type inst struct {
		Content  string `json:"content"`
		TaskType string `json:"task_type"`
	}
	type pred struct {
		Embeddings struct {
			Values     []float64 `json:"values"`
			Statistics struct {
				TokenCount float64 `json:"token_count"`
			} `json:"statistics"`
		} `json:"embeddings"`
	}
	var all []pred
	totalTokens := 0.0
	// Vertex allows a limited batch per predict call.
	for start := 0; start < len(req.Input); start += 64 {
		end := min(start+64, len(req.Input))
		var insts []inst
		for _, t := range req.Input[start:end] {
			insts = append(insts, inst{Content: t, TaskType: task})
		}
		vb, _ := json.Marshal(map[string]any{"instances": insts})
		url := fmt.Sprintf("%s/v1/projects/%s/locations/%s/publishers/google/models/%s:predict",
			s.vertexBase(), s.cfg.VertexProject, s.cfg.VertexLocation, req.Model)
		resp, err := s.postVertex(r, url, vb)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		rb, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			http.Error(w, string(rb), resp.StatusCode)
			return
		}
		var out struct {
			Predictions []pred `json:"predictions"`
		}
		if err := json.Unmarshal(rb, &out); err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		for _, p := range out.Predictions {
			totalTokens += p.Embeddings.Statistics.TokenCount
		}
		all = append(all, out.Predictions...)
	}
	type datum struct {
		Object    string    `json:"object"`
		Index     int       `json:"index"`
		Embedding []float64 `json:"embedding"`
	}
	data := make([]datum, len(all))
	for i, p := range all {
		data[i] = datum{Object: "embedding", Index: i, Embedding: p.Embeddings.Values}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"object": "list", "model": req.Model, "data": data,
		"usage": map[string]int{"prompt_tokens": int(totalTokens), "total_tokens": int(totalTokens)},
	})
}

func (s *Server) postVertex(r *http.Request, url string, body []byte) (*http.Response, error) {
	token, err := s.tokenSource.Token()
	if err != nil {
		return nil, fmt.Errorf("auth error: %w", err)
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token.AccessToken)
	req.Header.Set("Content-Type", "application/json")
	return http.DefaultClient.Do(req)
}

// ---- FR-C1 explicit context caching -----------------------------------------
//
// A client opts in per request with:
//   X-VA-Cache-Breakpoint: system_and_tools   (everything before the breakpoint is cached)
//   X-VA-Cache-TTL: 300s                      (Vertex cachedContent TTL)
// The gateway keys the prefix by sha256(model|systemInstruction|tools), creates
// a Vertex cachedContent on miss, and sends subsequent requests with
// `cachedContent` instead of the prefix. Hit/miss is reported in the
// X-VA-Cache response header and verified by Vertex's cachedContentTokenCount.

const (
	HeaderBreakpoint = "X-VA-Cache-Breakpoint"
	HeaderTTL        = "X-VA-Cache-TTL"
)

type cacheEntry struct {
	name    string
	expires time.Time
	reason  string // set when Vertex rejected the prefix (negative cache)
}

type cacheStats struct {
	Hits, Misses, Bypass int
	CachedTokens         int
	LastBypassReason     string
}

var (
	cacheMu    sync.Mutex
	cacheTable = map[string]cacheEntry{}
	stats      cacheStats
)

func (s *Server) applyCache(r *http.Request, model string, vReq *VertexRequest) string {
	bp := r.Header.Get(HeaderBreakpoint)
	if bp == "" || killswitch.Disabled("C1") {
		return "OFF"
	}
	// Breakpoints:
	//   system_and_tools  cache systemInstruction + tools
	//   after_turn:N      also cache the first N conversation turns (e.g. a
	//                     long reference document). Vertex counts only
	//                     `contents` toward its 1024-token minimum.
	prefixTurns := 0
	if strings.HasPrefix(bp, "after_turn:") {
		n, err := strconv.Atoi(strings.TrimPrefix(bp, "after_turn:"))
		if err != nil || n < 1 || n >= len(vReq.Contents) {
			return s.bypass("invalid breakpoint " + bp)
		}
		prefixTurns = n
	} else if bp != "system_and_tools" {
		return s.bypass("unsupported breakpoint " + bp)
	}
	if model == "" {
		model = os.Getenv("VA_MODEL")
		if model == "" {
			model = "gemini-2.5-flash"
		}
	}
	ttl := r.Header.Get(HeaderTTL)
	if ttl == "" {
		ttl = "300s"
	}
	d, err := time.ParseDuration(ttl)
	if err != nil {
		return s.bypass("bad TTL " + ttl)
	}
	cachedTurns := vReq.Contents[:prefixTurns]
	prefix, _ := json.Marshal([]any{model, vReq.SystemInstruction, vReq.Tools, cachedTurns})
	sum := sha256.Sum256(prefix)
	key := hex.EncodeToString(sum[:])

	cacheMu.Lock()
	e, ok := cacheTable[key]
	cacheMu.Unlock()
	if ok && e.name == "" && time.Now().Before(e.expires) {
		return s.bypass("prefix previously rejected by Vertex (negative-cached): " + e.reason)
	}
	status := "HIT"
	if !ok || time.Now().After(e.expires.Add(-d/10)) {
		status = "MISS"
		body, _ := json.Marshal(map[string]any{
			"model":             fmt.Sprintf("projects/%s/locations/%s/publishers/google/models/%s", s.cfg.VertexProject, s.cfg.VertexLocation, model),
			"systemInstruction": vReq.SystemInstruction,
			"tools":             vReq.Tools,
			"contents":          cachedTurns,
			"ttl":               fmt.Sprintf("%ds", int(d.Seconds())),
		})
		url := fmt.Sprintf("%s/v1/projects/%s/locations/%s/cachedContents", s.vertexBase(), s.cfg.VertexProject, s.cfg.VertexLocation)
		resp, err := s.postVertex(r, url, body)
		if err != nil {
			return s.bypass(err.Error())
		}
		rb, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			reason := fmt.Sprintf("cachedContents.create HTTP %d: %s", resp.StatusCode, truncate(string(rb), 300))
			cacheMu.Lock()
			cacheTable[key] = cacheEntry{expires: time.Now().Add(d), reason: reason}
			cacheMu.Unlock()
			return s.bypass(reason)
		}
		var cc struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(rb, &cc) != nil || cc.Name == "" {
			return s.bypass("cachedContents.create returned no name")
		}
		e = cacheEntry{name: cc.Name, expires: time.Now().Add(d)}
		cacheMu.Lock()
		cacheTable[key] = e
		cacheMu.Unlock()
	}
	vReq.CachedContent = e.name
	vReq.SystemInstruction = nil
	vReq.Tools = nil
	vReq.Contents = vReq.Contents[prefixTurns:]
	cacheMu.Lock()
	if status == "HIT" {
		stats.Hits++
	} else {
		stats.Misses++
	}
	cacheMu.Unlock()
	return status
}

func (s *Server) bypass(reason string) string {
	cacheMu.Lock()
	stats.Bypass++
	stats.LastBypassReason = reason
	cacheMu.Unlock()
	fmt.Printf("[Gateway cache] BYPASS: %s\n", reason)
	return "BYPASS"
}

func (s *Server) recordCacheTokens(status string, n int) {
	if n == 0 {
		return
	}
	cacheMu.Lock()
	stats.CachedTokens += n
	cacheMu.Unlock()
}

func (s *Server) handleCacheStats(w http.ResponseWriter, _ *http.Request) {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"hits": stats.Hits, "misses": stats.Misses, "bypass": stats.Bypass,
		"cached_tokens_total": stats.CachedTokens, "last_bypass_reason": stats.LastBypassReason,
		"entries": len(cacheTable),
	})
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}
