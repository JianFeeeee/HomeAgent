package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

func handleBuiltin(cmd string, cfg *Config, state *State, reconnect func(), out io.Writer) bool {
	switch {
	case cmd == "/help":
		fmt.Fprintln(out, `Built-in commands:
  /help                    show this help
  /exit, /quit             exit waiter
  /clear                   clear screen
  /reconnect               force reconnection
  /connect <path>          switch to a different unix socket
  /remote <url>            switch to remote HTTP mode
  /local                   switch back to local socket mode
  /conn list               list saved connections
  /conn save <name>        save current connection as <name>
  /conn use <name>         switch to saved connection
  /conn del <name>         delete saved connection

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
  /knowledge delete <name> delete knowledge item
  /agents                  list agents
  /chat <text>             send to agent

Any other text is sent to the agent directly.`)
		return true

	case cmd == "/exit" || cmd == "/quit":
		return true

	case cmd == "/clear":
		fmt.Fprint(out, "\033[H\033[2J")
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

	case cmd == "/conn list":
		if len(cfg.Connections) == 0 {
			fmt.Fprintln(out, "no saved connections")
		}
		for _, c := range cfg.Connections {
			mark := " "
			if c.Name == cfg.Default {
				mark = "*"
			}
			addr := c.Remote
			if addr == "" {
				addr = c.Socket
			}
			fmt.Fprintf(out, " %s %-15s %s\n", mark, c.Name, addr)
		}
		return true

	case strings.HasPrefix(cmd, "/conn save "):
		name := strings.TrimSpace(cmd[11:])
		cfg.SaveConnection(name)
		fmt.Fprintf(out, "connection saved as '%s' (default)\n", name)
		return true

	case strings.HasPrefix(cmd, "/conn use "):
		name := strings.TrimSpace(cmd[10:])
		if cfg.SwitchConnection(name) {
			fmt.Fprintf(out, "switched to '%s'\n", name)
			reconnect()
		} else {
			fmt.Fprintf(out, "connection '%s' not found\n", name)
		}
		return true

	case strings.HasPrefix(cmd, "/conn del "):
		name := strings.TrimSpace(cmd[10:])
		if cfg.DeleteConnection(name) {
			fmt.Fprintf(out, "connection '%s' deleted\n", name)
		} else {
			fmt.Fprintf(out, "connection '%s' not found\n", name)
		}
		return true

	case cmd == "/status":
		if rc := state.RemoteConn(); rc != nil {
			d, _ := rc.DoAPI("GET", "/api/v1/status", "")
			printJSON(out, d)
		} else {
			state.Send("/status")
		}
		return true

	case cmd == "/kernel":
		if rc := state.RemoteConn(); rc != nil {
			d, _ := rc.DoAPI("GET", "/api/v1/kernel", "")
			printJSON(out, d)
		} else {
			state.Send("/kernel")
		}
		return true

	case strings.HasPrefix(cmd, "/settings set "):
		parts := strings.SplitN(cmd[14:], " ", 2)
		if len(parts) < 2 {
			fmt.Fprintln(out, "usage: /settings set <key> <value>")
			return true
		}
		if rc := state.RemoteConn(); rc != nil {
			body := fmt.Sprintf(`{"%s":%q}`, parts[0], parts[1])
			rc.DoAPI("PUT", "/api/v1/settings", body)
			fmt.Fprintln(out, "ok")
		} else {
			state.Send(cmd[1:])
		}
		return true

	case strings.HasPrefix(cmd, "/settings"):
		if rc := state.RemoteConn(); rc != nil {
			d, _ := rc.DoAPI("GET", "/api/v1/settings", "")
			printJSON(out, d)
		} else {
			state.Send(cmd[1:])
		}
		return true

	case cmd == "/plugin list":
		if rc := state.RemoteConn(); rc != nil {
			d, _ := rc.DoAPI("GET", "/api/v1/plugins", "")
			printJSON(out, d)
		} else {
			state.Send("/plugin list")
		}
		return true

	case strings.HasPrefix(cmd, "/plugin install "):
		url := strings.TrimSpace(cmd[16:])
		if rc := state.RemoteConn(); rc != nil {
			body := fmt.Sprintf(`{"url":%q}`, url)
			d, _ := rc.DoAPI("POST", "/api/v1/plugins", body)
			printJSON(out, d)
		} else {
			state.Send(cmd[1:])
		}
		return true

	case strings.HasPrefix(cmd, "/plugin remove "):
		name := strings.TrimSpace(cmd[15:])
		if rc := state.RemoteConn(); rc != nil {
			d, _ := rc.DoAPI("DELETE", "/api/v1/plugins/"+name, "")
			printJSON(out, d)
		} else {
			state.Send(cmd[1:])
		}
		return true

	case strings.HasPrefix(cmd, "/plugin info "):
		name := strings.TrimSpace(cmd[13:])
		if rc := state.RemoteConn(); rc != nil {
			d, _ := rc.DoAPI("GET", "/api/v1/plugins/"+name, "")
			printJSON(out, d)
		} else {
			state.Send(cmd[1:])
		}
		return true

	case strings.HasPrefix(cmd, "/memory query "):
		q := strings.TrimSpace(cmd[14:])
		if rc := state.RemoteConn(); rc != nil {
			d, _ := rc.DoAPI("GET", "/api/v1/memory?query="+q, "")
			printJSON(out, d)
		} else {
			state.Send(cmd[1:])
		}
		return true

	case strings.HasPrefix(cmd, "/knowledge delete "):
		name := strings.TrimSpace(cmd[18:])
		if rc := state.RemoteConn(); rc != nil {
			d, _ := rc.DoAPI("DELETE", "/api/v1/knowledge/"+name, "")
			printJSON(out, d)
		} else {
			state.Send(cmd[1:])
		}
		return true

	case cmd == "/knowledge":
		if rc := state.RemoteConn(); rc != nil {
			d, _ := rc.DoAPI("GET", "/api/v1/knowledge", "")
			printJSON(out, d)
		} else {
			state.Send("/knowledge")
		}
		return true

	case cmd == "/agents":
		if rc := state.RemoteConn(); rc != nil {
			d, _ := rc.DoAPI("GET", "/api/v1/agents", "")
			printJSON(out, d)
		} else {
			state.Send("/agents")
		}
		return true

	default:
		return false
	}
}

func printJSON(out io.Writer, d map[string]interface{}) {
	if d == nil {
		fmt.Fprintln(out, "(no data)")
		return
	}
	b, _ := json.MarshalIndent(d, "", "  ")
	fmt.Fprintln(out, string(b))
}
