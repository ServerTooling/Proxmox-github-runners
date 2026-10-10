package main

import (
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
)

// cliOnly are commands with no button, and why.
var cliOnly = map[string]string{
	"setup":           "the page itself; the service page connects GitHub Apps the same way",
	"run":             "the service the page runs in",
	"version":         "shown in the status at the top of the page",
	"status":          "the top of the page",
	"config init":     "the service needs a config to start",
	"config validate": "every save validates",
}

// Everything the CLI can do should be doable from the settings page; a new command needs a button
// or a reason here.
func TestEveryCommandIsOnTheSettingsPage(t *testing.T) {
	buttons := map[string]bool{}
	var busy atomic.Bool
	for _, a := range webActions("/nonexistent/config.yaml", &busy) {
		if buttons[a.Name] {
			t.Errorf("two actions named %s", a.Name)
		}
		buttons[a.Name] = true
		if a.Run == nil || a.Label == "" {
			t.Errorf("action %s needs a label and a Run", a.Name)
		}
	}
	line := regexp.MustCompile(`(?m)^  pve-runners ([a-z]+)(?: ([a-z|]+))?`)
	for _, m := range line.FindAllStringSubmatch(usage, -1) {
		cmd, subs := m[1], strings.Split(m[2], "|")
		for _, sub := range subs {
			name := strings.TrimSpace(cmd + " " + sub)
			if cliOnly[name] != "" {
				continue
			}
			switch name {
			case "update":
				name = "update-check"
			case "config show":
				name = "config-show"
			}
			if id := strings.ReplaceAll(name, " ", "-"); !buttons[id] {
				t.Errorf("`pve-runners %s` has no button on the settings page", name)
			}
		}
	}
	if !buttons["update-now"] {
		t.Error("`pve-runners update --now` has no button on the settings page")
	}
}

// Image work and updates must not overlap the run loop's own image sync.
func TestImageButtonsWaitForTheRunLoopsSync(t *testing.T) {
	var busy atomic.Bool
	busy.Store(true)
	for _, a := range webActions("/nonexistent/config.yaml", &busy) {
		if a.Name != "images-pull" && a.Name != "images-prune" {
			continue
		}
		err := a.Run(t.Context(), &strings.Builder{}, nil)
		if err == nil || !strings.Contains(err.Error(), "already running") {
			t.Errorf("%s while a sync runs: %v", a.Name, err)
		}
	}
	if !busy.Load() {
		t.Fatal("a refused action released the run loop's guard")
	}
}
