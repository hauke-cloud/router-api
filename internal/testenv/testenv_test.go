package testenv

import (
	"strings"
	"testing"
)

func TestWithoutAssets(t *testing.T) {
	env := func(values map[string]string) func(string) string {
		return func(name string) string { return values[name] }
	}

	// The integration tests are most of the tests. Leaving them out has to
	// be something a person decides, not something that happens to whoever
	// runs `go test ./...` and sees "ok".
	code, message := withoutAssets(env(nil))
	if code == 0 {
		t.Error("a run without the control plane binaries reports success")
	}
	for _, want := range []string{"make test", SkipVariable} {
		if !strings.Contains(message, want) {
			t.Errorf("the message does not mention %q: %s", want, message)
		}
	}

	code, message = withoutAssets(env(map[string]string{SkipVariable: "1"}))
	if code != 0 {
		t.Errorf("code = %d although skipping was asked for", code)
	}
	if !strings.Contains(message, "skipping") {
		t.Errorf("message = %q", message)
	}
}
