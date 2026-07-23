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
import asyncio
import importlib.util
import inspect
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


# ---- Legacy Python Plugin Detection ----

def _resolve_env_vars(plugin_dir):
    """Read env var requirements from skill.json and resolve from OS env."""
    skill_json = os.path.join(plugin_dir, 'skill.json')
    if not os.path.isfile(skill_json):
        return {}
    try:
        with open(skill_json, 'r') as f:
            meta = json.load(f)
    except Exception:
        return {}

    env_map = {}
    for key in meta.get('required_env_vars', []):
        val = os.environ.get(key) or os.environ.get(key.lower())
        if val:
            env_map[key.lower()] = val
    for key in meta.get('optional_env_vars', []):
        val = os.environ.get(key) or os.environ.get(key.lower())
        if val:
            env_map[key.lower()] = val

    return env_map


def _collect_sub_skills(instance):
    """Discover sub-module names from a class instance by finding attributes with execute methods.
    
    Prefer instance __dict__ attributes over properties to avoid triggering side effects.
    """
    skills = []
    seen = set()

    # First look at instance __dict__ (safe, no property triggers)
    for attr_name in list(instance.__dict__.keys()):
        if attr_name.startswith('_'):
            continue
        attr = instance.__dict__[attr_name]
        seen.add(attr_name)
        if hasattr(attr, 'execute') and callable(attr.execute):
            try:
                sig = inspect.signature(attr.execute)
                params = list(sig.parameters.keys())
                if 'action' in params and len(params) >= 2:
                    skills.append(attr_name)
            except (ValueError, TypeError):
                continue

    # Also check class-level non-property attributes
    for attr_name in dir(type(instance)):
        if attr_name.startswith('_') or attr_name in seen:
            continue
        try:
            attr = getattr(type(instance), attr_name, None)
        except Exception:
            continue
        if isinstance(attr, property):
            continue  # skip properties (may raise on access)
        try:
            attr = getattr(instance, attr_name, None)
        except Exception:
            continue
        if attr is None:
            continue
        seen.add(attr_name)
        if hasattr(attr, 'execute') and callable(attr.execute):
            try:
                sig = inspect.signature(attr.execute)
                params = list(sig.parameters.keys())
                if 'action' in params and len(params) >= 2:
                    skills.append(attr_name)
            except (ValueError, TypeError):
                continue

    return skills


def _readable_class_name(cls):
    """Convert PascalCase to snake_case for readable names."""
    import re
    name = cls.__name__
    s1 = re.sub('(.)([A-Z][a-z]+)', r'\1_\2', name)
    return re.sub('([a-z0-9])([A-Z])', r'\1_\2', s1).lower()


def _make_async_execute(instance, skill_name):
    """Wrap an async sub-module execute into a sync callable for JSON-RPC."""
    def execute(args):
        action = args.pop('action', '') if isinstance(args, dict) else ''
        result = asyncio.run(instance.execute(skill_name=skill_name, action=action, **args))
        return result
    return execute


def _make_legacy_execute(instance, attr_name):
    """Wrap an async legacy execute into a sync callable for JSON-RPC."""
    def execute(args):
        action = args.pop('action', '') if isinstance(args, dict) else ''
        handler = getattr(instance, attr_name)
        result = asyncio.run(handler.execute(action=action, **args))
        return result
    return execute


def detect_legacy_plugin(module, plugin_dir, plugin_name):
    """Auto-detect legacy Python plugin patterns and register as OC tools.

    Returns a PluginAPI with discovered tools, or None if no pattern matches.
    """
    api = PluginAPI(plugin_dir)

    # Pattern 1: Module has a main class with execute(skill_name, action, **kwargs)
    # Look for any class that has an 'execute' method accepting 'skill_name'
    main_class = None
    main_class_name = None

    for name in dir(module):
        obj = getattr(module, name, None)
        if not isinstance(obj, type):
            continue
        if not hasattr(obj, 'execute') or not callable(getattr(obj, 'execute', None)):
            continue
        try:
            sig = inspect.signature(obj.execute)
            params = list(sig.parameters.keys())
            if 'skill_name' in params:
                main_class = obj
                main_class_name = name
                break
        except (ValueError, TypeError):
            continue

    if main_class is None:
        return None

    # Resolve env vars for constructor
    env_args = _resolve_env_vars(plugin_dir)
    try:
        sig = inspect.signature(main_class.__init__)
        init_params = list(sig.parameters.keys())[1:]  # skip self
        kwargs = {}
        for p in init_params:
            lower_p = p.lower()
            if lower_p in env_args:
                kwargs[p] = env_args[lower_p]
        sys.stderr.write(f'[pysimulator] legacy: instantiating {main_class_name} with {kwargs}\n')
        instance = main_class(**kwargs)
    except Exception as e:
        sys.stderr.write(f'[pysimulator] legacy: cannot instantiate {main_class_name}: {e}\n')
        return None

    # Discover sub-modules
    sub_skills = _collect_sub_skills(instance)
    sys.stderr.write(f'[pysimulator] legacy: discovered sub-skills: {sub_skills}\n')

    discovered_actions = {}
    for attr_name in sub_skills:
        handler = getattr(instance, attr_name)
        # Build parameter schema from the execute signature
        try:
            sig = inspect.signature(handler.execute)
            param_props = {}
            for p_name, p_sig in list(sig.parameters.items())[1:]:  # skip 'self'
                if p_name == 'action':
                    param_props['action'] = {
                        'type': 'string',
                        'description': f'Action to perform in {attr_name}',
                    }
                    continue
                if p_sig.annotation != inspect.Parameter.empty:
                    p_type = 'string'
                    if p_sig.annotation is int:
                        p_type = 'number'
                    elif p_sig.annotation is bool:
                        p_type = 'boolean'
                    elif p_sig.annotation is float:
                        p_type = 'number'
                    else:
                        anno_str = str(p_sig.annotation)
                        if 'int' in anno_str:
                            p_type = 'number'
                        elif 'bool' in anno_str:
                            p_type = 'boolean'
                        elif 'float' in anno_str:
                            p_type = 'number'
                    param_props[p_name] = {
                        'type': p_type,
                        'description': f'Parameter {p_name}',
                    }
                else:
                    param_props[p_name] = {
                        'type': 'string',
                        'description': f'Parameter {p_name}',
                    }

            required = ['action']
            schema = {
                'type': 'object',
                'properties': param_props,
                'required': required,
            }

            tool_name = attr_name
            doc = handler.execute.__doc__ or f'{attr_name} operations'
            first_line = doc.strip().split('\n')[0] if doc else f'{attr_name} module'

            discovered_actions[attr_name] = (tool_name, first_line, schema)
        except (ValueError, TypeError) as e:
            sys.stderr.write(f'[pysimulator] legacy: skip {attr_name}: {e}\n')
            continue

    if discovered_actions:
        # Register each sub-skill as a separate OC tool
        for attr_name, (tool_name, desc, schema) in discovered_actions.items():
            handler = getattr(instance, attr_name)
            api.register_tool(
                name=tool_name,
                description=f'{plugin_name}: {desc}',
                parameters=schema,
                execute=_make_legacy_execute(instance, attr_name),
            )
            sys.stderr.write(f'[pysimulator] legacy registered tool: {tool_name}\n')
    else:
        # Fallback: single tool wrapping the whole plugin
        try:
            sig = inspect.signature(main_class.execute)
            skill_params = [p for p in list(sig.parameters.keys())[1:]]  # skip self
            param_props = {}
            for p in skill_params:
                param_props[p] = {'type': 'string', 'description': p}

            api.register_tool(
                name=plugin_name,
                description=f'{plugin_name}: unified operations',
                parameters={
                    'type': 'object',
                    'properties': param_props,
                    'required': skill_params,
                },
                execute=_make_async_execute(instance, None),
            )
            sys.stderr.write(f'[pysimulator] legacy registered fallback tool: {plugin_name}\n')
        except Exception as e:
            sys.stderr.write(f'[pysimulator] legacy: fallback failed: {e}\n')
            return None

    return api


def load_plugin(plugin_dir):
    """Load the plugin from main.py.

    Tries OC-style register(api) first, then auto-detects legacy patterns.
    """
    entry_path = resolve_entry(plugin_dir)
    if entry_path is None:
        sys.stderr.write(f'[pysimulator] entry not found in {plugin_dir}\n')
        sys.exit(1)

    sys.path.insert(0, plugin_dir)
    spec = importlib.util.spec_from_file_location('plugin_main', entry_path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)

    plugin_name = os.path.basename(plugin_dir)

    # OC-style: register(api)
    if hasattr(module, 'register'):
        api = PluginAPI(plugin_dir)
        module.register(api)
        return api

    # Legacy auto-detection
    sys.stderr.write(f'[pysimulator] no register() function, trying legacy detection for {plugin_name}...\n')
    api = detect_legacy_plugin(module, plugin_dir, plugin_name)
    if api is not None:
        return api

    sys.stderr.write(f'[pysimulator] {entry_path} must define a register(api) function\n')
    sys.exit(1)


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
