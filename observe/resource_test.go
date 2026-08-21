package observe

import "testing"

// A version nobody stamped in is left off rather than sent empty. The generated
// Tracing() never fills it, so an attribute set unconditionally would put an
// empty service.version on every span most rig projects ever export.
func TestServiceResourceOmitsAnEmptyVersion(t *testing.T) {
	for _, tc := range []struct {
		name    string
		cfg     Config
		present bool
		want    string
	}{
		{"stamped", Config{ServiceName: "todo", ServiceVersion: "1.4.0"}, true, "1.4.0"},
		{"unset", Config{ServiceName: "todo"}, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var (
				version string
				present bool
			)
			for _, kv := range serviceResource(tc.cfg).Attributes() {
				if kv.Key == "service.version" {
					version, present = kv.Value.String(), true
				}
			}
			if present != tc.present {
				t.Errorf("service.version present is %v, want %v", present, tc.present)
			}
			if version != tc.want {
				t.Errorf("service.version is %q, want %q", version, tc.want)
			}
		})
	}
}
