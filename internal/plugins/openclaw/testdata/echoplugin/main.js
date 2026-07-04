const readline = require('readline');

const rl = readline.createInterface({
  input: process.stdin,
  output: process.stdout,
  terminal: false
});

const tools = [
  {
    name: 'echo',
    description: 'Echo back the input text',
    inputSchema: {
      type: 'object',
      properties: {
        text: { type: 'string', description: 'Text to echo' }
      },
      required: ['text']
    }
  },
  {
    name: 'add',
    description: 'Add two numbers',
    inputSchema: {
      type: 'object',
      properties: {
        a: { type: 'number', description: 'First number' },
        b: { type: 'number', description: 'Second number' }
      },
      required: ['a', 'b']
    }
  },
  {
    name: 'ping',
    description: 'Health check',
    inputSchema: {
      type: 'object',
      properties: {}
    }
  }
];

rl.on('line', (line) => {
  let req;
  try {
    req = JSON.parse(line);
  } catch (e) {
    sendError(null, -32700, 'Parse error');
    return;
  }

  const id = req.id;
  const method = req.method;

  if (method === 'ping') {
    sendResult(id, { status: 'ok' });
    return;
  }

  if (method === 'tools/list') {
    sendResult(id, { tools });
    return;
  }

  if (method === 'tools/call') {
    const params = req.params || {};
    const toolName = params.name;
    const args = params.arguments || {};

    const tool = tools.find(t => t.name === toolName);
    if (!tool) {
      sendError(id, -32601, `Tool not found: ${toolName}`);
      return;
    }

    let result;
    switch (toolName) {
      case 'echo':
        result = { content: [{ type: 'text', text: `Echo: ${args.text || ''}` }] };
        break;
      case 'add':
        const sum = (Number(args.a) || 0) + (Number(args.b) || 0);
        result = { content: [{ type: 'text', text: `${sum}` }] };
        break;
      default:
        sendError(id, -32601, `Not implemented: ${toolName}`);
        return;
    }
    sendResult(id, result);
    return;
  }

  sendError(id, -32601, `Method not found: ${method}`);
});

function sendResult(id, result) {
  const resp = { jsonrpc: '2.0', id, result };
  process.stdout.write(JSON.stringify(resp) + '\n');
}

function sendError(id, code, message) {
  const resp = { jsonrpc: '2.0', id, error: { code, message } };
  process.stdout.write(JSON.stringify(resp) + '\n');
}

// Sidecar is ready - no startup message needed
