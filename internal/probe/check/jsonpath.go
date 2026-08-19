package check

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// A deliberately small JSONPath: a root, named fields, and array indices. That
// is the whole of what the spec's own example ("$.status") needs, and what a
// health endpoint's response looks like.
//
// The alternative was a dependency implementing filter expressions, script
// expressions, recursive descent, and slices — a query language embedded in a
// monitor config, evaluated against a body a remote server controls. This
// version has no evaluation step to attack and no syntax an operator has to
// learn twice.
//
// Anything outside the subset is rejected at validation, not ignored at check
// time: an assertion that silently does not run is the failure mode this whole
// checker is built to avoid.

// pathSegment is one step: a field name or an array index.
type pathSegment struct {
	name  string
	index int
	isKey bool
}

// jsonPathAssertion is the parsed form of HttpConfig.json_path.
type jsonPathAssertion struct {
	segments []pathSegment
	operator string
	expected string
	rawPath  string
}

var jsonPathOperators = map[string]bool{
	"eq": true, "ne": true, "contains": true,
	"gt": true, "gte": true, "lt": true, "lte": true,
	"exists": true, "not_exists": true,
}

// needsExpected separates the operators that compare from the two that only ask
// whether anything is there.
func needsExpected(operator string) bool {
	return operator != "exists" && operator != "not_exists"
}

func parseJSONPathAssertion(raw json.RawMessage) (*jsonPathAssertion, error) {
	var spec struct {
		Path     string  `json:"path"`
		Operator string  `json:"operator"`
		Expected *string `json:"expected"`
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&spec); err != nil {
		return nil, fmt.Errorf("json_path: %w", err)
	}

	if !jsonPathOperators[spec.Operator] {
		return nil, fmt.Errorf("json_path operator %q: want eq, ne, contains, gt, gte, lt, lte, exists, or not_exists", spec.Operator)
	}
	segments, err := parseJSONPath(spec.Path)
	if err != nil {
		return nil, err
	}
	assertion := &jsonPathAssertion{segments: segments, operator: spec.Operator, rawPath: spec.Path}

	switch {
	case needsExpected(spec.Operator) && spec.Expected == nil:
		return nil, fmt.Errorf("json_path operator %q needs an expected value", spec.Operator)
	case spec.Expected != nil:
		assertion.expected = *spec.Expected
	}
	return assertion, nil
}

// parseJSONPath accepts $.a.b, $["a"], $['a'], $[0], and combinations.
func parseJSONPath(path string) ([]pathSegment, error) {
	if path == "" {
		return nil, errors.New("json_path path is required")
	}
	rest, found := strings.CutPrefix(path, "$")
	if !found {
		return nil, fmt.Errorf("json_path path %q must start with $", path)
	}

	var segments []pathSegment
	for rest != "" {
		switch {
		case strings.HasPrefix(rest, "."):
			rest = rest[1:]
			if strings.HasPrefix(rest, ".") {
				return nil, fmt.Errorf("json_path path %q: recursive descent (..) is not supported", path)
			}
			end := strings.IndexAny(rest, ".[")
			if end < 0 {
				end = len(rest)
			}
			name := rest[:end]
			if name == "" {
				return nil, fmt.Errorf("json_path path %q has an empty field name", path)
			}
			if strings.ContainsAny(name, "*?()@,:") {
				return nil, fmt.Errorf("json_path path %q: only plain field names and array indices are supported", path)
			}
			segments = append(segments, pathSegment{name: name, isKey: true})
			rest = rest[end:]

		case strings.HasPrefix(rest, "["):
			end := strings.Index(rest, "]")
			if end < 0 {
				return nil, fmt.Errorf("json_path path %q has an unclosed [", path)
			}
			inner := rest[1:end]
			rest = rest[end+1:]

			if len(inner) >= 2 && (inner[0] == '\'' || inner[0] == '"') && inner[len(inner)-1] == inner[0] {
				segments = append(segments, pathSegment{name: inner[1 : len(inner)-1], isKey: true})
				continue
			}
			index, err := strconv.Atoi(inner)
			if err != nil || index < 0 {
				return nil, fmt.Errorf("json_path path %q: [%s] must be a quoted field name or a non-negative index", path, inner)
			}
			segments = append(segments, pathSegment{index: index})

		default:
			return nil, fmt.Errorf("json_path path %q: expected . or [ at %q", path, rest)
		}
	}
	if len(segments) == 0 {
		return nil, fmt.Errorf("json_path path %q selects the whole document; give a field", path)
	}
	return segments, nil
}

// evaluate walks the path and returns the value it selects.
func (a *jsonPathAssertion) evaluate(body []byte) (value any, found bool, err error) {
	var document any
	if err := json.Unmarshal(body, &document); err != nil {
		return nil, false, fmt.Errorf("response body is not JSON: %w", err)
	}

	current := document
	for _, segment := range a.segments {
		if segment.isKey {
			object, ok := current.(map[string]any)
			if !ok {
				return nil, false, nil
			}
			current, ok = object[segment.name]
			if !ok {
				return nil, false, nil
			}
			continue
		}
		array, ok := current.([]any)
		if !ok || segment.index >= len(array) {
			return nil, false, nil
		}
		current = array[segment.index]
	}
	return current, true, nil
}

// assert returns an empty string when the assertion holds, and the failure
// message otherwise.
func (a *jsonPathAssertion) assert(body []byte) string {
	value, found, err := a.evaluate(body)
	if err != nil {
		return err.Error()
	}

	switch a.operator {
	case "exists":
		if !found {
			return fmt.Sprintf("%s is not present in the response body", a.rawPath)
		}
		return ""
	case "not_exists":
		if found {
			return fmt.Sprintf("%s is present in the response body but should not be", a.rawPath)
		}
		return ""
	}

	if !found {
		return fmt.Sprintf("%s is not present in the response body", a.rawPath)
	}
	got := renderJSONValue(value)

	switch a.operator {
	case "eq":
		if got != a.expected {
			return fmt.Sprintf("%s is %q, want %q", a.rawPath, got, a.expected)
		}
	case "ne":
		if got == a.expected {
			return fmt.Sprintf("%s is %q but should not be", a.rawPath, got)
		}
	case "contains":
		if !strings.Contains(got, a.expected) {
			return fmt.Sprintf("%s is %q, which does not contain %q", a.rawPath, got, a.expected)
		}
	default:
		return a.compare(got)
	}
	return ""
}

// compare handles the ordered operators. Numbers compare numerically and
// everything else lexicographically: the spec says values are compared as
// strings, but "9" > "10" would make gt useless on the response-code and
// queue-depth fields these assertions are written against.
func (a *jsonPathAssertion) compare(got string) string {
	gotNumber, gotErr := strconv.ParseFloat(got, 64)
	wantNumber, wantErr := strconv.ParseFloat(a.expected, 64)

	var ordering int
	if gotErr == nil && wantErr == nil {
		switch {
		case gotNumber < wantNumber:
			ordering = -1
		case gotNumber > wantNumber:
			ordering = 1
		}
	} else {
		ordering = strings.Compare(got, a.expected)
	}

	ok := false
	switch a.operator {
	case "gt":
		ok = ordering > 0
	case "gte":
		ok = ordering >= 0
	case "lt":
		ok = ordering < 0
	case "lte":
		ok = ordering <= 0
	}
	if ok {
		return ""
	}
	return fmt.Sprintf("%s is %q, which is not %s %q", a.rawPath, got, a.operator, a.expected)
}

// renderJSONValue turns a decoded value into the string the operators compare.
// Numbers lose the float formatting encoding/json gives them, because an
// operator writing `"expected": "200"` should not have to write "200.000000".
func renderJSONValue(value any) string {
	switch v := value.(type) {
	case nil:
		return "null"
	case string:
		return v
	case bool:
		return strconv.FormatBool(v)
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	default:
		encoded, err := json.Marshal(v)
		if err != nil {
			return fmt.Sprint(v)
		}
		return string(encoded)
	}
}
