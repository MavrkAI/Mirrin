package main

import (
	"fmt"
	"io"

	"github.com/MavrkAI/Mirrin/internal/api"
	"github.com/MavrkAI/Mirrin/internal/config"
	"github.com/MavrkAI/Mirrin/internal/daemon"
)

// devicePages maps `mirrin devices <word>` to the page it opens on this
// computer: "Add your phone", the Devices page, or the review after a
// restore.
var devicePages = map[string]string{
	"add":    "/devices/add",
	"page":   "/devices/page",
	"review": "/restore/review",
}

// openPage is how a page is opened in the browser; tests replace it.
var openPage = openURL

// devicesPage opens one of the running twin's device pages. The link
// carries the master key, as the menu's do; the page swaps it for the
// browser's own key at once.
func devicesPage(cfg *config.Config, sub string, out io.Writer) error {
	path := devicePages[sub]
	if cfg.API.Listen == "" || api.Connect(cfg.API.Listen, cfg.DataDir) == nil {
		if daemon.HomeInUse(cfg.DataDir) {
			return fmt.Errorf("%s is running but isn't answering on its local connection, so its pages can't open from here; open them from the menu bar instead", cfg.Name)
		}
		return fmt.Errorf("%s isn't running, and its pages come from it. Start it (open Mirrin, or `mirrin run`), then try again. To pair a terminal without it, use `mirrin pair`", cfg.Name)
	}
	tok, err := api.LoadOrCreateToken(cfg.DataDir)
	if err != nil {
		return err
	}
	u := "http://" + api.LoopbackAddr(cfg.API.Listen) + path + "?token=" + tok
	if err := openPage(u); err != nil {
		return fmt.Errorf("couldn't open a browser (%v); open it from %s's menu instead", err, cfg.Name)
	}
	fmt.Fprintf(out, "Opened %s in your browser.\n", map[string]string{"add": "Add your phone", "page": "Devices", "review": "the device review"}[sub])
	return nil
}
