/** One page of a paginated read, as far as the iteration cares. */
export type Page<T> = {
    items: T[];
    /**
     * Every row matching the query, ignoring pagination. It is what says whether
     * there is another page.
     */
    total: number;
    /** Where this page started, as the server reported it. */
    offset: number;
};

/**
 * Walks a paginated read to its end.
 *
 * `fetch` is handed the offset to ask for and returns one page; the limit is
 * whatever the caller's query said, which `fetch` closes over. Iteration stops
 * after the page that reaches the reported total, at the first failure, or when
 * a page comes back empty — that last one is the guard against a server whose
 * total disagrees with what it returns, which would otherwise be an infinite
 * loop rather than a bug report.
 *
 * A failure is thrown, so `for await` reports it where the caller is standing.
 * There is no partial answer: what came before the failure was yielded, and
 * nothing after it is.
 *
 * ```ts
 * for await (const todo of paginate(0, (offset) =>
 *     client.todos.list({ limit: 100, offset }).then(toPage)
 * )) {
 *     …
 * }
 * ```
 */
export async function* paginate<T>(
    startOffset: number,
    fetch: (offset: number) => Promise<Page<T>>,
): AsyncGenerator<T, void, undefined> {
    let offset = startOffset;

    for (;;) {
        const page = await fetch(offset);
        for (const item of page.items) yield item;

        if (page.items.length === 0) return;
        offset += page.items.length;
        if (offset >= page.total) return;
    }
}
