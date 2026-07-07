package main

import (
	"fmt"
	"strings"
)

func handleBuiltin(cmd string, cfg *Config, state *State, reconnect func()) bool {
	switch {
	case cmd == "/help":
		fmt.Println(`Built-in commands:
  /help                    show this help
  /exit, /quit             exit waiter
  /clear                   clear screen
  /reconnect               force reconnection
  /connect <path>          switch to a different unix socket
  /remote <url>            switch to remote HTTP mode
  /local                   switch back to local socket mode

Server commands (sent to agent):
  /status                  system status
  /kernel                  kernel status
  /settings [prefix]       list settings
  /settings set <k> <v>    set a setting
  /plugin list             list installed plugins
  /plugin install <url>    install plugin
  /plugin remove <name>    remove plugin
  /plugin info <name>      plugin details
  /memory query <text>     query graph memory
  /knowledge               list knowledge base
  /agents                  list agents
  /chat <text>             send to agent

Any other text is sent to the agent directly.`)
		return true

	case cmd == "/exit" || cmd == "/quit":
		return true

	case cmd == "/clear":
		fmt.Print("\033[H\033[2J")
		return true

	case cmd == "/reconnect":
		printlnC(colorYellow, "reconnecting...")
		reconnect()
		return true

	case strings.HasPrefix(cmd, "/connect "):
		cfg.Socket = strings.TrimSpace(cmd[9:])
		cfg.Remote = ""
		reconnect()
		return true

	case strings.HasPrefix(cmd, "/remote "):
		cfg.Remote = strings.TrimSpace(cmd[8:])
		cfg.Socket = ""
		reconnect()
		return true

	case cmd == "/local":
		cfg.Remote = ""
		cfg.Socket = discoverSocket("")
		reconnect()
		return true

	default:
		return false
	}
}
