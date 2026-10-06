//go:build darwin && cgo

// Package orb shows Mirrin's floating orb: a small transparent always-on-top
// panel in the corner of the screen, like Siri's, hosting the presence page
// in widget mode. macOS only; other platforms open the page in a browser.
package orb

/*
#cgo CFLAGS: -x objective-c -fobjc-arc
#cgo LDFLAGS: -framework Cocoa -framework WebKit
#include <stdlib.h>
void orb_show(const char *url, int w, int h, int margin);
void orb_preload(const char *url, int w, int h, int margin);
void orb_hide(void);
void orb_interactive(int on);
void orb_reload(void);
*/
import "C"

import "unsafe"

// Available reports native support.
func Available() bool { return true }

// Show creates (or re-shows) the orb panel loading url.
func Show(url string, w, h, margin int) {
	cu := C.CString(url)
	defer C.free(unsafe.Pointer(cu))
	C.orb_show(cu, C.int(w), C.int(h), C.int(margin))
}

// Preload creates the hidden panel and loads the page so Show is instant.
func Preload(url string, w, h, margin int) {
	cu := C.CString(url)
	defer C.free(unsafe.Pointer(cu))
	C.orb_preload(cu, C.int(w), C.int(h), C.int(margin))
}

// Hide orders the panel out.
func Hide() { C.orb_hide() }

// SetInteractive lets clicks reach the page (for Yes/No) or pass through to the desktop.
func SetInteractive(on bool) {
	v := 0
	if on {
		v = 1
	}
	C.orb_interactive(C.int(v))
}

// Reload refreshes the page.
func Reload() { C.orb_reload() }
