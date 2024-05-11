package health

var (
	ErrorCategoryGeneric       = 1 << 0
	ErrorCategoryCapacity      = 1 << 1
	ErrorCategoryAvailability  = 1 << 2
	ErrorCategoryBilling       = 1 << 3
	ErrorCategoryRecoverable   = 1 << 4
	ErrorCategoryUnrecoverable = 1 << 5
)
