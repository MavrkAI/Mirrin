package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/MavrkAI/Mirrin/internal/api"
	"github.com/MavrkAI/Mirrin/internal/config"
)

// `mirrin devices add` (and page, review) opens the running twin's page on
// this computer through the menu's kind of link; with no twin running it
// says so, and a list still works as before.
func TestDevicesOpensThePages(t *testing.T) {
	cfg, _, master := runningTwin(t, "127.0.0.1:0")
	var opened []string
	prev := openPage
	openPage = func(u string) error { opened = append(opened, u); return nil }
	t.Cleanup(func() { openPage = prev })
	var out strings.Builder
	for sub, path := range devicePages {
		opened = nil
		if err := devicesCmd(context.Background(), cfg, []string{sub}, &out); err != nil {
			t.Fatalf("%s: %v", sub, err)
		}
		want := "http://" + api.LoopbackAddr(cfg.API.Listen) + path + "?token=" + master
		if len(opened) != 1 || opened[0] != want {
			t.Fatalf("%s opened %v, want %s", sub, opened, want)
		}
	}
	openPage = func(string) error { return errors.New("no browser") }
	if err := devicesCmd(context.Background(), cfg, []string{"add"}, &out); err == nil || !strings.Contains(err.Error(), "menu") {
		t.Fatalf("no browser: %v", err)
	}

	stopped := config.Default()
	stopped.DataDir = t.TempDir()
	stopped.API.Listen = "127.0.0.1:1"
	if err := devicesCmd(context.Background(), stopped, []string{"add"}, &out); err == nil || !strings.Contains(err.Error(), "isn't running") {
		t.Fatalf("stopped: %v", err)
	}
}
