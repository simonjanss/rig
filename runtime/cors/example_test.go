package cors_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"time"

	"github.com/simonjanss/rig/runtime/cors"
)

// A front end on https://app.example.com calling an API that is not served from
// there. The policy wraps the whole handler; rig's probes are answered outside
// whatever is wrapped.
func Example() {
	mux := http.NewServeMux()
	mux.HandleFunc("QUERY /api/v1/notes", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("RateLimit-Remaining", "99")
		fmt.Fprint(w, `[]`)
	})

	handler := cors.Policy{
		AllowedOrigins: []string{"https://app.example.com"},
		AllowedMethods: []string{"GET", "POST", "QUERY", "OPTIONS"},
		AllowedHeaders: []string{"Authorization", "Content-Type", "Idempotency-Key"},
		ExposedHeaders: []string{"RateLimit-Remaining", "Retry-After"},
		MaxAge:         10 * time.Minute,
	}.Wrap(mux)

	// The browser asks first. The mux has no OPTIONS route, so the policy
	// answers this itself.
	preflight := httptest.NewRequest(http.MethodOptions, "/api/v1/notes", nil)
	preflight.Header.Set("Origin", "https://app.example.com")
	preflight.Header.Set("Access-Control-Request-Method", "QUERY")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, preflight)
	fmt.Println(rec.Code, rec.Header().Get("Access-Control-Allow-Methods"))

	// Then it sends the request, and may read the headers the policy exposed.
	search := httptest.NewRequest("QUERY", "/api/v1/notes", nil)
	search.Header.Set("Origin", "https://app.example.com")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, search)
	fmt.Println(rec.Code, rec.Header().Get("Access-Control-Allow-Origin"), rec.Header().Get("Access-Control-Expose-Headers"))

	// A page from anywhere else gets the answer and no permission to read it.
	other := httptest.NewRequest("QUERY", "/api/v1/notes", nil)
	other.Header.Set("Origin", "https://evil.example.com")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, other)
	fmt.Printf("%d %q\n", rec.Code, rec.Header().Get("Access-Control-Allow-Origin"))

	// Output:
	// 204 GET, POST, QUERY, OPTIONS
	// 200 https://app.example.com RateLimit-Remaining, Retry-After
	// 200 ""
}
