import { useRouter } from "next/router";
import {useConfig } from 'nextra-theme-docs'

export default {
	docsRepositoryBase: "https://github.com/erpc/erpc/tree/main/docs",
	logo: <b>eRPC</b>,
	project: {
		link: "https://github.com/erpc/erpc",
		icon: (
			<img
				src="https://img.shields.io/github/stars/erpc/erpc"
				alt="GitHub stars"
				width="100"  // Set both width and height to the same value
				height="100" // Match this to the width
				style={{ objectFit: 'contain' }} // Ensures the image fits within the dimensions
			/>
		)
	},
	chat: {
		link: "https://t.me/erpc_cloud",
		icon: (
			<svg width="35" height="35" viewBox="0 0 48 48">
				<circle cx="24" cy="24" r="20" fill="currentColor" />
				<path fill="gray" d="M33.95,15l-3.746,19.126c0,0-0.161,0.874-1.245,0.874c-0.576,0-0.873-0.274-0.873-0.274l-8.114-6.733 l-3.97-2.001l-5.095-1.355c0,0-0.907-0.262-0.907-1.012c0-0.625,0.933-0.923,0.933-0.923l21.316-8.468 c-0.001-0.001,0.651-0.235,1.126-0.234C33.667,14,34,14.125,34,14.5C34,14.75,33.95,15,33.95,15z"></path>
				<path fill="currentColor" d="M23,30.505l-3.426,3.374c0,0-0.149,0.115-0.348,0.12c-0.069,0.002-0.143-0.009-0.219-0.043 l0.964-5.965L23,30.505z"></path>
				<path fill="currentColor" d="M29.897,18.196c-0.169-0.22-0.481-0.26-0.701-0.093L16,26c0,0,2.106,5.892,2.427,6.912 c0.322,1.021,0.58,1.045,0.58,1.045l0.964-5.965l9.832-9.096C30.023,18.729,30.064,18.416,29.897,18.196z"></path>
			</svg>
		)
	},
	banner: {
		key: '2.0-release',
		text: (
			<a href="https://github.com/erpc/erpc" target="_blank">
				If you like eRPC, give it a star on GitHub ⭐️
			</a>
		)
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
	head: function useHead() {
		const config = useConfig()
		console.log('CONFIG', config)
		const { route } = useRouter()
		const isDefault = route === '/' || !config.title
		const image =
		  'https://erpc-test.up.railway.app' +
		  (isDefault ? `/og?title=eRPC` : `/og?title=${config.title}`)
	
		const description =
		  config.frontMatter.description ||
		  'open-source fault-tolerant evm rpc proxy and cache'
		const title = config.title + (route === '/' ? '' : ' - eRPC')
	
		return (
		  <>
			<title>{title}</title>
	 		<link rel="icon" href="./assets/favicon.ico" type="image/x-icon"></link>
			<meta property="og:title" content={title} />
			<meta name="description" content={description} />
			<meta property="og:description" content={description} />
			<meta property="og:image" content={image} />
	
			<meta name="msapplication-TileColor" content="#fff" />
			<meta httpEquiv="Content-Language" content="en" />
			<meta name="twitter:card" content="summary_large_image" />
			<meta name="twitter:site:domain" content="docs.erpc.cloud" />
			<meta name="twitter:url" content="https://docs.erpc.cloud" />
			<meta name="apple-mobile-web-app-title" content="eRPC" />
		  </>
		)
	  }
