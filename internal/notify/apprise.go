package notify

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Apprise, the meta-provider: one binary reaching roughly ninety services this
// project will never implement natively, for the cost of an exec (§3.3).
//
// It is the only channel whose availability depends on the host rather than on
// the build, which is why it is reported as a capability. The alternative —
// offering the channel type unconditionally and failing on first use — is the
// dead-control pattern progressive disclosure exists to avoid.

// appriseTimeout bounds the subprocess. Apprise delivers to several
// destinations serially, so this is more generous than the HTTP providers get.
const appriseTimeout = 60 * time.Second

func lookupApprise() string {
	path, err := exec.LookPath("apprise")
	if err != nil {
		return ""
	}
	return path
}

func sendApprise(ctx context.Context, s *Sender, c conf, ev Event) (Receipt, error) {
	if s.apprisePath == "" {
		return Receipt{}, fmt.Errorf("apprise is not installed on this instance: install it (pipx install apprise) and restart, or use a native channel type")
	}

	urls := c.list("urls")
	if len(urls) == 0 {
		return Receipt{}, fmt.Errorf("no apprise urls configured")
	}

	title := Title(ev)
	if tmpl := c.str("title_template", ""); tmpl != "" {
		rendered, err := Render(tmpl, Context(ev), EscapeNone)
		if err != nil {
			return Receipt{}, err
		}
		title = rendered
	}
	body, err := message(c, "body_template", ev)
	if err != nil {
		return Receipt{}, err
	}

	// The URLs are written to a mode-0600 file rather than passed as arguments.
	// An Apprise URL embeds its own credentials, and an argument vector is
	// readable by every user on the host through ps.
	dir, err := os.MkdirTemp("", "cairn-apprise-")
	if err != nil {
		return Receipt{}, fmt.Errorf("stage apprise configuration: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()

	configPath := filepath.Join(dir, "apprise.conf")
	if err := os.WriteFile(configPath, []byte(strings.Join(urls, "\n")+"\n"), 0o600); err != nil {
		return Receipt{}, fmt.Errorf("stage apprise configuration: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, appriseTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, s.apprisePath,
		"--config", configPath,
		"--input-format", "text",
		"--title", title,
		"--body", body)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	receipt := Receipt{Payload: truncate(title+"\n\n"+body, maxRecordedPayload)}
	if err := cmd.Run(); err != nil {
		// Apprise reports which destination failed on stderr, and that is the
		// only part of this an operator can act on.
		detail := collapse(strings.TrimSpace(stderr.String()))
		if detail == "" {
			detail = collapse(strings.TrimSpace(stdout.String()))
		}
		if detail == "" {
			return receipt, err
		}
		return receipt, fmt.Errorf("%v: %s", err, truncate(detail, maxProviderError))
	}
	return receipt, nil
}
