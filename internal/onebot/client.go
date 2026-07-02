package onebot

import (
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// EventHandler 处理 OneBot 推送的事件
type EventHandler func(event *Event)

// Client 是 OneBot 反向 WebSocket 客户端
// 连接到 OneBot 兼容前端（如 go-cqhttp、Lagrange）的 WS 地址
type Client struct {
	url        string
	accessToken string
	conn       *websocket.Conn
	mu         sync.Mutex
	done       chan struct{}

	eventHandler EventHandler

	// 等待响应的 Action 调用
	pending   sync.Map
	echoCount int64

	connected atomic.Bool
}

// NewClient 创建 OneBot 反向 WS 客户端
// wsURL: ws://host:port/onebot/v11/ws
func NewClient(wsURL, accessToken string) *Client {
	return &Client{
		url:         wsURL,
		accessToken: accessToken,
		done:        make(chan struct{}),
	}
}

// SetEventHandler 注册事件处理函数
func (c *Client) SetEventHandler(h EventHandler) {
	c.eventHandler = h
}

// Connected 返回是否已连接
func (c *Client) Connected() bool {
	return c.connected.Load()
}

// Connect 连接到 OneBot 前端（阻塞直到连接建立或失败）
func (c *Client) Connect() error {
	header := make(map[string][]string)
	if c.accessToken != "" {
		header["Authorization"] = []string{"Bearer " + c.accessToken}
	}

	conn, _, err := websocket.DefaultDialer.Dial(c.url, header)
	if err != nil {
		return err
	}

	c.mu.Lock()
	if c.conn != nil {
		c.conn.Close()
	}
	c.conn = conn
	c.mu.Unlock()

	c.connected.Store(true)
	log.Printf("[onebot] connected to %s", c.url)

	go c.readLoop()
	return nil
}

// reconnect 自动重连
func (c *Client) reconnect() {
	c.connected.Store(false)
	backoff := time.Second

	for {
		select {
		case <-c.done:
			return
		case <-time.After(backoff):
			log.Printf("[onebot] reconnecting in %v...", backoff)
			if err := c.Connect(); err != nil {
				log.Printf("[onebot] reconnect failed: %v, retry", err)
				backoff *= 2
				if backoff > 30*time.Second {
					backoff = 30 * time.Second
				}
				continue
			}
			log.Printf("[onebot] reconnected")
			return
		}
	}
}

func (c *Client) readLoop() {
	defer c.connected.Store(false)

	for {
		c.mu.Lock()
		conn := c.conn
		c.mu.Unlock()

		if conn == nil {
			log.Printf("[onebot] read loop: not connected")
			go c.reconnect()
			return
		}

		_, message, err := conn.ReadMessage()
		if err != nil {
			log.Printf("[onebot] read error: %v", err)
			go c.reconnect()
			return
		}

		// 尝试解析为 ActionResponse（有 echo 字段）
		var resp ActionResponse
		if err := json.Unmarshal(message, &resp); err == nil && resp.Echo != "" {
			if ch, ok := c.pending.Load(resp.Echo); ok {
				select {
				case ch.(chan *ActionResponse) <- &resp:
				default:
				}
			}
			continue
		}

		// 解析为 Event
		var evt Event
		if err := json.Unmarshal(message, &evt); err != nil {
			log.Printf("[onebot] parse error: %v", err)
			continue
		}

		if c.eventHandler != nil {
			c.eventHandler(&evt)
		}
	}
}

// SendAction 发送一个 OneBot API 请求并等待响应
func (c *Client) SendAction(action string, params map[string]interface{}, timeout time.Duration) (*ActionResponse, error) {
	echo := atomic.AddInt64(&c.echoCount, 1)
	echoStr := formatInt64(echo)

	msg := Action{
		Action: action,
		Params: params,
		Echo:   echoStr,
	}

	data, err := json.Marshal(msg)
	if err != nil {
		return nil, err
	}

	ch := make(chan *ActionResponse, 1)
	c.pending.Store(echoStr, ch)
	defer c.pending.Delete(echoStr)

	c.mu.Lock()
	if c.conn == nil {
		c.mu.Unlock()
		return nil, fmt.Errorf("not connected")
	}
	err = c.conn.WriteMessage(websocket.TextMessage, data)
	c.mu.Unlock()

	if err != nil {
		return nil, err
	}

	if timeout <= 0 {
		timeout = 10 * time.Second
	}

	select {
	case resp := <-ch:
		if resp.Status == "failed" {
			return resp, fmt.Errorf("onebot action %s failed: retcode=%d", action, resp.RetCode)
		}
		return resp, nil
	case <-time.After(timeout):
		return nil, fmt.Errorf("onebot action %s timeout", action)
	}
}

// SendPrivateMessage 发送私聊消息（便捷方法）
func (c *Client) SendPrivateMessage(userID int64, message interface{}, autoEscape bool) (*ActionResponse, error) {
	params := map[string]interface{}{
		"user_id":  userID,
		"message":  message,
		"auto_escape": autoEscape,
	}
	return c.SendAction("send_private_msg", params, 0)
}

// SendGroupMessage 发送群消息（便捷方法）
func (c *Client) SendGroupMessage(groupID int64, message interface{}, autoEscape bool) (*ActionResponse, error) {
	params := map[string]interface{}{
		"group_id":  groupID,
		"message":   message,
		"auto_escape": autoEscape,
	}
	return c.SendAction("send_group_msg", params, 0)
}

// GetLoginInfo 获取登录号信息
func (c *Client) GetLoginInfo() (*ActionResponse, error) {
	return c.SendAction("get_login_info", nil, 0)
}

// GetGroupMemberInfo 获取群成员信息
func (c *Client) GetGroupMemberInfo(groupID, userID int64) (*ActionResponse, error) {
	return c.SendAction("get_group_member_info", map[string]interface{}{
		"group_id": groupID,
		"user_id":  userID,
	}, 0)
}

// GetGroupList 获取群列表
func (c *Client) GetGroupList() (*ActionResponse, error) {
	return c.SendAction("get_group_list", nil, 0)
}

// Close 关闭连接
func (c *Client) Close() error {
	close(c.done)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn != nil {
		return c.conn.Close()
	}
	return nil
}

func formatInt64(n int64) string {
	return fmt.Sprintf("%d", n)
}
