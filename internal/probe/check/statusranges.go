package check

import (
	"fmt"
	"strconv"
	"strings"
)

// statusRanges is the parsed form of accepted_status_codes: "200-299", "301",
// or any mix. Parsed once at validation and once per check rather than
// re-parsed per assertion.
type statusRanges []statusRange

type statusRange struct{ lo, hi int }

// defaultStatusRange matches the OpenAPI default of ["200-299"]. It lives here
// so the default is stated once, in the code that applies it.
var defaultStatusRange = statusRanges{{lo: 200, hi: 299}}

func parseStatusRanges(spec []string) (statusRanges, error) {
	if len(spec) == 0 {
		return defaultStatusRange, nil
	}

	out := make(statusRanges, 0, len(spec))
	for _, entry := range spec {
		lo, hi, found := strings.Cut(entry, "-")
		low, err := parseCode(lo)
		if err != nil {
			return nil, fmt.Errorf("accepted_status_codes %q: %w", entry, err)
		}
		high := low
		if found {
			high, err = parseCode(hi)
			if err != nil {
				return nil, fmt.Errorf("accepted_status_codes %q: %w", entry, err)
			}
		}
		if high < low {
			return nil, fmt.Errorf("accepted_status_codes %q: range runs backwards", entry)
		}
		out = append(out, statusRange{lo: low, hi: high})
	}
	return out, nil
}

func parseCode(s string) (int, error) {
	code, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0, fmt.Errorf("%q is not a status code", s)
	}
	if code < 100 || code > 599 {
		return 0, fmt.Errorf("%d is outside 100-599", code)
	}
	return code, nil
}

func (r statusRanges) accepts(code int) bool {
	for _, rg := range r {
		if code >= rg.lo && code <= rg.hi {
			return true
		}
	}
	return false
}

// String renders the ranges back for an error message, so a failing check says
// "status 500 is not in 200-299" rather than naming a Go struct.
func (r statusRanges) String() string {
	parts := make([]string, 0, len(r))
	for _, rg := range r {
		if rg.lo == rg.hi {
			parts = append(parts, strconv.Itoa(rg.lo))
			continue
		}
		parts = append(parts, strconv.Itoa(rg.lo)+"-"+strconv.Itoa(rg.hi))
	}
	return strings.Join(parts, ", ")
}
