//go:build !nowhatsapp

package whatsapp

// Built reports whether this program has the WhatsApp channel. A build made
// with -tags nowhatsapp leaves it, and the GPL-3.0 Signal library it needs,
// out (docs/licensing.md).
const Built = true
