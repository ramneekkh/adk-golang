package main

import (
	"context"
	"fmt"
	"os"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/cmd/launcher"
	"google.golang.org/adk/v2/cmd/launcher/full"
	"google.golang.org/adk/v2/plugin"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/telemetry"

	"va_adk_pilot/internal/agents/loan"
	"va_adk_pilot/internal/agents/orchestrator"
	"va_adk_pilot/internal/agents/rag"
	"va_adk_pilot/internal/agents/transfer"
	"va_adk_pilot/internal/models"
	"va_adk_pilot/internal/privacy"
	mytelemetry "va_adk_pilot/internal/telemetry"
)

func main() {
	ctx := context.Background()

	must := func(a agent.Agent, err error) agent.Agent {
		if err != nil {
			panic(err)
		}
		return a
	}

	// One gateway-backed model client per agent so caching policy (FR-C1) can
	// be set per agent. All inference goes through the gateway (FR-H1).
	ragAgent := must(rag.NewAgent(models.MustForAgent(ctx, "rag")))
	loanAgent := must(loan.NewAgent(models.MustForAgent(ctx, "loan")))
	transferAgent := must(transfer.NewAgent(models.MustForAgent(ctx, "transfer")))
	orchAgent := must(orchestrator.NewAgent(models.MustForAgent(ctx, "orchestrator"), ragAgent, loanAgent, transferAgent))

	// Root the web app on the orchestrator (sub-agents are reachable through it).
	agentLoader := agent.NewSingleLoader(orchAgent)

	privacyPlugin, err := privacy.NewPlugin()
	if err != nil {
		panic(err)
	}
	exporter := mytelemetry.NewFileExporter()
	fmt.Println("[web] OTel spans (redacted, FR-D1/D2/D3) ->", exporter.Path())

	appConfig := &launcher.Config{
		AgentLoader:  agentLoader,
		PluginConfig: runner.PluginConfig{Plugins: []*plugin.Plugin{privacyPlugin}},
		TelemetryOptions: []telemetry.Option{
			telemetry.WithSpanProcessors(mytelemetry.NewProcessor(exporter)),
		},
	}

	webPort := os.Getenv("VA_WEB_PORT")
	if webPort == "" {
		webPort = "8080"
	}

	args := []string{
		"web",
		"--port=" + webPort,
		"webui",
		"--api_server_address=/api",
		"api",
		"--webui_address=*",
		"--include_debug_api=true",
	}
	if err := full.NewLauncher().Execute(ctx, appConfig, args); err != nil {
		fmt.Println("Error:", err)
		os.Exit(1)
	}
}
