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
			{name: "Circuit breaker", href: "/config/failsafe#circuitbreaker"},
			{name: "Hedge", href: "/config/failsafe#hedge"},
			{name: "Retry", href: "/config/failsafe#retry"},
			{name: "Timeout", href: "/config/failsafe#timeout"},
			{name: "Integrity", href: "/config/failsafe/integrity"},
			{name: "Empty/missing data", href: "/config/failsafe/integrity#empty-or-missing-data-handling"},
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
