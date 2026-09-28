// Package replay implements FR-H3: record every model exchange of a session
// (request fingerprint + response) and replay it deterministically through
// the identical agent flow. Replay is strict: if the flow sends a request
// whose fingerprint differs from the recording, replay fails instead of
// silently calling a live model.
package replay

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"iter"
	"os"
	"sync"

	"google.golang.org/adk/v2/model"
)

// Entry is one recorded exchange.
type Entry struct {
	Seq         int                `json:"seq"`
	Model       string             `json:"model"`
	Fingerprint string             `json:"request_fingerprint"`
	Response    *model.LLMResponse `json:"response"`
}

// Fingerprint hashes the parts of a request that determine the model output:
// system instruction, contents and tool declarations.
func Fingerprint(req *model.LLMRequest) string {
	var sys any
	var tools any
	if req.Config != nil {
		sys = req.Config.SystemInstruction
		tools = req.Config.Tools
	}
	b, _ := json.Marshal([]any{req.Model, sys, req.Contents, tools})
	// Function-call IDs are transport identifiers (may be generated per run);
	// drop them so the fingerprint reflects content only.
	var generic any
	_ = json.Unmarshal(b, &generic)
	stripIDs(generic)
	b, _ = json.Marshal(generic)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// Recorder wraps a live model and appends every exchange to a JSONL file.
type Recorder struct {
	model.LLM
	File string
	mu   sync.Mutex
	seq  int
}

func (r *Recorder) GenerateContent(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
	fp := Fingerprint(req)
	origin := r.LLM.GenerateContent(ctx, req, false)
	return func(yield func(*model.LLMResponse, error) bool) {
		for resp, err := range origin {
			if err == nil && resp != nil {
				r.mu.Lock()
				r.seq++
				b, _ := json.Marshal(Entry{Seq: r.seq, Model: r.LLM.Name(), Fingerprint: fp, Response: resp})
				if f, ferr := os.OpenFile(r.File, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); ferr == nil {
					_, _ = f.Write(append(b, '\n'))
					f.Close()
				}
				r.mu.Unlock()
			}
			if !yield(resp, err) {
				return
			}
		}
	}
}

// Player serves recorded responses in order, verifying fingerprints.
type Player struct {
	entries []Entry
	mu      sync.Mutex
	idx     int
	name    string
}

// Load reads a recording.
func Load(file string) (*Player, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	p := &Player{name: "replay:" + file}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		if len(sc.Bytes()) == 0 {
			continue
		}
		var e Entry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			return nil, err
		}
		p.entries = append(p.entries, e)
		if e.Model != "" {
			p.name = e.Model // replay presents as the recorded model
		}
	}
	return p, sc.Err()
}

func (p *Player) Name() string { return p.name }

// Remaining returns the number of unplayed entries.
func (p *Player) Remaining() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.entries) - p.idx
}

func (p *Player) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	p.mu.Lock()
	defer p.mu.Unlock()
	fp := Fingerprint(req)
	if p.idx >= len(p.entries) {
		return func(yield func(*model.LLMResponse, error) bool) {
			yield(nil, fmt.Errorf("replay: flow made more model calls than recorded (%d)", len(p.entries)))
		}
	}
	e := p.entries[p.idx]
	p.idx++
	if e.Fingerprint != fp {
		return func(yield func(*model.LLMResponse, error) bool) {
			yield(nil, fmt.Errorf("replay divergence at call %d: request fingerprint %s != recorded %s", e.Seq, fp[:12], e.Fingerprint[:12]))
		}
	}
	return func(yield func(*model.LLMResponse, error) bool) { yield(e.Response, nil) }
}

func stripIDs(v any) {
	switch t := v.(type) {
	case map[string]any:
		for _, k := range []string{"functionCall", "functionResponse"} {
			if m, ok := t[k].(map[string]any); ok {
				delete(m, "id")
			}
		}
		for _, val := range t {
			stripIDs(val)
		}
	case []any:
		for _, val := range t {
			stripIDs(val)
		}
	}
}
