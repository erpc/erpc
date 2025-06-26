module.exports = {
	"example": {
		title: "erpc.yaml/ts",
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
			{name: "Integrity", href: "/config/failsafe/integrity"},
			{name: "Consensus", href: "/config/failsafe/consensus"},
		],
	},
	database: {
		title: "Database",
	},
	auth: {
		title: "Auth",
		// children: [
		// 	{name: "Secret", href: "/config/auth#secret-strategy"},
		// 	{name: "IP / CIDR", href: "/config/auth#network-strategy"},
		// 	{name: "JWT Token", href: "/config/auth#jwt-strategy"},
		// 	{name: "Sign-in with Ethereum", href: "/config/auth#siwe-strategy"},
		// ],
	},
	"rate-limiters": {
		title: "Rate limiters",
	},
	matcher: {
		title: "Matcher syntax",
	},
};
