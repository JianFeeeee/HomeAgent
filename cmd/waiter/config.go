package main

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

type Connection struct {
	Name   string `yaml:"name"`
	Socket string `yaml:"socket,omitempty"`
	Remote string `yaml:"remote,omitempty"`
	APIKey string `yaml:"api_key,omitempty"`
}

type Config struct {
	Socket           string       `yaml:"socket"`
	Remote           string       `yaml:"remote"`
	APIKey           string       `yaml:"api_key"`
	Default          string       `yaml:"default"`
	Connections      []Connection `yaml:"connections,omitempty"`
	DeviceGateway    string       `yaml:"device_gateway,omitempty"`    // remotedevice 网关地址（如 127.0.0.1:9890）
	DeviceToken      string       `yaml:"device_token,omitempty"`      // 设备接入 token
	DeviceAuthorized bool         `yaml:"device_authorized,omitempty"` // 客户端本地授权（用户手动开启，服务端无法篡改）
	// DeviceCmdAllowlist 是设备桥**命令白名单**（可执行命令名的第一段）。
	//
	// 留空/缺省 ⇒ 用内置默认集（见 device.go 的 defaultCmdAllowlist）。
	// ★ 不是"追加"而是"替换"：写了就以它为准，避免"以为加了 find、
	//   结果还留着 python3 -c 任意执行"这类误判。
	//
	// 为什么需要它：白名单原本是源码里硬编码的正则（18 个命令），
	// 而 waiter.yaml 里没有任何键能改它 ⇒ find / grep / sed / sort / tr
	// 这些排查问题最常用的**只读**命令一律被拒，实测报错：
	//     device_ctl_cmdrun  device_id:waiter-fnnas  error: command not in whitelist
	DeviceCmdAllowlist []string `yaml:"device_cmd_allowlist,omitempty"`
}

func (c *Config) Active() *Connection {
	for i := range c.Connections {
		if c.Connections[i].Name == c.Default {
			return &c.Connections[i]
		}
	}
	return nil
}

func (c *Config) ApplyDefault() {
	conn := c.Active()
	if conn == nil {
		return
	}
	if c.Socket == "" && c.Remote == "" {
		c.Socket = conn.Socket
		c.Remote = conn.Remote
		c.APIKey = conn.APIKey
	}
}

func discoverConfig(configPath string) *Config {
	if configPath != "" {
		if cfg := readFile(configPath); cfg != nil {
			return cfg
		}
	}

	candidates := configCandidates()
	for _, p := range candidates {
		if cfg := readFile(p); cfg != nil {
			return cfg
		}
	}

	return &Config{}
}

func configCandidates() []string {
	var cands []string

	home, _ := os.UserHomeDir()
	if home != "" {
		cands = append(cands, filepath.Join(home, ".config", "homeagent", "waiter.yaml"))
	}
	cands = append(cands, "/root/.config/homeagent/waiter.yaml")

	cands = append(cands, filepath.Join(".", "waiter.yaml"))

	if exe, err := os.Executable(); err == nil {
		cands = append(cands, filepath.Join(filepath.Dir(exe), "waiter.yaml"))
	}

	return cands
}

func configPath() string {
	home, _ := os.UserHomeDir()
	if home == "" {
		return ""
	}
	return filepath.Join(home, ".config", "homeagent", "waiter.yaml")
}

func readFile(path string) *Config {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		fmt.Fprintf(os.Stderr, "warning: %s: %v\n", path, err)
		return nil
	}
	return &cfg
}

func (c *Config) Save() {
	p := configPath()
	if p == "" {
		return
	}
	os.MkdirAll(filepath.Dir(p), 0755)
	data, err := yaml.Marshal(c)
	if err != nil {
		return
	}
	os.WriteFile(p, data, 0644)
}

func (c *Config) MergeCLI(socket, remote, apiKey string) {
	if socket != "" {
		c.Socket = socket
	}
	if remote != "" {
		c.Remote = remote
	}
	if apiKey != "" {
		c.APIKey = apiKey
	}
}

func (c *Config) SaveConnection(name string) {
	conn := Connection{
		Name:   name,
		Socket: c.Socket,
		Remote: c.Remote,
		APIKey: c.APIKey,
	}
	for i, existing := range c.Connections {
		if existing.Name == name {
			c.Connections[i] = conn
			c.Default = name
			c.Save()
			return
		}
	}
	c.Connections = append(c.Connections, conn)
	c.Default = name
	c.Save()
}

func (c *Config) SwitchConnection(name string) bool {
	for _, conn := range c.Connections {
		if conn.Name == name {
			c.Socket = conn.Socket
			c.Remote = conn.Remote
			c.APIKey = conn.APIKey
			c.Default = name
			c.Save()
			return true
		}
	}
	return false
}

func (c *Config) DeleteConnection(name string) bool {
	for i, conn := range c.Connections {
		if conn.Name == name {
			c.Connections = append(c.Connections[:i], c.Connections[i+1:]...)
			if c.Default == name {
				c.Default = ""
			}
			c.Save()
			return true
		}
	}
	return false
}
