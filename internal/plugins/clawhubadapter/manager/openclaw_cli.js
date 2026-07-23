#!/usr/bin/env node
const path = require('path');
const fs = require('fs');
const { fork } = require('child_process');

const binDir = __dirname;
const skillsdirPath = path.join(binDir, '..', '.skillsdir');

if (!fs.existsSync(skillsdirPath)) {
  process.stderr.write('Error: OpenClaw manager not running (no .skillsdir found)\n');
  process.exit(1);
}

const skillsDir = fs.readFileSync(skillsdirPath, 'utf8').trim();
const managerPath = path.join(binDir, '..', 'manager.js');

const proc = fork(managerPath, [skillsDir, ...process.argv.slice(2)], {
  stdio: 'inherit',
  env: { ...process.env, OPENCLAW_CLI: '1' },
});

proc.on('exit', (code) => process.exit(code));
