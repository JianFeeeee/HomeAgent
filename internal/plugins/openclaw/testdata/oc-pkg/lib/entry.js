module.exports = {
  default: {
    id: 'oc-pkg',
    name: 'OC Package',
    version: '2.0.0',
    description: 'Test plugin discovered via package.json extensions',
    register(api) {
      api.registerTool({
        name: 'add',
        description: 'Add two numbers',
        parameters: {
          type: 'object',
          properties: {
            a: { type: 'number', description: 'First number' },
            b: { type: 'number', description: 'Second number' },
          },
          required: ['a', 'b'],
        },
        execute(id, params) {
          return String(params.a + params.b);
        },
      });

      api.registerTool({
        name: 'info',
        description: 'Return plugin info',
        execute(id, params) {
          return JSON.stringify({ id: this.id, name: this.name, version: this.version });
        },
      });
    },
  },
};
