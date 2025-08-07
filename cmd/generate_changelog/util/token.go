package util

import (
	"os"
)

func GetTokenFromEnv(tokenValue string) string {
	if tokenValue == "" {
		tokenValue = os.Getenv("GITHUB_TOKEN")
		if tokenValue == "" {
			tokenValue = os.Getenv("GH_TOKEN")
		}
	}
	return tokenValue
}
