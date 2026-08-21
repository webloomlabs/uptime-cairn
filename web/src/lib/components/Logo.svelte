<script lang="ts">
	/**
	 * The cairn mark, drawn inline.
	 *
	 * Inline rather than `<img src="/logo.svg">` for the same reason Icon.svelte
	 * is: an `<img>` cannot inherit a colour, so the mark would be frozen at the
	 * green it was exported with and would sit wrong against a light theme. Here
	 * the fill is `currentColor` and the caller decides, which also spares the
	 * shell a request that would land after first paint — a logo that pops in
	 * late is the thing every dashboard is judged by on load.
	 *
	 * The paths and their transforms are web/static/logo.svg unchanged; only the
	 * fill is different. Resolved, the three stones sit at x 2–302, 0–198 and
	 * 3–102 against a 302×156 viewBox, so the mark is left-aligned rather than
	 * centred and its box is exactly the artwork.
	 *
	 * Decorative in every current use: the wordmark is beside it and carries the
	 * name, so it is hidden from assistive technology rather than read out twice.
	 */
	let { size = 18 }: { size?: number } = $props();

	const ASPECT = 302 / 156;
</script>

<svg
	width={size * ASPECT}
	height={size}
	viewBox="0 0 302 156"
	fill="currentColor"
	xmlns="http://www.w3.org/2000/svg"
	aria-hidden="true"
	focusable="false"
	style="fill-rule:evenodd;clip-rule:evenodd"
>
	<g transform="matrix(1,0,0,1,-99,-181)">
		<g transform="matrix(0.568182,0,0,0.808511,11.227273,-63.212766)">
			<path
				d="M686,471.5C686,484.47 671.016,495 652.56,495L191.44,495C172.984,495 158,484.47 158,471.5C158,458.53 172.984,448 191.44,448L652.56,448C671.016,448 686,458.53 686,471.5Z"
			/>
		</g>
		<g transform="matrix(0.375,0,0,0.808511,39.75,-122.212766)">
			<path
				d="M686,471.5C686,484.47 663.297,495 635.333,495L208.667,495C180.703,495 158,484.47 158,471.5C158,458.53 180.703,448 208.667,448L635.333,448C663.297,448 686,458.53 686,471.5Z"
			/>
		</g>
		<g transform="matrix(0.1875,0,0,0.808511,72.375,-181.212766)">
			<path
				d="M686,471.5C686,484.47 640.594,495 584.667,495L259.333,495C203.406,495 158,484.47 158,471.5C158,458.53 203.406,448 259.333,448L584.667,448C640.594,448 686,458.53 686,471.5Z"
			/>
		</g>
	</g>
</svg>
