package domain

// ThinkingLevel represents reasoning/thinking levels supported across providers.
type ThinkingLevel string

const (
	ThinkingOff    ThinkingLevel = "off"
	ThinkingLow    ThinkingLevel = "low"
	ThinkingMedium ThinkingLevel = "medium"
	ThinkingHigh   ThinkingLevel = "high"
)

// ThinkingBudgets defines standardized token budgets for reasoning-enabled models.
var ThinkingBudgets = map[ThinkingLevel]int64{
	ThinkingLow:    1024,
	ThinkingMedium: 2048,
	ThinkingHigh:   4096,
}
