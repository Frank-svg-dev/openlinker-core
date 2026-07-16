package main

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestParseCommandRequiresExplicitCASAndCutoverIdentity(t *testing.T) {
	getenv := func(key string) string {
		values := map[string]string{
			"DATABASE_URL":          "postgres://openlinker:secret@postgres/openlinker",
			"OPENLINKER_RELEASE_ID": "release-1",
			"OPENLINKER_GIT_SHA":    "commit-1",
		}
		return values[key]
	}
	var stderr bytes.Buffer
	if _, err := parseCommand("drain", []string{"--expected-version=2", "--expected-replicas=2"}, &stderr, getenv); err != nil {
		t.Fatal(err)
	}
	if _, err := parseCommand("drain", []string{"--expected-replicas=2"}, &stderr, getenv); err == nil {
		t.Fatal("drain accepted a missing CAS version")
	}
	if _, err := parseCommand("reopen", []string{"--expected-version=3"}, &stderr, getenv); err == nil {
		t.Fatal("reopen accepted a missing cutover id")
	}
}

func TestRetireStaleMembersHasNoUnsafeMutationFlags(t *testing.T) {
	getenv := func(key string) string {
		if key == "DATABASE_URL" {
			return "postgres://openlinker:secret@postgres/openlinker"
		}
		return ""
	}
	var stderr bytes.Buffer
	if _, err := parseCommand("retire-stale-members", nil, &stderr, getenv); err != nil {
		t.Fatal(err)
	}
	cfg, err := parseCommand("retire-stale-members", []string{"--runtime-uninstalled-ok"}, &stderr, getenv)
	if err != nil || !cfg.allowRuntimeUninstalledNoop {
		t.Fatalf("cfg=%#v err=%v", cfg, err)
	}
	if _, err := parseCommand("retire-stale-members", []string{"--force"}, &stderr, getenv); err == nil {
		t.Fatal("retire-stale-members accepted an unsafe force flag")
	}
}

func TestRuntimeUninstalledMaintenanceNoopStillNeedsExplicitFlag(t *testing.T) {
	getenv := func(key string) string {
		if key == "DATABASE_URL" {
			return "postgres://openlinker:secret@postgres/openlinker"
		}
		return ""
	}
	var stderr bytes.Buffer
	cfg, err := parseCommand("hard-maintenance", []string{"--runtime-uninstalled-ok"}, &stderr, getenv)
	if err != nil || !cfg.allowRuntimeUninstalledNoop {
		t.Fatalf("cfg=%#v err=%v", cfg, err)
	}
	if _, err = parseCommand("hard-maintenance", nil, &stderr, getenv); err == nil {
		t.Fatal("hard-maintenance accepted neither CAS nor explicit runtime-uninstalled mode")
	}
}

func TestHelpAndUnknownCommandDoNotConnect(t *testing.T) {
	getenv := func(string) string { return "" }
	var stdout, stderr bytes.Buffer
	if code := run([]string{"help"}, &stdout, &stderr, getenv); code != exitOK || !strings.Contains(stdout.String(), "runtime-cutover") {
		t.Fatalf("help code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "retire-stale-members") {
		t.Fatalf("help omitted retire-stale-members: %q", stdout.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"unknown"}, &stdout, &stderr, getenv); code != exitUsage || !strings.Contains(stderr.String(), "usage:") {
		t.Fatalf("unknown code=%d stderr=%q", code, stderr.String())
	}
}

func TestCommandTimeoutIsPositiveAndConfigurable(t *testing.T) {
	getenv := func(key string) string {
		if key == "DATABASE_URL" {
			return "postgres://openlinker:secret@postgres/openlinker"
		}
		if key == "OPENLINKER_CUTOVER_COMMAND_TIMEOUT" {
			return "45s"
		}
		return ""
	}
	var stderr bytes.Buffer
	cfg, err := parseCommand("status", nil, &stderr, getenv)
	if err != nil || cfg.commandTimeout != 45*time.Second {
		t.Fatalf("cfg=%#v err=%v", cfg, err)
	}
	if _, err = parseCommand("status", []string{"--timeout=0s"}, &stderr, getenv); err == nil {
		t.Fatal("zero timeout was accepted")
	}
}
