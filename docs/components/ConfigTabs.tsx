import React from "react";
import { Tabs as RawTabs } from "nextra/components";
import { ConfigCode } from "./ConfigCode";

// Nextra's Tabs typings (from headlessui) confuse TS about JSX children
// inference in .tsx contexts even though the JSX itself is valid (and works
// in every existing .mdx page). Cast to a permissive type so we can keep
// using normal JSX child syntax below.
const Tabs = RawTabs as unknown as React.ComponentType<{
	items: string[];
	defaultIndex?: number;
	selectedIndex?: number;
	onChange?: (index: number) => void;
	storageKey?: string;
	children?: React.ReactNode;
}> & {
	Tab: React.ComponentType<{ children?: React.ReactNode }>;
};

export interface ConfigTabsProps {
	/** YAML code body. */
	yaml: string;
	/** TypeScript code body. */
	ts: string;
	/** Hierarchical breadcrumb; applied to both tabs unless overridden. */
	path?: string;
	/**
	 * Lines (1-indexed) to keep at full opacity. Applied to both tabs unless
	 * a tab-specific override is supplied.
	 */
	focus?: string;
	focusYaml?: string;
	focusTs?: string;
	filenameYaml?: string;
	filenameTs?: string;
	showLineNumbers?: boolean;
}

/**
 * Renders the canonical eRPC YAML / TypeScript config pair in a Nextra Tabs
 * group. The tab selection persists across the entire docs site via the
 * shared `GlobalConfigTypeTabIndex` storageKey (matches the existing pattern).
 */
export function ConfigTabs({
	yaml,
	ts,
	path,
	focus,
	focusYaml,
	focusTs,
	filenameYaml = "erpc.yaml",
	filenameTs = "erpc.ts",
	showLineNumbers,
}: ConfigTabsProps) {
	return (
		<Tabs
			items={["yaml", "typescript"]}
			defaultIndex={0}
			storageKey="GlobalConfigTypeTabIndex"
		>
			<Tabs.Tab>
				<ConfigCode
					language="yaml"
					filename={filenameYaml}
					path={path}
					focus={focusYaml ?? focus}
					showLineNumbers={showLineNumbers}
					code={yaml}
				/>
			</Tabs.Tab>
			<Tabs.Tab>
				<ConfigCode
					language="typescript"
					filename={filenameTs}
					path={path}
					focus={focusTs ?? focus}
					showLineNumbers={showLineNumbers}
					code={ts}
				/>
			</Tabs.Tab>
		</Tabs>
	);
}

export default ConfigTabs;
