package check

import "testing"

// The JSONPath subset is the one piece of this package with no network in it and
// the most ways to be quietly wrong: a path that fails to parse is caught at
// validation, but a path that parses and selects the wrong thing produces a
// monitor that reports up while asserting nothing.

func TestParseJSONPath(t *testing.T) {
	t.Parallel()

	accepted := map[string][]pathSegment{
		"$.status":        {{name: "status", isKey: true}},
		"$.a.b.c":         {{name: "a", isKey: true}, {name: "b", isKey: true}, {name: "c", isKey: true}},
		"$.items[0]":      {{name: "items", isKey: true}, {index: 0}},
		"$[0]":            {{index: 0}},
		`$["a b"]`:        {{name: "a b", isKey: true}},
		`$['a b'].c`:      {{name: "a b", isKey: true}, {name: "c", isKey: true}},
		"$.a[2].b":        {{name: "a", isKey: true}, {index: 2}, {name: "b", isKey: true}},
		"$.results[0][1]": {{name: "results", isKey: true}, {index: 0}, {index: 1}},
	}
	for path, want := range accepted {
		got, err := parseJSONPath(path)
		if err != nil {
			t.Errorf("parseJSONPath(%q): %v", path, err)
			continue
		}
		if len(got) != len(want) {
			t.Errorf("parseJSONPath(%q) = %d segments, want %d", path, len(got), len(want))
			continue
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("parseJSONPath(%q)[%d] = %+v, want %+v", path, i, got[i], want[i])
			}
		}
	}

	// Every one of these has to be an error at validation rather than a silent
	// no-op at check time: the whole point of rejecting json_path in the
	// previous build was that an assertion which does not run reports up.
	rejected := []string{
		"",
		"status",      // no root
		"$",           // selects the whole document
		"$..name",     // recursive descent
		"$.items[*]",  // wildcard
		"$.items[",    // unclosed
		"$.items[-1]", // negative index
		"$.items[a]",  // neither quoted name nor index
		"$.a..b",      // recursive descent mid-path
		"$.a[?(@.x)]", // filter expression
	}
	for _, path := range rejected {
		if _, err := parseJSONPath(path); err == nil {
			t.Errorf("parseJSONPath(%q) was accepted; want an error", path)
		}
	}
}

func TestJSONPathAssertions(t *testing.T) {
	t.Parallel()

	body := []byte(`{
		"status": "ok",
		"code": 200,
		"healthy": true,
		"missing": null,
		"queue": {"depth": 9},
		"nodes": [{"name": "a"}, {"name": "b"}],
		"empty": []
	}`)

	tests := []struct {
		name     string
		path     string
		operator string
		expected string
		wantPass bool
	}{
		{"string eq", "$.status", "eq", "ok", true},
		{"string eq mismatch", "$.status", "eq", "degraded", false},
		{"number renders without float noise", "$.code", "eq", "200", true},
		{"bool renders as a word", "$.healthy", "eq", "true", true},
		{"null renders as null", "$.missing", "eq", "null", true},
		{"ne", "$.status", "ne", "degraded", true},
		{"contains", "$.status", "contains", "o", true},
		{"nested field", "$.queue.depth", "eq", "9", true},
		{"array index into object", "$.nodes[1].name", "eq", "b", true},
		{"exists", "$.queue", "exists", "", true},
		{"exists on a missing field", "$.nope", "exists", "", false},
		{"not_exists", "$.nope", "not_exists", "", true},
		{"not_exists on a present field", "$.status", "not_exists", "", false},

		// The reason gt is not a string comparison: "9" > "10" lexicographically,
		// and a queue-depth assertion written that way would never fire.
		{"gt compares numerically", "$.queue.depth", "gt", "10", false},
		{"lt compares numerically", "$.queue.depth", "lt", "10", true},
		{"gte at the boundary", "$.code", "gte", "200", true},
		{"lte at the boundary", "$.code", "lte", "200", true},
		{"gt falls back to lexical for non-numbers", "$.status", "gt", "abc", true},

		// A path past the end of an array is absent, not an error: the response
		// simply did not carry what the assertion asked about.
		{"index past the end is absent", "$.empty[0]", "exists", "", false},
		{"field on a non-object is absent", "$.status.nope", "exists", "", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			assertion := &jsonPathAssertion{operator: tc.operator, expected: tc.expected, rawPath: tc.path}
			segments, err := parseJSONPath(tc.path)
			if err != nil {
				t.Fatalf("parseJSONPath: %v", err)
			}
			assertion.segments = segments

			msg := assertion.assert(body)
			if tc.wantPass && msg != "" {
				t.Errorf("assertion failed but should have passed: %s", msg)
			}
			if !tc.wantPass && msg == "" {
				t.Error("assertion passed but should have failed")
			}
		})
	}
}

func TestJSONPathNonJSONBody(t *testing.T) {
	t.Parallel()

	assertion := &jsonPathAssertion{
		segments: []pathSegment{{name: "status", isKey: true}},
		operator: "eq",
		expected: "ok",
		rawPath:  "$.status",
	}
	// An HTML error page where JSON was expected must fail the assertion, not
	// pass it: the server answered, and the answer was wrong.
	if msg := assertion.assert([]byte("<html>502 Bad Gateway</html>")); msg == "" {
		t.Error("a non-JSON body passed a JSON-path assertion")
	}
}

func TestParseJSONPathAssertionRequiresExpected(t *testing.T) {
	t.Parallel()

	if _, err := parseJSONPathAssertion([]byte(`{"path":"$.a","operator":"eq"}`)); err == nil {
		t.Error("eq without an expected value was accepted")
	}
	if _, err := parseJSONPathAssertion([]byte(`{"path":"$.a","operator":"exists"}`)); err != nil {
		t.Errorf("exists without an expected value was rejected: %v", err)
	}
	if _, err := parseJSONPathAssertion([]byte(`{"path":"$.a","operator":"matches","expected":"x"}`)); err == nil {
		t.Error("an unknown operator was accepted")
	}
}
