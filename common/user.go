package common

// User 表示系统中的用户，包含 ID 和速率限制预算信息。
type User struct {
	Id              string
	RateLimitBudget string
}
