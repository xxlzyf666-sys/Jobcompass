import { cp, mkdir, copyFile } from 'node:fs/promises';
import { fileURLToPath } from 'node:url';

const root = fileURLToPath(new URL('../', import.meta.url));
const source = `${root}node_modules/pdfjs-dist`;
const destination = `${root}internal/app/web/vendor`;
await mkdir(destination, { recursive: true });
for (const name of ['pdf.mjs', 'pdf.worker.mjs']) {
  await copyFile(`${source}/build/${name}`, `${destination}/${name}`);
}
await copyFile(`${source}/LICENSE`, `${destination}/PDFJS-LICENSE.txt`);
for (const directory of ['cmaps', 'standard_fonts', 'wasm']) {
  await cp(`${source}/${directory}`, `${destination}/${directory}`, { recursive: true });
}
process.stdout.write('PDF.js assets copied for embedding; no Node.js runtime is needed on the server.\n');
