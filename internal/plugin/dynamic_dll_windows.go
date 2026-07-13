//go:build windows

package plugin

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"unsafe"

	sdk "gitcode.com/JianFeeeee/HomeAgent/internal/sdk"
)

// dllPlugin wraps a Windows DLL compiled with -buildmode=c-shared.
//
// Required exports:
//
//	NewPlugin(name *C.char, configJSON *C.char) unsafe.Pointer  → plugin handle
//	StartPlugin(handle unsafe.Pointer) C.int
//	StopPlugin(handle unsafe.Pointer) C.int
//	DestroyPlugin(handle unsafe.Pointer)
//
// Optional exports (tool registration):
//
//	GetToolDefsJSON(handle unsafe.Pointer) *C.char              → JSON array of tool defs
//	InvokeToolJSON(handle unsafe.Pointer, toolName *C.char, argsJSON *C.char) *C.char
//	FreeCString(s *C.char)                                       → free C string from DLL
//	GetStagesJSON(handle unsafe.Pointer) *C.char                 → JSON array of stage names
//	InvokeStage(handle unsafe.Pointer, stage *C.char, contextJSON *C.char) C.int
type dllPlugin struct {
	name    string
	dll     syscall.Handle
	handle  uintptr
	sdk     *sdk.PluginSDK

	// cached proc addresses
	newPlugin     uintptr
	startPlugin   uintptr
	stopPlugin    uintptr
	destroyPlugin uintptr
	getTools      uintptr
	invokeTool    uintptr
	freeCStr      uintptr
	getStages     uintptr
	invokeStage   uintptr
}

func findProc(dll syscall.Handle, name string) uintptr {
	addr, err := syscall.GetProcAddress(dll, name)
	if err != nil {
		return 0
	}
	return addr
}

func newDLLPlugin(dllPath, name string, config map[string]interface{}) (*dllPlugin, error) {
	dll, err := syscall.LoadLibrary(dllPath)
	if err != nil {
		return nil, fmt.Errorf("LoadLibrary %s: %w", dllPath, err)
	}

	np := findProc(dll, "NewPlugin")
	if np == 0 {
		_ = syscall.FreeLibrary(dll)
		return nil, fmt.Errorf("dll %s must export NewPlugin", name)
	}

	return &dllPlugin{
		name: name,
		dll:  dll,
		// required
		newPlugin:     np,
		startPlugin:   findProc(dll, "StartPlugin"),
		stopPlugin:    findProc(dll, "StopPlugin"),
		destroyPlugin: findProc(dll, "DestroyPlugin"),
		// optional tool/stage API
		getTools:    findProc(dll, "GetToolDefsJSON"),
		invokeTool:  findProc(dll, "InvokeToolJSON"),
		freeCStr:    findProc(dll, "FreeCString"),
		getStages:   findProc(dll, "GetStagesJSON"),
		invokeStage: findProc(dll, "InvokeStage"),
	}, nil
}

func (p *dllPlugin) Name() string { return p.name }

func (p *dllPlugin) Start(s *sdk.PluginSDK) error {
	p.sdk = s

	cfgJSON, _ := json.Marshal(map[string]interface{}{
		"name":   p.name,
		"config": s.Settings().Dump(),
	})
	cName := append([]byte(p.name), 0)
	cConfig := append(cfgJSON, 0)

	ret, _, _ := syscall.SyscallN(
		p.newPlugin,
		uintptr(unsafe.Pointer(&cName[0])),
		uintptr(unsafe.Pointer(&cConfig[0])),
	)
	if ret == 0 {
		_ = syscall.FreeLibrary(p.dll)
		return fmt.Errorf("dll NewPlugin %s returned nil", p.name)
	}
	p.handle = ret

	if p.startPlugin != 0 {
		syscall.SyscallN(p.startPlugin, p.handle)
	}

	// discover and register tools from DLL
	if p.getTools != 0 {
		if err := p.registerTools(s); err != nil {
			return fmt.Errorf("dll %s register tools: %w", p.name, err)
		}
	}
	if p.getStages != 0 {
		p.registerStages(s)
	}
	return nil
}

func (p *dllPlugin) Stop() error {
	if p.stopPlugin != 0 {
		syscall.SyscallN(p.stopPlugin, p.handle)
	}
	if p.destroyPlugin != 0 {
		syscall.SyscallN(p.destroyPlugin, p.handle)
	}
	_ = syscall.FreeLibrary(p.dll)
	return nil
}

// --- tool registration via C ABI ---

type dllToolDef struct {
	Name        string                   `json:"name"`
	Description string                   `json:"description"`
	Parameters  map[string]interface{}   `json:"parameters,omitempty"`
}

func (p *dllPlugin) registerTools(s *sdk.PluginSDK) error {
	ret, _, _ := syscall.SyscallN(p.getTools, p.handle)
	if ret == 0 {
		return nil // no tools
	}
	defsJSON := cStringPtrToString(ret)
	if p.freeCStr != 0 {
		syscall.SyscallN(p.freeCStr, ret)
	}

	var defs []dllToolDef
	if err := json.Unmarshal([]byte(defsJSON), &defs); err != nil {
		return fmt.Errorf("parse tool defs: %w", err)
	}
	for _, d := range defs {
		if d.Name == "" {
			continue
		}
		toolName := d.Name
		handler := p.makeToolHandler(toolName)
		s.RegisterTool(toolName, sdk.ToolDef{
			Name:        toolName,
			Description: d.Description,
			Parameters:  d.Parameters,
			Plugin:      p.name,
		}, handler)
	}
	return nil
}

func (p *dllPlugin) makeToolHandler(toolName string) sdk.ToolHandler {
	return func(args map[string]interface{}) (interface{}, error) {
		if p.invokeTool == 0 {
			return nil, fmt.Errorf("dll %s does not export InvokeToolJSON", p.name)
		}
		argsJSON, _ := json.Marshal(args)
		cToolName := append([]byte(toolName), 0)
		cArgs := append(argsJSON, 0)

		ret, _, _ := syscall.SyscallN(
			p.invokeTool,
			p.handle,
			uintptr(unsafe.Pointer(&cToolName[0])),
			uintptr(unsafe.Pointer(&cArgs[0])),
		)
		if ret == 0 {
			return nil, fmt.Errorf("dll InvokeToolJSON %s returned nil", toolName)
		}
		resultJSON := cStringPtrToString(ret)
		if p.freeCStr != 0 {
			syscall.SyscallN(p.freeCStr, ret)
		}
		var result map[string]interface{}
		if err := json.Unmarshal([]byte(resultJSON), &result); err != nil {
			return nil, fmt.Errorf("dll tool %s result parse: %w", toolName, err)
		}
		return result, nil
	}
}

func (p *dllPlugin) registerStages(s *sdk.PluginSDK) {
	ret, _, _ := syscall.SyscallN(p.getStages, p.handle)
	if ret == 0 {
		return
	}
	stagesJSON := cStringPtrToString(ret)
	if p.freeCStr != 0 {
		syscall.SyscallN(p.freeCStr, ret)
	}
	type stageEntry struct {
		Stage string `json:"stage"`
	}
	var entries []stageEntry
	if err := json.Unmarshal([]byte(stagesJSON), &entries); err != nil {
		return
	}
	for _, e := range entries {
		if e.Stage == "" {
			continue
		}
		stageName := sdk.Stage(e.Stage)
		stage := stageName
		s.RegisterStage(stage, func(sc *sdk.StageContext) error {
			if p.invokeStage == 0 {
				return nil
			}
			ctxJSON, _ := json.Marshal(map[string]interface{}{
				"raw_message": sc.RawMessage,
				"user_id":     sc.UserID,
				"phase":       string(sc.Phase),
			})
			cStage := append([]byte(stage), 0)
			cCtx := append(ctxJSON, 0)
			syscall.SyscallN(
				p.invokeStage,
				p.handle,
				uintptr(unsafe.Pointer(&cStage[0])),
				uintptr(unsafe.Pointer(&cCtx[0])),
			)
			return nil
		})
	}
}

func cStringPtrToString(ptr uintptr) string {
	if ptr == 0 {
		return ""
	}
	var buf []byte
	for i := uintptr(0); ; i++ {
		b := *(*byte)(unsafe.Pointer(ptr + i))
		if b == 0 {
			break
		}
		buf = append(buf, b)
	}
	return string(buf)
}

// tryLoadDLL 尝试从插件目录加载 plugin.dll。
// 返回 nil,nil 表示目录中没有 plugin.dll。
func tryLoadDLL(dir, name string, config map[string]interface{}) (sdk.Plugin, error) {
	dllPath := filepath.Join(dir, dllEntry)
	if _, err := os.Stat(dllPath); os.IsNotExist(err) {
		return nil, nil
	}
	return newDLLPlugin(dllPath, name, config)
}
