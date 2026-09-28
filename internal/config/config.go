package config

import (
	"os"
)

// GatewayConfig holds configuration for the LiteLLM-compatible gateway.
type GatewayConfig struct {
	Port           string
	VertexProject  string
	VertexLocation string
}

func LoadGatewayConfig() GatewayConfig {
	port := os.Getenv("VA_GATEWAY_PORT")
	if port == "" {
		port = "8081"
	}

	project := os.Getenv("VA_VERTEX_PROJECT")
	if project == "" {
		project = "elevate-data-508005"
	}

	location := os.Getenv("VA_VERTEX_LOCATION")
	if location == "" {
		location = "us-central1"
	}

	return GatewayConfig{
		Port:           port,
		VertexProject:  project,
		VertexLocation: location,
	}
}

// GetModel mapped name.
func GetModel() string {
	model := os.Getenv("VA_MODEL")
	if model == "" {
		model = "gemini-2.5-flash"
	}
	return model
}

func GetGatewayURL() string {
	url := os.Getenv("VA_GATEWAY_URL")
	if url == "" {
		url = "http://127.0.0.1:8081/v1"
	}
	return url
}
