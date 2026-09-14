package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestConfigValidateValidDefault(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := run([]string{"config", "validate"}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("expected valid default config, got error: %v", err)
	}
	if !strings.Contains(stdout.String(), "configuration valid") {
		t.Errorf("expected 'configuration valid' in stdout, got: %q", stdout.String())
	}
}

func TestConfigValidateValidCustomFlags(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := run([]string{
		"config", "validate",
		"-mode=solo",
		"-data-dir=/tmp/test-cairn",
		"-listen=:9090",
		"-instance-name=My Instance",
		"-base-url=https://status.example.com",
		"-trusted-proxy=10.0.0.1,192.168.1.0/24",
	}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("expected valid custom config, got error: %v", err)
	}
	if !strings.Contains(stdout.String(), "configuration valid") {
		t.Errorf("expected 'configuration valid' in stdout, got: %q", stdout.String())
	}
}

func TestConfigValidateInvalidFields(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantErrMsg string
	}{
		{
			name:       "empty listen address",
			args:       []string{"config", "validate", "-listen="},
			wantErrMsg: "--listen must not be empty",
		},
		{
			name:       "empty data dir",
			args:       []string{"config", "validate", "-data-dir="},
			wantErrMsg: "--data-dir must not be empty",
		},
		{
			name:       "unknown mode",
			args:       []string{"config", "validate", "-mode=invalid"},
			wantErrMsg: "unknown --mode \"invalid\"",
		},
		{
			name:       "probe mode (Phase 4 not built)",
			args:       []string{"config", "validate", "-mode=probe"},
			wantErrMsg: "--mode=probe is Phase 4 work",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			err := run(tt.args, &stdout, &stderr)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tt.wantErrMsg)
			}
			if !strings.Contains(err.Error(), tt.wantErrMsg) {
				t.Errorf("error = %q, want it to contain %q", err.Error(), tt.wantErrMsg)
			}
		})
	}
}

func TestConfigValidateUnexpectedArgument(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := run([]string{"config", "validate", "unexpected"}, &stdout, &stderr)
	if err == nil {
		t.Fatal("expected error for unexpected argument, got nil")
	}
	if !strings.Contains(err.Error(), "unexpected argument") {
		t.Errorf("error = %q, want it to mention unexpected argument", err.Error())
	}
}

func TestConfigUnknownSubcommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := run([]string{"config", "unknown"}, &stdout, &stderr)
	if err == nil {
		t.Fatal("expected error for unknown config subcommand, got nil")
	}
	if !strings.Contains(stderr.String(), "usage: cairn config validate") {
		t.Errorf("expected usage in stderr, got: %q", stderr.String())
	}
}

func TestConfigHelp(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := run([]string{"config", "validate", "-h"}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("expected -h to succeed, got: %v", err)
	}
	if !strings.Contains(stderr.String(), "usage: cairn config validate") {
		t.Errorf("expected usage in stderr, got: %q", stderr.String())
	}
}
