// Package pagetest checks the pages internal/api serves: the presence screen
// (ui.html) and the settings pages (health, memory, channels, accounts,
// protocols). Colour contrast is checked statically on every run; behaviour is
// checked in a headless Chrome against a fake daemon when one is available.
//
// It lives in its own package so the tests never share names with the API's
// own tests. It has no code of its own.
package pagetest
