package core

import (
	"testing"

	agentAPI "gitcode.com/JianFeeeee/HomeAgent/internal/agent/api"
	agentIO "gitcode.com/JianFeeeee/HomeAgent/internal/agent/io"
)

// mockOutputDevice implements agentIO.Device for testing output tools
type mockOutputDevice struct {
	name   string
	caps   agentIO.OutputCapability
	tools  []agentIO.ToolDef
	toolFn func(string, map[string]interface{}) (interface{}, error)
}

func (d *mockOutputDevice) Name() string                          { return d.name }
func (d *mockOutputDevice) Type() agentIO.DeviceType              { return agentIO.DeviceOutput }
func (d *mockOutputDevice) Description() string                   { return "mock " + d.name }
func (d *mockOutputDevice) Tools() []agentIO.ToolDef              { return d.tools }
func (d *mockOutputDevice) Start() error                          { return nil }
func (d *mockOutputDevice) Stop() error                           { return nil }
func (d *mockOutputDevice) OutputCapabilities() agentIO.OutputCapability { return d.caps }
func (d *mockOutputDevice) Execute(tool string, args map[string]interface{}) (interface{}, error) {
	if d.toolFn != nil {
		return d.toolFn(tool, args)
	}
	return nil, nil
}

func TestExecuteOutputListChannels(t *testing.T) {
	io := agentIO.NewIOManager()
	dev := &mockOutputDevice{
		name: "speaker",
		caps: agentIO.CapText | agentIO.CapAudio,
	}
	io.RegisterDevice(dev)

	a := &Agent{io: io}
	result := a.executeOutputListChannels()
	if result == "" || result == "没有可用通道" {
		t.Errorf("expected channel list, got: %s", result)
	}
}

func TestExecuteOutputListChannelsEmpty(t *testing.T) {
	io := agentIO.NewIOManager()
	a := &Agent{io: io}
	result := a.executeOutputListChannels()
	if result != "没有可用通道" {
		t.Errorf("expected '没有可用通道', got: %s", result)
	}
}

func TestExecuteOutputSendTool(t *testing.T) {
	io := agentIO.NewIOManager()
	io.RegisterDevice(&mockOutputDevice{
		name: "screen",
		caps: agentIO.CapText,
	})

	a := &Agent{io: io}
	tc := agentAPI.ToolCall{Name: "output_send", Arguments: map[string]interface{}{
		"channel": "screen",
		"content": "hello world",
	}}
	result := a.executeOutputSendTool(tc)
	if result != "已通过 [screen] 通道发送" {
		t.Errorf("unexpected result: %s", result)
	}
}

func TestExecuteOutputSendToolMissingChannel(t *testing.T) {
	io := agentIO.NewIOManager()
	a := &Agent{io: io}
	tc := agentAPI.ToolCall{Name: "output_send", Arguments: map[string]interface{}{
		"content": "hello",
	}}
	result := a.executeOutputSendTool(tc)
	if result != "channel 和 content 不能为空" {
		t.Errorf("unexpected result: %s", result)
	}
}

func TestExecuteOutputSendToolEmptyContent(t *testing.T) {
	io := agentIO.NewIOManager()
	a := &Agent{io: io}
	tc := agentAPI.ToolCall{Name: "output_send", Arguments: map[string]interface{}{
		"channel": "screen",
	}}
	result := a.executeOutputSendTool(tc)
	if result != "channel 和 content 不能为空" {
		t.Errorf("unexpected result: %s", result)
	}
}

func TestExecuteOutputSendToolChannelNotExist(t *testing.T) {
	io := agentIO.NewIOManager()
	a := &Agent{io: io}
	tc := agentAPI.ToolCall{Name: "output_send", Arguments: map[string]interface{}{
		"channel": "nonexistent",
		"content": "hello",
	}}
	result := a.executeOutputSendTool(tc)
	if result != "通道 [nonexistent] 不存在或不可用。可用通道请用 output_list_channels 查看" {
		t.Errorf("unexpected result: %s", result)
	}
}

func TestExecuteOutputSendToolNoTextCap(t *testing.T) {
	io := agentIO.NewIOManager()
	io.RegisterDevice(&mockOutputDevice{
		name: "camera",
		caps: agentIO.CapImage,
	})

	a := &Agent{io: io}
	tc := agentAPI.ToolCall{Name: "output_send", Arguments: map[string]interface{}{
		"channel": "camera",
		"content": "hello",
	}}
	result := a.executeOutputSendTool(tc)
	if result == "已通过 [camera] 通道发送" {
		t.Errorf("should reject channel without text capability")
	}
}

func TestBuildToolDefsOutputToolsAlwaysPresent(t *testing.T) {
	io := agentIO.NewIOManager()
	a := &Agent{io: io, knowledge: nil, docStore: nil, pluginReg: nil}
	tools := a.buildToolDefs()

	foundSend := false
	foundList := false
	for _, td := range tools {
		m, ok := td.(map[string]interface{})
		if !ok {
			continue
		}
		fn, ok := m["function"].(map[string]interface{})
		if !ok {
			continue
		}
		name, _ := fn["name"].(string)
		switch name {
		case "output_send":
			foundSend = true
		case "output_list_channels":
			foundList = true
		}
	}
	if !foundSend {
		t.Error("output_send should always be in tools")
	}
	if !foundList {
		t.Error("output_list_channels should always be in tools")
	}
}

func TestGetAllToolsEmpty(t *testing.T) {
	io := agentIO.NewIOManager()
	a := &Agent{io: io}
	tools := a.buildToolDefs()
	// should have at least output_send, output_list_channels
	if len(tools) < 2 {
		t.Errorf("expected at least 2 tools, got %d", len(tools))
	}
}
