import adapter from '@sveltejs/adapter-static';
import { sveltekit } from '@sveltejs/kit/vite';
import { defineConfig, loadEnv } from 'vite';

// The UI is a single-page app embedded in the Go binary (see web.go). In
// development, `npm run dev` proxies API calls to the Go server at
// DISPATCH_API_URL (default https://localhost:8443). The server's certificate
// is self-signed by default, so the proxy doesn't verify it.
export default defineConfig(({ mode }) => {
	const env = loadEnv(mode, '.', 'DISPATCH_');
	const apiTarget = env.DISPATCH_API_URL || 'https://localhost:8443';

	return {
		plugins: [
			sveltekit({
				compilerOptions: {
					// Force runes mode for the project, except for libraries. Can be removed in svelte 6.
					runes: ({ filename }) =>
						filename.split(/[/\\]/).includes('node_modules') ? undefined : true
				},
				// SPA mode: every route is served by the fallback page (see web.go).
				adapter: adapter({ fallback: '200.html' })
			})
		],
		server: {
			proxy: {
				'/api': { target: apiTarget, secure: false },
				'/healthz': { target: apiTarget, secure: false },
				'/readyz': { target: apiTarget, secure: false }
			}
		}
	};
});
