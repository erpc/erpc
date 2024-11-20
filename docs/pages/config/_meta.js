module.exports = {
	"example": {
		title: "erpc.yaml/ts",
	},
	"projects": {
		title: "Projects",
	},
	auth: {
		title: "Auth",
		children: [
			{name: "Secret", href: "/config/auth#secret-strategy"},
			{name: "IP / CIDR", href: "/config/auth#network-strategy"},
			{name: "JWT Token", href: "/config/auth#jwt-strategy"},
			{name: "Sign-in with Ethereum", href: "/config/auth#siwe-strategy"},
		],
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
