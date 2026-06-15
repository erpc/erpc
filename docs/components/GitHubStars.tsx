import React from "react";

const REPO = "erpc/erpc";
const CACHE_KEY = "erpc-gh-stars";
const CACHE_TTL_MS = 24 * 60 * 60 * 1000;

function formatCount(n: number): string {
	if (n >= 1000) return `${(n / 1000).toFixed(1).replace(/\.0$/, "")}k`;
	return String(n);
}

/**
 * Navbar GitHub link: octocat mark + live star count. The count is fetched
 * client-side from the GitHub API and cached in localStorage for a day;
 * on any failure it degrades to just the logo — unlike the shields.io
 * badge it replaces, it can never render an "invalid" error image.
 */
export function GitHubStars() {
	const [stars, setStars] = React.useState<string | null>(null);

	React.useEffect(() => {
		try {
			const cached = window.localStorage.getItem(CACHE_KEY);
			if (cached) {
				const { v, t } = JSON.parse(cached);
				if (typeof v === "string" && Date.now() - t < CACHE_TTL_MS) {
					setStars(v);
					return;
				}
			}
		} catch {
			/* ignore corrupt cache */
		}
		fetch(`https://api.github.com/repos/${REPO}`)
			.then((r) => (r.ok ? r.json() : null))
			.then((d) => {
				if (typeof d?.stargazers_count === "number") {
					const v = formatCount(d.stargazers_count);
					try {
						window.localStorage.setItem(
							CACHE_KEY,
							JSON.stringify({ v, t: Date.now() }),
						);
					} catch {
						/* storage full/blocked — still render */
					}
					setStars(v);
				}
			})
			.catch(() => {
				/* degrade to logo only */
			});
	}, []);

	return (
		<span
			style={{
				display: "inline-flex",
				alignItems: "center",
				gap: "0.45rem",
				fontSize: "0.85rem",
				fontWeight: 600,
			}}
			title="Star eRPC on GitHub"
		>
			<svg viewBox="0 0 16 16" width="22" height="22" fill="currentColor" aria-hidden="true">
				<path d="M8 0C3.58 0 0 3.58 0 8c0 3.54 2.29 6.53 5.47 7.59.4.07.55-.17.55-.38 0-.19-.01-.82-.01-1.49-2.01.37-2.53-.49-2.69-.94-.09-.23-.48-.94-.82-1.13-.28-.15-.68-.52-.01-.53.63-.01 1.08.58 1.23.82.72 1.21 1.87.87 2.33.66.07-.52.28-.87.51-1.07-1.78-.2-3.64-.89-3.64-3.95 0-.87.31-1.59.82-2.15-.08-.2-.36-1.02.08-2.12 0 0 .67-.21 2.2.82.64-.18 1.32-.27 2-.27s1.36.09 2 .27c1.53-1.04 2.2-.82 2.2-.82.44 1.1.16 1.92.08 2.12.51.56.82 1.27.82 2.15 0 3.07-1.87 3.75-3.65 3.95.29.25.54.73.54 1.48 0 1.07-.01 1.93-.01 2.2 0 .21.15.46.55.38A8.01 8.01 0 0 0 16 8c0-4.42-3.58-8-8-8z" />
			</svg>
			{stars !== null && (
				<span style={{ display: "inline-flex", alignItems: "center", gap: "0.2rem" }}>
					<svg viewBox="0 0 16 16" width="13" height="13" fill="currentColor" aria-hidden="true">
						<path d="M8 .25a.75.75 0 0 1 .673.418l1.882 3.815 4.21.612a.75.75 0 0 1 .416 1.279l-3.046 2.97.719 4.192a.75.75 0 0 1-1.088.791L8 12.347l-3.766 1.98a.75.75 0 0 1-1.088-.79l.72-4.194L.819 6.374a.75.75 0 0 1 .416-1.28l4.21-.611L7.327.668A.75.75 0 0 1 8 .25z" />
					</svg>
					{stars}
				</span>
			)}
		</span>
	);
}

export default GitHubStars;
