// Command mirrin may import cloud; here it reaches it through the daemon.
package main

import "github.com/MavrkAI/Mirrin/internal/daemon"

func main() { _ = daemon.Linked() }
