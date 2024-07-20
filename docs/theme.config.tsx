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
};
