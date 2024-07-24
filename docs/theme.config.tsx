import { useRouter } from "next/router";

export default {
	docsRepositoryBase: "https://github.com/erpc/erpc/tree/main/docs",
	logo: <b>eRPC</b>,
	project: {
		link: "https://github.com/erpc/erpc",
	},
	chat: {
	  link: "https://t.me/erpc_cloud",
	},
	sidebar: {
	  defaultMenuCollapseLevel: 2,
	},
	toc: {
	  backToTop: true,
	},
	feedback: {
	  content: null,
	},
	navigation: {
	  prev: true,
	  next: true,
	},
	darkMode: true,
	nextThemes: {
	  defaultTheme: "dark",
	},
	useNextSeoProps() {
		const { asPath } = useRouter();
		if (asPath !== "/") {
			return {
				titleTemplate: "%s – eRPC",
			};
		}
	},
	head: (
		<>
		   <title>eRPC - EVM RPC Proxy & Cache Service</title>
			<meta
			name="description"
			content="Fault-tolerant EVM RPC load balancer with reorg-aware permanent caching and auto-discovery of node providers."
			/>
			<meta
			property="og:title"
			content="eRPC - EVM RPC Proxy & Cache Service"
			/>
			<meta
			property="og:description"
			content="Fault-tolerant EVM RPC load balancer with reorg-aware permanent caching and auto-discovery of node providers."
			/>
			<meta
			property="og:image"
			content="https://i.imgur.com/Nq4yoEP.png"
			/>
			<meta
			property="og:url"
			content="https://erpc.cloud"
			/>
			<meta
			name="twitter:card"
			content="summary_large_image"
			/>
			<meta
			name="twitter:title"
			content="eRPC - EVM RPC Proxy & Cache Service"
			/>
			<meta
			name="twitter:description"
			content="Fault-tolerant EVM RPC load balancer with reorg-aware permanent caching and auto-discovery of node providers."
			/>
			<meta
			name="twitter:image"
			content="https://i.imgur.com/Nq4yoEP.png"
			/>
			<script defer data-domain="erpc.cloud" src="https://plausible.io/js/script.js"></script>
		</>
	  )
};
