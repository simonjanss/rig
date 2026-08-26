package cache

// ServeLocallyForTest makes a [RowCache] hold values with no channel behind it.
//
// It is compiled into the test binary and nowhere else, and it is a seam rather
// than a method on purpose. "Attached to nothing and holding anyway" is a real
// posture for a [Keyed] — one process, no replicas, staleness is the caller's
// problem, which is what [Keyed.ServeLocally] names — and it is not one for a
// row cache, which is why [RowCache.Serve] with a nil bus leaves it dead. A way
// to reach it on the type would be the arrangement the type exists to refuse,
// exported and documented.
//
// What it buys is a cache that holds without a Postgres connection to hold it,
// so the local half of [RowCache.Forget] can be asserted in `make test`. The
// other half — the publication, and its atomicity with the writing transaction
// — needs a real bus, and that is `internal/authtest`'s.
func ServeLocallyForTest[V any](c *RowCache[V]) {
	c.k.ServeLocally()
}
