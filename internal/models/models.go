// Package models builds the gateway-backed model clients used by agents.
//
// Every agent talks to the in-repo LLM gateway (FR-H1) through ADK's
// OpenAI-compatible model; the model name comes from VA_MODEL (swap = config).
// FR-C1: agents listed in VA_CACHE_AGENTS (default "all") send an explicit
// cache breakpoint after system instruction + tools, with TTL VA_CACHE_TTL.
package models

import (
	"context"
	"os"
	"strings"

	"github.com/openai/openai-go/v3/option"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/model/openaimodel"

	"va_adk_pilot/internal/config"
	"va_adk_pilot/internal/nostream"
)

// CacheEnabled reports whether the agent opts into the gateway context cache.
func CacheEnabled(agentName string) bool {
	v, ok := os.LookupEnv("VA_CACHE_AGENTS")
	if !ok {
		v = "all"
	}
	for _, a := range strings.Split(v, ",") {
		a = strings.TrimSpace(a)
		if a == "all" || a == agentName {
			return true
		}
	}
	return false
}

// ForAgent returns the model client for an agent.
func ForAgent(ctx context.Context, agentName string) (model.LLM, error) {
	var opts []option.RequestOption
	opts = append(opts, option.WithHeader("X-VA-Agent", agentName))
	if CacheEnabled(agentName) {
		ttl := os.Getenv("VA_CACHE_TTL")
		if ttl == "" {
			ttl = "300s"
		}
		opts = append(opts,
			option.WithHeader("X-VA-Cache-Breakpoint", "system_and_tools"),
			option.WithHeader("X-VA-Cache-TTL", ttl))
	}
	m, err := openaimodel.NewModel(ctx, config.GetModel(), &openaimodel.ClientConfig{
		BaseURL: config.GetGatewayURL(),
		APIKey:  "gateway-internal",
		Options: opts,
	})
	if err != nil {
		return nil, err
	}
	return nostream.Wrap(m), nil
}

func MustForAgent(ctx context.Context, agentName string) model.LLM {
	m, err := ForAgent(ctx, agentName)
	if err != nil {
		panic(err)
	}
	return m
}
