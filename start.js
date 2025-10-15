const { spawnSync } = require('child_process');
const path = require('path');
const os = require('os');

// Determine binary name depending on platform
const isWin = os.platform().startsWith('win');
const binName = isWin ? 'gdaltweb.exe' : 'gdaltweb';
const binPath = path.join(__dirname, 'bin', binName);

console.log(`Starting binary: ${binPath}`);

const result = spawnSync(binPath, { stdio: 'inherit', shell: false });

if (result.error) {
  console.error('Failed to spawn binary:', result.error);
  process.exit(1);
}
process.exit(result.status || 0);
