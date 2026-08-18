package rigclient

import (
	"context"
	"iter"
)

// Page is one page of a paginated read, as far as the iteration cares.
type Page[T any] struct {
	Items []T
	// Total is every row matching the query, ignoring pagination. It is what
	// says whether there is another page.
	Total int64
	// Offset is where this page started, as the server reported it.
	Offset int
}

// Paginate walks a paginated read to its end.
//
// fetch is handed the offset to ask for and returns one page; the limit is
// whatever the caller's query said, which fetch closes over. Iteration stops
// after the page that reaches the reported total, at the first failure, or when
// a page comes back empty — that last one is the guard against a server whose
// total disagrees with what it returns, which would otherwise be an infinite
// loop rather than a bug report.
//
// A failure is the second value of the final pair. There is no partial answer:
// what came before the failure was yielded, and nothing after it is.
func Paginate[T any](
	ctx context.Context, offset int, fetch func(context.Context, int) (Page[T], error),
) iter.Seq2[T, error] {
	return func(yield func(T, error) bool) {
		for {
			page, err := fetch(ctx, offset)
			if err != nil {
				var zero T
				yield(zero, err)
				return
			}
			for _, item := range page.Items {
				if !yield(item, nil) {
					return
				}
			}

			if len(page.Items) == 0 {
				return
			}
			offset += len(page.Items)
			if int64(offset) >= page.Total {
				return
			}
		}
	}
}
