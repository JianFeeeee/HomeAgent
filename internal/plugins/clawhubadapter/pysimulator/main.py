#!/usr/bin/env python3
"""
OpenClaw Python Sidecar Simulator

Loads a Python plugin (main.py) from the plugin directory and
provides JSON-RPC over stdin/stdout for tool listing and execution.

Protocol is identical to the Node.js simulator:
  - ping: returns {"status": "ok"}
  - tools/list: lists registered tools
  - tools/call: executes a tool

Plugin main.py should define:
  def register(api):
      api.register_tool(name, description, parameters, execute_fn)
"""
import importlib.util
import json
import os
import sys

def resolve_entry(plugin_dir):
    """Resolve the plugin entry point."""
    main_py = os.path.join(plugin_dir, 'main.py')
    if os.path.isfile(main_py):
        return main_py
    return None


class PluginAPI:
    """API passed to the plugin's register() function."""

    def __init__(self, plugin_dir):
        self.id = os.path.basename(plugin_dir)
        self.name = os.path.basename(plugin_dir)
        self.version = '1.0.0'
        self.description = ''
        self.source = plugin_dir
        self.root_dir = plugin_dir
        self.config = {}
        self.plugin_config = {}
        self.registration_mode = 'full'
        self._tools = {}

    def register_tool(self, name, description='', parameters=None, execute=None):
        if parameters is None:
            parameters = {'type': 'object', 'properties': {}}
        # Notify Go side
        self._notify('register', {
            'type': 'tool',
            'data': {
                'name': name,
                'description': description,
                'parameters': parameters,
            }
        })
        self._tools[name] = {
            'name': name,
            'description': description,
            'parameters': parameters,
            'execute': execute,
        }

    def register_provider(self, provider_type, provider):
        self._notify('register', {
            'type': 'provider',
            'data': {'name': provider.name if hasattr(provider, 'name') else str(provider)},
        })

    def register_channel(self, channel):
        self._notify('register', {
            'type': 'channel',
            'data': {'name': channel.name if hasattr(channel, 'name') else str(channel)},
        })

    def _notify(self, method, params):
        line = json.dumps({'jsonrpc': '2.0', 'method': method, 'params': params}, ensure_ascii=False)
        sys.stdout.write(line + '\n')
        sys.stdout.flush()


def load_plugin(plugin_dir):
    """Load the plugin from main.py and call its register()."""
    entry_path = resolve_entry(plugin_dir)
    if entry_path is None:
        sys.stderr.write(f'[pysimulator] entry not found in {plugin_dir}\n')
        sys.exit(1)

    sys.path.insert(0, plugin_dir)
    spec = importlib.util.spec_from_file_location('plugin_main', entry_path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)

    if not hasattr(module, 'register'):
        sys.stderr.write(f'[pysimulator] {entry_path} must define a register(api) function\n')
        sys.exit(1)

    api = PluginAPI(plugin_dir)
    module.register(api)
    return api


def main():
    if len(sys.argv) < 2:
        sys.stderr.write('[pysimulator] usage: python3 main.py <plugin-dir>\n')
        sys.exit(1)

    plugin_dir = os.path.abspath(sys.argv[1])
    api = load_plugin(plugin_dir)

    # JSON-RPC loop over stdin
    for line in sys.stdin:
        line = line.strip()
        if not line:
            continue

        try:
            req = json.loads(line)
        except json.JSONDecodeError:
            resp = {'jsonrpc': '2.0', 'id': None, 'error': {'code': -32700, 'message': 'Parse error'}}
            sys.stdout.write(json.dumps(resp, ensure_ascii=False) + '\n')
            sys.stdout.flush()
            continue

        req_id = req.get('id')
        method = req.get('method', '')

        if method == 'ping':
            resp = {'jsonrpc': '2.0', 'id': req_id, 'result': {'status': 'ok'}}
        elif method == 'tools/list':
            tools = [
                {'name': t['name'], 'description': t['description'], 'inputSchema': t['parameters']}
                for t in api._tools.values()
            ]
            resp = {'jsonrpc': '2.0', 'id': req_id, 'result': {'tools': tools}}
        elif method == 'tools/call':
            params = req.get('params', {})
            tool_name = params.get('name', '')
            arguments = params.get('arguments', {})

            tool = api._tools.get(tool_name)
            if tool is None:
                resp = {'jsonrpc': '2.0', 'id': req_id, 'error': {'code': -32601, 'message': f'Tool not found: {tool_name}'}}
            elif tool.get('execute') is None:
                resp = {'jsonrpc': '2.0', 'id': req_id, 'error': {'code': -32603, 'message': f'Tool {tool_name} has no execute function'}}
            else:
                try:
                    result = tool['execute'](arguments)
                    if isinstance(result, dict) and 'content' in result:
                        resp = {'jsonrpc': '2.0', 'id': req_id, 'result': result}
                    else:
                        text = str(result) if not isinstance(result, str) else result
                        resp = {'jsonrpc': '2.0', 'id': req_id, 'result': {'content': [{'type': 'text', 'text': text}]}}
                except Exception as e:
                    resp = {'jsonrpc': '2.0', 'id': req_id, 'error': {'code': -32603, 'message': str(e)}}
        else:
            resp = {'jsonrpc': '2.0', 'id': req_id, 'error': {'code': -32601, 'message': f'Method not found: {method}'}}

        sys.stdout.write(json.dumps(resp, ensure_ascii=False) + '\n')
        sys.stdout.flush()


if __name__ == '__main__':
    main()
