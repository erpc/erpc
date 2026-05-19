module.exports = {
	index: {
		title: "Quick start",
		theme: {
			// Hide the right-hand TOC sidebar on the home page so the hero
			// diagram gets the full content width.
			toc: false,
			layout: "full",
			breadcrumb: false,
			pagination: false,
		},
	},
	why: { title: "Why eRPC?" },
	free: { title: "Free & Public RPCs" },
	faq: { title: "FAQ" },
	"-- Config": {
		type: "separator",
		title: "Config",
	},
	config: { display: "children", title: "Config" },
	"-- Deployment": {
		type: "separator",
		title: "Deployment",
	},
	deployment: { title: "Deployment", display: "children" },
	"-- Operations": {
		type: "separator",
		title: "Operations",
	},
	operation: { title: "Operation", display: "children" },
	// Presets / examples are still accessible via direct URL (/presets/*)
	// but are not surfaced in the sidebar. Add `display: "hidden"` instead
	// of omitting the keys entirely so Nextra doesn't auto-include them.
	presets: { display: "hidden" },
	"preview-retry": { display: "hidden" },
};
