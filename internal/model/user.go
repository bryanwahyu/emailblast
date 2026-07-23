package model

// User is one recipient plus the merge fields used for personalization.
type User struct {
	ID     string            // stable unique id, used as idempotency key
	Email  string            // recipient address
	Fields map[string]string // arbitrary merge data (first_name, plan, ...)
}

// Job is a unit of work flowing through the pipeline.
type Job struct {
	User    User
	Attempt int // 0 on first try, incremented on retry
}
