#!/usr/bin/env node
import path from 'node:path';
import { spawn } from 'node:child_process';
import { getBinaryName } from './platform';

const binaryPath = path.join(__dirname, 'bin', getBinaryName());

const child = spawn(binaryPath, process.argv.slice(2), {
  stdio: 'inherit'
});

child.on('error', (err: Error) => {
  if (err.message.includes('ENOENT')) {
    console.error('erpc binary not found. Try reinstalling @erpc-cloud/cli');
  } else {
    console.error('Failed to start erpc:', err.message);
  }
  process.exit(1);
});

child.on('exit', (code: number | null) => {
  process.exit(code || 0);
});