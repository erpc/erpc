module.exports = {
	"example": {
		title: "erpc.yaml",
	},
	"projects": {
		title: "Projects",
	},
	failsafe: {
		title: "Failsafe",
		children: [
			{name: "Circuit breaker", href: "/config/failsafe#circuitbreaker-policy"},
			{name: "Hedge", href: "/config/failsafe#hedge-policy"},
			{name: "Retry", href: "/config/failsafe#retry-policy"},
			{name: "Timeout", href: "/config/failsafe#timeout-policy"},
		],
	},
	// database: {
	// 	title: "Database",
	// },
	"rate-limiters": {
		title: "Rate limiters",
	},
	// "health-checks": {
	// 	title: "Health checks",
	// },
};
