package util

import (
	"os"
)

// GetTokenFromEnv returns the provided tokenValue if non-empty, otherwise checks
// the GITHUB_TOKEN then GH_TOKEN environment variables and returns the first found.
func GetTokenFromEnv(tokenValue string) string {
	if tokenValue == "" {
		tokenValue = os.Getenv("GITHUB_TOKEN")
		if tokenValue == "" {
			tokenValue = os.Getenv("GH_TOKEN")
		}
	}
	return tokenValue
}
