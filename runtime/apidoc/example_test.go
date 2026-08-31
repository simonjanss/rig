package apidoc_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing/fstest"

	"github.com/simonjanss/rig/runtime/apidoc"
)

// What a project writes is the go:embed line; the paths come from the generated
// router, which took them from the compiled document. Here the filesystem is a
// fake so the example can run.
func Example() {
	// In a real project:
	//
	//	//go:embed docs/openapi.gen.json docs/openapi.gen.yaml
	//	var apidocs embed.FS
	apidocs := fstest.MapFS{
		"docs/openapi.gen.json": {Data: []byte(`{"openapi":"3.1.0"}`)},
	}

	docs, err := apidoc.New(apidocs, apidoc.Options{
		JSONPath: "/api/v1/openapi.json",
		YAMLPath: "/api/v1/openapi.yaml",
	})
	if err != nil {
		panic(err)
	}

	mux := http.NewServeMux()
	docs.Mount(mux)

	// Only the JSON rendering was embedded, so only its route exists. A request
	// for the other one is a 404 rather than an empty document.
	fmt.Println(docs.Paths())

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/openapi.json", nil))
	fmt.Println(rec.Code, rec.Header().Get("Content-Type"))
	fmt.Println(rec.Body.String())

	// Output:
	// [/api/v1/openapi.json]
	// 200 application/json
	// {"openapi":"3.1.0"}
}
