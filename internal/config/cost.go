package config

func GetTokenPricePer1K(model string, isInput bool) float64 {
	// Example pricing for gemini-2.5-flash
	if isInput {
		return 0.075
	}
	return 0.30
}
