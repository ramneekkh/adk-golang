package main

import (
	"fmt"
	"os"

	"va_adk_pilot/internal/config"
	"va_adk_pilot/internal/gateway"
)

func main() {
	cfg := config.LoadGatewayConfig()
	server, err := gateway.NewServer(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to start gateway: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Gateway starting on port %s\n", cfg.Port)
	if err := server.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "Gateway error: %v\n", err)
		os.Exit(1)
	}
}
