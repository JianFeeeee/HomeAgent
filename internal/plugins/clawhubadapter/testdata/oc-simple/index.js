module.exports = {
  default: {
    id: 'oc-simple',
    name: 'OC Simple',
    version: '1.0.0',
    description: 'Simple test plugin for OpenClaw simulator',
    register(api) {
      api.registerTool({
        name: 'greet',
        description: 'Greet someone',
        parameters: {
          type: 'object',
          properties: {
            name: { type: 'string', description: 'Name to greet' },
          },
          required: ['name'],
        },
        execute(id, params) {
          return `Hello, ${params.name}!`;
        },
      });
      api.registerTool({
        name: 'ping',
        description: 'Health check ping',
        execute(id, params) {
          return 'pong';
        },
      });
    },
  },
};
