// How long the browser takes to turn a PNG into something drawable.
//
// The gateway pays about 9 ms to build one scroll-like redraw and splits it into
// strips to do so, which trades its own parallel time for the browser's serial
// time: every strip is one createImageBitmap and one texture upload, and those
// do not run in parallel with each other. This measures which side of that trade
// is worse, by decoding the same pixels as one image and as many.
//
// Run: node test/e2e/decode-cost.js
const { chromium } = require('playwright');

const CHROME = process.env.ADX_CHROME ||
  '/home/upi/.cache/ms-playwright/chromium-1217/chrome-linux64/chrome';

(async () => {
  const browser = await chromium.launch({
    executablePath: CHROME,
    args: ['--use-gl=angle', '--use-angle=swiftshader', '--enable-unsafe-swiftshader'],
  });
  const page = await browser.newPage();
  await page.goto('about:blank');

  const result = await page.evaluate(async () => {
    // A 1280x720 desktop's worth of pixels, as one image and as N strips.
    const W = 1280, H = 720;

    function makeCanvas(w, h, seed) {
      const c = document.createElement('canvas');
      c.width = w; c.height = h;
      const x = c.getContext('2d');
      // Content that does not compress to nothing, so the decode is real work.
      for (let i = 0; i < h; i += 4) {
        x.fillStyle = `rgb(${(i * 7 + seed) % 256},${(i * 13) % 256},${(i * 29) % 256})`;
        x.fillRect(0, i, w, 2);
      }
      return c;
    }

    const toBlob = (c) => new Promise((r) => c.toBlob(r, 'image/png'));

    async function timeit(fn, reps) {
      const t0 = performance.now();
      for (let i = 0; i < reps; i++) await fn(i);
      return (performance.now() - t0) / reps;
    }

    const gl = document.createElement('canvas').getContext('webgl2');
    const tex = gl.createTexture();
    gl.bindTexture(gl.TEXTURE_2D, tex);
    const upload = async (bmp) => {
      gl.texImage2D(gl.TEXTURE_2D, 0, gl.RGBA, gl.RGBA, gl.UNSIGNED_BYTE, bmp);
    };

    const out = [];

    // One image for the whole desktop.
    const whole = await toBlob(makeCanvas(W, H, 1));
    out.push({
      what: '1 image 1280x720',
      decodeMs: await timeit(async () => {
        const b = await createImageBitmap(whole);
        await upload(b);
        b.close();
      }, 10),
    });

    // The same pixels as N strips, which is what splitting a region produces.
    for (const n of [2, 4, 8, 16]) {
      const stripH = Math.max(8, Math.floor(H / n));
      const blobs = [];
      for (let i = 0; i < n; i++) blobs.push(await toBlob(makeCanvas(W, stripH, i + 2)));
      out.push({
        what: `${n} strips of 1280x${stripH}`,
        decodeMs: await timeit(async () => {
          for (const b of blobs) {
            const bmp = await createImageBitmap(b);
            await upload(bmp);
            bmp.close();
          }
        }, 6),
      });
    }

    return out;
  });

  console.log('decode + upload per redraw of 1280x720:');
  for (const r of result) {
    console.log(`  ${r.what.padEnd(24)} ${r.decodeMs.toFixed(2)} ms`);
  }
  await browser.close();
})().catch((e) => { console.log('harness error:', e.message); process.exit(2); });
