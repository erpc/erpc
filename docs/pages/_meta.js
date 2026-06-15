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
	"-- Use cases": {
		type: "separator",
		title: "Use cases",
	},
	"use-cases": { title: "Use cases", display: "children" },
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
	"-- Reference": {
		type: "separator",
		title: "Reference",
	},
	reference: { title: "Reference", display: "children" },
	// Presets / examples are still accessible via direct URL (/presets/*)
	// but are not surfaced in the sidebar.
	presets: { display: "hidden" },
};
