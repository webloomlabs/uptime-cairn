import { redirect } from '@sveltejs/kit';

/**
 * The monitor list lives at `/` — it is the product's home screen, and having it
 * at two addresses would mean two copies of the same page drifting apart.
 *
 * This redirect exists because `/monitors` was that address first: bookmarks and
 * anything already linking to it should keep working rather than land on a
 * not-found. 308, because the move is permanent.
 */
export function load() {
	redirect(308, '/');
}
