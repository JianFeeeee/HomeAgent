// DeviceBridge DLL 桥接模块
// 提供设备桥共享库的 Node.js 封装，GUI 通过 FFI 调用 Go 编译的 DLL。
// 优先尝试加载 DLL，失败则回退到纯 JS 实现（保留兼容）。

const path = require('path');
const os = require('os');

let koffi = null;
let bridgeLib = null;
let _handle = null;

// DLL 路径
function dllPath() {
  const dir = __dirname;
  const plat = os.platform();
  if (plat === 'win32') {
    return path.join(dir, 'devicebridge.dll');
  }
  // Linux/Mac 使用 .so/.dylib
  const ext = plat === 'darwin' ? 'dylib' : 'so';
  return path.join(dir, `devicebridge.${ext}`);
}

// 尝试加载 FFI 库
async function loadFFI() {
  try {
    koffi = require('koffi');
    return true;
  } catch (e) {
    try {
      const ffi = require('ffi-napi');
      const ref = require('ref-napi');
      // 使用 ffi-napi 作为备选
      return true;
    } catch (e2) {
      return false;
    }
  }
}

// 加载 DLL
function loadDLL() {
  const dll = dllPath();
  try {
    if (koffi) {
      return koffi.load(dll);
    }
    const ffi = require('ffi-napi');
    const ref = require('ref-napi');
    return ffi.Library(dll, {
      'devicebridge_new': ['pointer', ['string', 'string', 'string', 'string', 'pointer', 'int']],
      'devicebridge_start': ['int', ['pointer']],
      'devicebridge_stop': ['void', ['pointer']],
      'devicebridge_free': ['void', ['pointer']],
      'devicebridge_connected': ['int', ['pointer']],
      'devicebridge_device_id': ['string', ['pointer']],
      'devicebridge_send_result': ['int', ['pointer', 'string', 'string', 'string', 'string']],
      'devicebridge_send_event': ['void', ['pointer', 'string', 'string']],
      'devicebridge_send_status': ['void', ['pointer', 'string']],
      'devicebridge_send_data_start': ['void', ['pointer', 'string', 'string', 'string', 'int']],
      'devicebridge_send_data_chunk': ['int', ['pointer', 'pointer', 'int']],
      'devicebridge_send_data_end': ['void', ['pointer', 'string', 'string', 'string']],
    });
  } catch (e) {
    console.error('[devicebridge-dll] load failed:', e.message);
    return null;
  }
}

// 设备桥封装
class DeviceBridgeDLL {
  constructor() {
    this.connected = false;
    this.deviceId = '';
    this._onCmd = null;
    this._onData = null;
  }

  // 初始化并连接
  async start(gateway, token, deviceId, deviceName, caps, info) {
    bridgeLib = loadDLL();
    if (!bridgeLib) {
      throw new Error('DLL not loaded');
    }

    // 构建 caps 数组
    const capsArr = caps.map(c => Buffer.from(c + '\0'));
    const capsPtr = Buffer.alloc(8 * capsArr.length);
    // 简化：实际 FFI 调用需要更复杂的参数处理
    // 这里使用 koffi 方式
    
    if (koffi) {
      // 使用 koffi 调用
      try {
        // TODO: 实现 koffi 调用
        throw new Error('koffi not fully implemented');
      } catch (e) {
        throw e;
      }
    }

    throw new Error('FFI library not available. Install koffi or ffi-napi');
  }

  stop() {
    if (bridgeLib && _handle) {
      try {
        bridgeLib.devicebridge_stop(_handle);
        bridgeLib.devicebridge_free(_handle);
      } catch (e) {}
      _handle = null;
      this.connected = false;
    }
  }

  sendResult(reqId, status, output, error) {
    if (!bridgeLib || !_handle) return;
    try {
      bridgeLib.devicebridge_send_result(_handle, reqId, status, output || '', error || '');
    } catch (e) {
      console.error('[devicebridge-dll] sendResult error:', e);
    }
  }
}

module.exports = { DeviceBridgeDLL, loadFFI };