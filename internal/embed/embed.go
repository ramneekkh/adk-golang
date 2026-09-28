// Package embed provides text embedders for the RAG retriever.
//
//   - Gateway: real embeddings via the in-repo LLM gateway (/v1/embeddings ->
//     Vertex text-multilingual-embedding-002). Default.
//   - Local: deterministic hashed character-trigram vectors, for offline tests
//     (VA_EMBEDDER=local). A genuine lexical embedding, not canned output.
//
// Every call is traced as an "embeddings" span carrying both the input text
// and the output vector, as generic GenAI instrumentation would; FR-D1's span
// processor policy then decides which of those attributes are exported.
package embed

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"math"
	"net/http"
	"os"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
)

// Attribute keys emitted on embedding spans.
const (
	AttrInput  = "gen_ai.embeddings.input"
	AttrVector = "gen_ai.response.embeddings"
	AttrModel  = "gen_ai.request.model"
	AttrDims   = "gen_ai.embeddings.dimension.count"
)

type Embedder interface {
	Name() string
	// Embed returns one vector per input. kind is "query" or "document".
	Embed(ctx context.Context, texts []string, kind string) ([][]float64, error)
}

// FromEnv picks the embedder from VA_EMBEDDER (gateway|local).
func FromEnv(gatewayURL string) Embedder {
	if os.Getenv("VA_EMBEDDER") == "local" {
		return Local{Dims: 512}
	}
	m := os.Getenv("VA_EMBED_MODEL")
	if m == "" {
		m = "text-multilingual-embedding-002"
	}
	return &Gateway{BaseURL: strings.TrimRight(gatewayURL, "/"), Model: m, Client: &http.Client{Timeout: 60 * time.Second}}
}

// Traced wraps an embedder with an OpenTelemetry span per call.
func Traced(e Embedder) Embedder { return traced{e} }

type traced struct{ Embedder }

func (t traced) Embed(ctx context.Context, texts []string, kind string) ([][]float64, error) {
	ctx, span := otel.Tracer("va_adk_pilot/embed").Start(ctx, "embeddings "+t.Name())
	defer span.End()
	span.SetAttributes(
		attribute.String("gen_ai.operation.name", "embeddings"),
		attribute.String(AttrModel, t.Name()),
		attribute.String("va.embed.kind", kind),
		attribute.StringSlice(AttrInput, texts),
	)
	vecs, err := t.Embedder.Embed(ctx, texts, kind)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	if len(vecs) > 0 {
		span.SetAttributes(attribute.Int(AttrDims, len(vecs[0])))
		b, _ := json.Marshal(vecs)
		span.SetAttributes(attribute.String(AttrVector, string(b)))
	}
	return vecs, nil
}

// Gateway calls POST {BaseURL}/embeddings (OpenAI-compatible).
type Gateway struct {
	BaseURL string
	Model   string
	Client  *http.Client
}

func (g *Gateway) Name() string { return g.Model }

func (g *Gateway) Embed(ctx context.Context, texts []string, kind string) ([][]float64, error) {
	body, _ := json.Marshal(map[string]any{"model": g.Model, "input": texts, "input_type": kind})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.BaseURL+"/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := g.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embeddings gateway: %w", err)
	}
	defer resp.Body.Close()
	var out struct {
		Data []struct {
			Index     int       `json:"index"`
			Embedding []float64 `json:"embedding"`
		} `json:"data"`
		Error any `json:"error"`
	}
	if resp.StatusCode != http.StatusOK {
		var b bytes.Buffer
		_, _ = b.ReadFrom(resp.Body)
		return nil, fmt.Errorf("embeddings gateway HTTP %d: %s", resp.StatusCode, b.String())
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if len(out.Data) != len(texts) {
		return nil, fmt.Errorf("embeddings gateway returned %d vectors for %d inputs", len(out.Data), len(texts))
	}
	vecs := make([][]float64, len(texts))
	for _, d := range out.Data {
		vecs[d.Index] = d.Embedding
	}
	return vecs, nil
}

// Local is a hashed character-trigram embedder (L2-normalized).
type Local struct{ Dims int }

func (l Local) Name() string { return fmt.Sprintf("local-trigram-%d", l.Dims) }

func (l Local) Embed(_ context.Context, texts []string, _ string) ([][]float64, error) {
	out := make([][]float64, len(texts))
	for i, t := range texts {
		v := make([]float64, l.Dims)
		r := []rune(strings.ToLower(strings.Join(strings.Fields(t), " ")))
		for j := 0; j+3 <= len(r); j++ {
			h := fnv.New32a()
			h.Write([]byte(string(r[j : j+3])))
			v[int(h.Sum32())%l.Dims]++
		}
		Normalize(v)
		out[i] = v
	}
	return out, nil
}

// Normalize L2-normalizes v in place.
func Normalize(v []float64) {
	var s float64
	for _, x := range v {
		s += x * x
	}
	if s == 0 {
		return
	}
	n := math.Sqrt(s)
	for i := range v {
		v[i] /= n
	}
}

// Cosine similarity.
func Cosine(a, b []float64) float64 {
	var dot, na, nb float64
	for i := range a {
		if i >= len(b) {
			break
		}
		dot += a[i] * b[i]
		na += a[i] * a[i]
		nb += b[i] * b[i]
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}
