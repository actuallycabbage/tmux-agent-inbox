package main

import (
	"os"

	"tmux-agent-inbox/internal/inbox"
)

func main() {
	os.Exit(inbox.Run(os.Args[1:]))
}
