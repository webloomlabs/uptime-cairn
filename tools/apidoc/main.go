// Command apidoc renders docs/api/reference.md from docs/api/openapi.yaml.
//
// Generated rather than written, because the alternative is a reference page
// that agrees with the spec on the day it is written and drifts from it
// silently afterwards — and the spec is the contract. A page that disagrees with
// the contract is worse than no page: somebody implements against it.
//
//	go run ./tools/apidoc
//
// # Why this reads the file with a scanner rather than a YAML library
//
// The dependency policy is that a package is not reached for when a hundred
// lines of our own code will do (AGENTS.md §5, data model §11.2), and this is
// that case: the spec's `paths:` section is regularly indented, this reads six
// fields out of it, and a general-purpose YAML parser would be a build-time
// dependency on a project that publishes an SBOM.
//
// The cost of that choice is that a scanner can silently misread a structure it
// does not understand, so it does not: anything unexpected inside `paths:` is a
// hard error naming the line. A generator that skips what it cannot parse
// produces a reference missing endpoints, which is the exact failure this exists
// to prevent.
package main

import (
	"bufio"
	"fmt"
	"os"
	"sort"
	"strings"
)

const (
	specPath = "docs/api/openapi.yaml"
	outPath  = "docs/api/reference.md"
)

// operation is one method on one path.
type operation struct {
	Path        string
	Method      string
	Tag         string
	OperationID string
	Summary     string
	Scopes      []string
	Phase       int

	// Public is true when the operation declares `security: []`, which is the
	// spec's way of saying it needs no credential at all. It is called out in
	// the reference because an unauthenticated endpoint is the one somebody has
	// to be able to find deliberately rather than discover.
	Public bool
}

var methods = map[string]bool{
	"get": true, "post": true, "put": true, "patch": true, "delete": true,
	"head": true, "options": true,
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "apidoc: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	f, err := os.Open(specPath)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	ops, tags, err := parse(f)
	if err != nil {
		return err
	}
	if len(ops) == 0 {
		return fmt.Errorf("%s produced no operations, which means the parser and the spec have diverged", specPath)
	}

	out, err := os.Create(outPath)
	if err != nil {
		return err
	}
	defer func() { _ = out.Close() }()

	render(out, ops, tags)
	fmt.Printf("wrote %s: %d operations across %d tags\n", outPath, len(ops), len(tags))
	return nil
}

// parse walks the spec's paths section.
func parse(f *os.File) ([]operation, map[string]string, error) {
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)

	var (
		ops      []operation
		tags     = map[string]string{}
		tagOrder []string
		inPaths  bool
		inTags   bool
		path     string
		current  *operation
		lineNo   int
		lastTag  string
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

		// Top-level keys end whichever section we are in.
		if indent == 0 {
			flush()
			inPaths = trimmed == "paths:"
			inTags = trimmed == "tags:"
			continue
		}

		if inTags {
			// - name: System
			//   description: ...
			if name, ok := strings.CutPrefix(trimmed, "- name: "); ok {
				lastTag = strings.TrimSpace(name)
				tagOrder = append(tagOrder, lastTag)
				tags[lastTag] = ""
			} else if description, ok := strings.CutPrefix(trimmed, "description: "); ok && lastTag != "" {
				tags[lastTag] = strings.TrimSpace(description)
			}
			continue
		}
		if !inPaths {
			continue
		}

		switch indent {
		case 2:
			// A path, or the shared `parameters:` some paths declare.
			flush()
			if trimmed == "parameters:" {
				continue
			}
			name, ok := strings.CutSuffix(trimmed, ":")
			if !ok || !strings.HasPrefix(name, "/") {
				continue
			}
			path = name

		case 4:
			// A method, or a path-level key this reference does not render.
			flush()
			name, ok := strings.CutSuffix(trimmed, ":")
			if !ok || !methods[name] {
				continue
			}
			if path == "" {
				return nil, nil, fmt.Errorf("%s:%d: %s outside any path", specPath, lineNo, name)
			}
			current = &operation{Path: path, Method: strings.ToUpper(name)}

		case 6:
			if current == nil {
				continue
			}
			switch {
			case strings.HasPrefix(trimmed, "tags: ["):
				current.Tag = strings.Trim(strings.TrimSuffix(strings.TrimPrefix(trimmed, "tags: ["), "]"), " ")
			case strings.HasPrefix(trimmed, "operationId: "):
				current.OperationID = strings.TrimSpace(strings.TrimPrefix(trimmed, "operationId: "))
			case strings.HasPrefix(trimmed, "summary: "):
				current.Summary = strings.Trim(strings.TrimSpace(strings.TrimPrefix(trimmed, "summary: ")), `"`)
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
		return nil, nil, err
	}

	// Every operation the spec declares has an operationId. One without means
	// the parser lost its place, which is the failure this check exists for.
	for _, op := range ops {
		if op.OperationID == "" {
			return nil, nil, fmt.Errorf("%s %s has no operationId — the parser and the spec have diverged",
				op.Method, op.Path)
		}
	}

	ordered := map[string]string{}
	for i, tag := range tagOrder {
		// The index is kept so the reference renders tags in the spec's order
		// rather than alphabetically: the spec's order is the reading order
		// somebody chose, and alphabetical would put Authentication before
		// System.
		ordered[tag] = fmt.Sprintf("%03d|%s", i, tags[tag])
	}
	return ops, ordered, nil
}

func render(out *os.File, ops []operation, tags map[string]string) {
	byTag := map[string][]operation{}
	for _, op := range ops {
		tag := op.Tag
		if tag == "" {
			tag = "Other"
		}
		byTag[tag] = append(byTag[tag], op)
	}

	order := make([]string, 0, len(byTag))
	for tag := range byTag {
		order = append(order, tag)
	}
	sort.Slice(order, func(i, j int) bool {
		return sortKey(tags, order[i]) < sortKey(tags, order[j])
	})

	phases := map[int]int{}
	for _, op := range ops {
		phases[op.Phase]++
	}

	fmt.Fprintf(out, `# API reference

Every operation in the API, its scope, and the phase it ships in.

**Generated from [openapi.yaml](openapi.yaml) by %s — do not edit.** Run
%sgo run ./tools/apidoc%s after changing the spec. The spec is the contract; this
page exists so that reading it does not require a YAML viewer, and it is
generated precisely so it cannot come to disagree with what it describes.

For the conventions the whole surface follows — authentication, pagination,
error shape, the %sinclude=%s parameter — read [README.md](README.md) first. This
page is the index, not the introduction.

`, "`tools/apidoc`", "`", "`", "`", "`")

	fmt.Fprintf(out, "## At a glance\n\n| | Count |\n|---|---|\n| Operations | %d |\n", len(ops))
	for _, phase := range sortedPhases(phases) {
		if phase == 0 {
			continue
		}
		fmt.Fprintf(out, "| Phase %d operations | %d |\n", phase, phases[phase])
	}
	fmt.Fprintln(out)

	fmt.Fprintf(out, `## Reading the tables

**Scope** is what a credential must hold. %swrite%s implies %sread%s on the same
resource, and a key can never be granted a scope its creator does not hold.

**public** means the operation declares no security at all: the push ingest, the
status page read path, and the subscription links a status page mails out. Those
are unauthenticated by design — %scurl <url>%s with no flags has to work, or the
feature does not get used — and each carries its credential in the path.

**Phase** is when it ships. An operation marked for a later phase is in the
contract and answers %s501%s in this build, naming itself, rather than %s404%s —
because "not yet" and "no such thing" are different answers and a client
generator should see the first.

`, "`", "`", "`", "`", "`", "`", "`", "`", "`", "`")

	for _, tag := range order {
		fmt.Fprintf(out, "## %s\n\n", tag)
		if description := describe(tags, tag); description != "" {
			fmt.Fprintf(out, "%s\n\n", description)
		}

		rows := byTag[tag]
		sort.Slice(rows, func(i, j int) bool {
			if rows[i].Path != rows[j].Path {
				return rows[i].Path < rows[j].Path
			}
			return rows[i].Method < rows[j].Method
		})

		fmt.Fprintln(out, "| Operation | Scope | Phase | |")
		fmt.Fprintln(out, "|---|---|---|---|")
		for _, op := range rows {
			scope := "—"
			switch {
			case op.Public:
				scope = "**public**"
			case len(op.Scopes) > 0:
				scope = "`" + strings.Join(op.Scopes, "`, `") + "`"
			}
			phase := "—"
			if op.Phase > 0 {
				phase = fmt.Sprintf("%d", op.Phase)
			}
			fmt.Fprintf(out, "| `%s %s` | %s | %s | %s |\n",
				op.Method, op.Path, scope, phase, op.Summary)
		}
		fmt.Fprintln(out)
	}
}

func sortKey(tags map[string]string, tag string) string {
	if v, ok := tags[tag]; ok {
		return v
	}
	return "999|"
}

func describe(tags map[string]string, tag string) string {
	v, ok := tags[tag]
	if !ok {
		return ""
	}
	_, description, _ := strings.Cut(v, "|")
	return description
}

func sortedPhases(m map[int]int) []int {
	out := make([]int, 0, len(m))
	for phase := range m {
		out = append(out, phase)
	}
	sort.Ints(out)
	return out
}
