const { spawnSync } = require('child_process');
const path = require('path');
const os = require('os');

const isWin = os.platform().startsWith('win');
const outName = isWin ? 'gdaltweb.exe' : 'gdaltweb';
const outPath = path.join(__dirname, 'bin', outName);

console.log(`Building binary: ${outPath}`);

const args = ['build', '-o', outPath, './services'];
const res = spawnSync('go', args, { stdio: 'inherit', shell: false });
if (res.error) {
  console.error('go build failed:', res.error);
  process.exit(1);
}
process.exit(res.status || 0);
