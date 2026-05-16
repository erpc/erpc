module.exports = {
	"example": {
		title: "erpc.yaml/ts",
	},
	"server": {
		title: "Server",
	},
	"projects": {
		title: "Projects",
	},
	failsafe: {
		title: "Failsafe",
		children: [
			{name: "Timeout", href: "/config/failsafe/timeout"},
			{name: "Retry", href: "/config/failsafe/retry"},
			{name: "Hedge", href: "/config/failsafe/hedge"},
			{name: "Circuit breaker", href: "/config/failsafe/circuit-breaker"},
			{name: "Consensus", href: "/config/failsafe/consensus"},
			{name: "Integrity", href: "/config/failsafe/integrity"},
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
