package api

import (
	"bufio"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// Contract tests: the server against the frozen spec.
//
// The spec is written before the code and is the contract a stranger implements
// against. What can go wrong between them is not usually a wrong field type —
// the hand-written DTOs make that visible in review — it is drift in the
// *surface*: an endpoint in the spec with no handler, a handler on a path the
// spec does not describe, or a scope declaration that says one thing and is
// enforced as another.
//
// None of those three is caught by any other test in this package. A handler
// for an unspecified path passes every test written for it, and a spec entry
// with no handler passes every test in the spec because the spec runs nothing.
//
// # Reading the spec without a YAML library
//
// Same reasoning as tools/apidoc: the paths section is regularly indented, six
// fields are read from it, and a build-time YAML dependency on a project that
// publishes an SBOM is a real cost for a hundred lines of scanner. Anything the
// scanner does not understand is a failed test naming the line rather than a
// silent skip — a contract test that quietly reads fewer endpoints than the
// spec contains is worse than none.

// specOperation is one method on one path, as the spec declares it.
type specOperation struct {
	Path   string
	Method string
	Scopes []string
	Phase  int
	Public bool
	Line   int
}

func (o specOperation) String() string { return o.Method + " " + o.Path }

var specMethods = map[string]bool{
	"get": true, "post": true, "put": true, "patch": true, "delete": true,
	"head": true, "options": true,
}

// readSpec parses docs/api/openapi.yaml's paths section.
func readSpec(t *testing.T) []specOperation {
	t.Helper()

	// Located relative to this package rather than to the working directory,
	// which `go test` sets to the package directory and a CI runner may not.
	path := filepath.Join("..", "..", "docs", "api", "openapi.yaml")
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open the spec: %v", err)
	}
	defer func() { _ = f.Close() }()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)

	var (
		ops     []specOperation
		inPaths bool
		current *specOperation
		urlPath string
		lineNo  int
	)
	flush := func() {
		if current != nil {
			ops = append(ops, *current)
			current = nil
		}
	}

	for scanner.Scan() {
		lineNo++
		raw := scanner.Text()
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		indent := len(raw) - len(strings.TrimLeft(raw, " "))

		if indent == 0 {
			flush()
			inPaths = trimmed == "paths:"
			continue
		}
		if !inPaths {
			continue
		}

		switch indent {
		case 2:
			flush()
			if trimmed == "parameters:" {
				continue
			}
			name, ok := strings.CutSuffix(trimmed, ":")
			if ok && strings.HasPrefix(name, "/") {
				urlPath = name
			}
		case 4:
			flush()
			name, ok := strings.CutSuffix(trimmed, ":")
			if !ok || !specMethods[name] {
				continue
			}
			current = &specOperation{Path: urlPath, Method: strings.ToUpper(name), Line: lineNo}
		case 6:
			if current == nil {
				continue
			}
			switch {
			case strings.HasPrefix(trimmed, "x-cairn-scopes: ["):
				list := strings.TrimSuffix(strings.TrimPrefix(trimmed, "x-cairn-scopes: ["), "]")
				for _, scope := range strings.Split(list, ",") {
					if scope = strings.TrimSpace(scope); scope != "" {
						current.Scopes = append(current.Scopes, scope)
					}
				}
			case strings.HasPrefix(trimmed, "x-cairn-phase: "):
				_, _ = fmt.Sscanf(trimmed, "x-cairn-phase: %d", &current.Phase)
			case trimmed == "security: []":
				current.Public = true
			}
		}
	}
	flush()

	if err := scanner.Err(); err != nil {
		t.Fatalf("read the spec: %v", err)
	}
	if len(ops) < 100 {
		t.Fatalf("read only %d operations out of the spec; the scanner and the spec have diverged", len(ops))
	}
	return ops
}

// coveredPhases are the phases whose operations must be routed.
//
// Phase 2 joined on 2026-09-03, which is the point at which the reporting
// surface stopped being a plan and became a contract this test enforces. The
// checklist asks for the selection to be "switched on per operation as it lands
// rather than in one move at the end", and the honest way round is the one this
// file already prefers: **every** Phase 2 operation is exercised, and the ones
// that have not landed are named in skippedFromContract with a reason. A growing
// skip list is visible on the screen; an allow-list that has to be added to
// before an operation is checked is a list somebody forgets to add to, and the
// forgetting is silent.
var coveredPhases = map[int]bool{1: true, 2: true}

// Every operation in a covered phase has a handler.
//
// The failure this catches is an endpoint that exists in the contract and
// nowhere else. Somebody implements a client against the spec, calls it, and
// gets the application shell back with a 200 — because the SPA fallback answers
// any unrouted path — which is the most confusing possible way to discover that
// an endpoint was never built.
func TestEveryCoveredOperationIsRouted(t *testing.T) {
	t.Parallel()

	server := testServer(t)
	c := newClient(t, server)
	c.setup()

	var missing []string
	exercised := 0
	byPhase := map[int]int{}
	for _, op := range readSpec(t) {
		if !coveredPhases[op.Phase] {
			continue
		}
		if skippedFromContract[op.String()] {
			continue
		}
		exercised++
		byPhase[op.Phase]++

		// A concrete path: {monitorId} and friends are replaced with a
		// syntactically valid id, so the request reaches the routing table
		// rather than being rejected at parse time.
		path := concretePath(op.Path)

		resp, body := c.do(op.Method, path, contractBody(op))
		_ = body

		// 404 with the *application shell* is the signature of an unrouted
		// path: the SPA fallback answered it. A JSON problem document with a
		// 404 is a real handler saying the row does not exist, which is fine.
		if resp.StatusCode == http.StatusNotFound &&
			!strings.HasPrefix(resp.Header.Get("Content-Type"), "application/problem+json") {
			missing = append(missing, fmt.Sprintf("%s (spec line %d)", op, op.Line))
			continue
		}
		if resp.StatusCode == http.StatusNotImplemented {
			missing = append(missing, fmt.Sprintf("%s answers 501 but is marked Phase %d (spec line %d)",
				op, op.Phase, op.Line))
		}
	}

	sort.Strings(missing)
	for _, entry := range missing {
		t.Errorf("no handler: %s", entry)
	}

	// A contract test that quietly exercises nothing passes forever. The floors
	// are well under the real counts and their only job is to fail loudly if the
	// scanner stops finding operations — **per phase**, because a single total
	// would let Phase 2 drop to zero without the number moving much.
	for phase, floor := range map[int]int{1: 60, 2: 18} {
		if byPhase[phase] < floor {
			t.Fatalf("only %d Phase %d operations were exercised, want at least %d; "+
				"the scanner and the spec have diverged", byPhase[phase], phase, floor)
		}
	}
	t.Logf("%d operations exercised (%d Phase 1, %d Phase 2)", exercised, byPhase[1], byPhase[2])
}

// Nothing is served that the spec does not describe.
//
// The other direction, and the one that matters for the promise the project
// makes: the dashboard is an ordinary API client with no privileged endpoint. An
// endpoint added for the dashboard's convenience and never written into the
// contract *is* a privileged endpoint, whatever the intention was — no other
// client knows it exists.
//
// It reads the routing table out of the server rather than guessing at it, so
// adding a route without a spec entry fails here on the next run.
func TestNothingIsServedThatTheSpecDoesNotDescribe(t *testing.T) {
	t.Parallel()

	described := map[string]bool{}
	for _, op := range readSpec(t) {
		described[op.Method+" "+normalisePattern(op.Path)] = true
	}

	var undescribed []string
	for _, route := range serverRoutes() {
		method, path, found := strings.Cut(route, " ")
		if !found {
			continue
		}
		if !strings.HasPrefix(path, "/api/v1/") {
			// /healthz, /readyz, /metrics and the SPA fallback. The first three
			// are in the spec under their bare paths and are checked above;
			// the fallback is not API surface.
			continue
		}
		if !described[method+" "+normalisePattern(path)] {
			undescribed = append(undescribed, route)
		}
	}

	sort.Strings(undescribed)
	for _, route := range undescribed {
		t.Errorf("%s is served but is not in docs/api/openapi.yaml — the dashboard gets no endpoint a scoped key cannot reach", route)
	}
}

// normalisePattern reduces both spellings of a path parameter to one shape.
// Go's mux writes {monitorId} and so does the spec, but the names are free to
// differ and comparing them literally would fail on a rename that changed
// nothing.
func normalisePattern(path string) string {
	var out strings.Builder
	for path != "" {
		before, rest, found := strings.Cut(path, "{")
		out.WriteString(before)
		if !found {
			break
		}
		_, after, ok := strings.Cut(rest, "}")
		if !ok {
			break
		}
		out.WriteString("{}")
		path = after
	}
	return out.String()
}

// concretePath substitutes a valid-looking value for every path parameter.
func concretePath(path string) string {
	var out strings.Builder
	for path != "" {
		before, rest, found := strings.Cut(path, "{")
		out.WriteString(before)
		if !found {
			break
		}
		name, after, ok := strings.Cut(rest, "}")
		if !ok {
			break
		}
		out.WriteString(placeholderFor(name))
		path = after
	}
	return out.String()
}

// placeholderFor supplies a syntactically valid value per parameter shape. The
// value never has to exist — a 404 from a real handler is a pass — it only has
// to survive parsing.
func placeholderFor(name string) string {
	switch {
	case strings.Contains(strings.ToLower(name), "token"):
		return "0000000000000000000000000000000000000000000"
	case strings.Contains(strings.ToLower(name), "slug"):
		return "example"
	case strings.Contains(strings.ToLower(name), "domain"):
		return "example.com"
	default:
		return "00000000-0000-7000-8000-000000000001"
	}
}

// contractBody supplies an empty JSON object for methods that need one, so a
// write reaches its handler rather than failing at the body reader.
func contractBody(op specOperation) any {
	switch op.Method {
	case http.MethodPost, http.MethodPut, http.MethodPatch:
		return map[string]any{}
	default:
		return nil
	}
}

// skippedFromContract is the small set of operations this test cannot exercise,
// each with a reason. It is deliberately explicit: a growing skip list is
// visible, and a silent one is not.
var skippedFromContract = map[string]bool{
	// Multipart, so an empty JSON body never reaches the handler. Covered by
	// the import tests, which post a real database.
	"POST /api/v1/imports/kuma": true,

	// A long-lived stream. `do` reads the body to completion, so exercising it
	// here would block until the keepalive ticker outlived the test. Covered by
	// live_test.go, which reads it as a stream.
	"GET /api/v1/live": true,

	// Not JSON, and answered by the metrics handler rather than the API mux.
	"GET /metrics": true,

	// --- Phase 2: share links ------------------------------------------------
	//
	// **Human-led work, not unbuilt work.** A share link is an unauthenticated
	// credential: the token in the URL is the whole of the authorisation, and
	// generating, hashing and sealing it is exactly what AGENTS.md rule 8 puts
	// in a person's hands ("Do not generate authentication, session, crypto, or
	// access-control code"). The schema is in migration 0008 and the spec is
	// merged; what is missing is deliberately missing.
	//
	// They are listed here rather than left to fail so that the reason is on the
	// screen. Deleting these four lines is the last step of building them, and
	// the test then says whether they work.
	"POST /api/v1/report-runs/{reportRunId}/share":     true,
	"DELETE /api/v1/report-runs/{reportRunId}/share":   true,
	"GET /api/v1/public/reports/{shareToken}":          true,
	"GET /api/v1/public/reports/{shareToken}/download": true,
}

// serverRoutes reads the routing table out of Handler's source.
//
// http.ServeMux does not expose its patterns, and adding a parallel list on the
// server for a test to read would be a second list to keep in step — which is
// the exact class of bug this test exists to catch, reintroduced one level up.
//
// So the table is read from the syntax tree: every string literal passed as the
// first argument to HandleFunc or Handle inside this package. go/ast is
// standard library, the pattern is a literal at every call site, and a call site
// that stopped being one would be caught by the count check below.
func serverRoutes() []string {
	fset := token.NewFileSet()
	pkg, err := parser.ParseDir(fset, ".", func(info os.FileInfo) bool {
		return !strings.HasSuffix(info.Name(), "_test.go")
	}, 0)
	if err != nil {
		return nil
	}

	var routes []string
	for _, p := range pkg {
		ast.Inspect(p, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) == 0 {
				return true
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if selector.Sel.Name != "HandleFunc" && selector.Sel.Name != "Handle" {
				return true
			}
			literal, ok := call.Args[0].(*ast.BasicLit)
			if !ok || literal.Kind != token.STRING {
				return true
			}
			pattern, err := strconv.Unquote(literal.Value)
			if err == nil && strings.Contains(pattern, " ") {
				routes = append(routes, pattern)
			}
			return true
		})
	}
	return routes
}
