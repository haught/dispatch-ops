import { goto } from '$app/navigation';
import { page } from '$app/state';

/** One tab of a page: its ID in the ?tab= query parameter, and its label. */
export interface Tab {
	id: string;
	label: string;
	/** A number shown beside the label, such as how many rows the tab lists. */
	count?: number;
}

/**
 * The ID of the tab the URL selects: its ?tab= parameter when that names one
 * of the tabs, and the first tab otherwise (a tab the viewer can't see, or
 * one that doesn't apply to this resource).
 */
export function activeTab(tabs: Tab[]): string {
	const want = page.url.searchParams.get('tab');
	return tabs.find((t) => t.id === want)?.id ?? tabs[0]?.id ?? '';
}

/** The link to a tab, keeping the URL's other query parameters. The first tab needs none. */
export function tabHref(tabs: Tab[], id: string): string {
	const url = new URL(page.url.href);
	if (id === tabs[0]?.id) url.searchParams.delete('tab');
	else url.searchParams.set('tab', id);
	return url.pathname + url.search;
}

/** Switches to a tab, for buttons elsewhere on the page that lead to one. */
export function selectTab(tabs: Tab[], id: string): Promise<void> {
	return goto(tabHref(tabs, id), { reset: false });
}
