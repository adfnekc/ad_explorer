// Offline check that the browser never presents a screen made of pieces from
// more than one moment.
//
// The gateway is fed by adx-mockframes -motion, which dirties one horizontal
// strip over and over and re-sends the whole of it each time, block and all. A
// correct client shows one block. A client that composed a screen out of regions
// that arrived at different times shows two of them, or a fragment of one, and
// that is the failure that a person describes as tearing.
//
// Run it with: node test/e2e/motion.js [port]
const { chromium } = require('playwright');
const { spawn } = require('node:child_process');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');

const CHROME = process.env.ADX_CHROME ||
  '/home/upi/.cache/ms-playwright/chromium-1217/chrome-linux64/chrome';
const PORT = Number(process.argv[2] || 18699);
const BASE = `http://127.0.0.1:${PORT}`;

// The mock's colours, so the read-back can tell the block from the track.
const BAR = [232, 72, 32];
const TRACK = [24, 26, 30];

const root = path.resolve(__dirname, '..', '..');
const mockBin = path.join(os.tmpdir(), 'adxmotion-mock');

function sleep(ms) { return new Promise((r) => setTimeout(r, ms)); }

async function build() {
  const go = process.env.ADX_GO || '/home/upi/sdk/go1.25.2/bin/go';
  const out = await new Promise((resolve, reject) => {
    const p = spawn(go, ['build', '-o', mockBin, './cmd/adx-mockframes'], { cwd: root });
    let err = '';
    p.stderr.on('data', (d) => { err += d; });
    p.on('close', (c) => (c === 0 ? resolve() : reject(new Error(err))));
  });
  return out;
}

(async () => {
  await build();

  const mock = spawn(mockBin, ['-motion', '-listen', `127.0.0.1:${PORT}`], { stdio: 'ignore' });
  await sleep(300);
  const force2d = !!process.env.ADX_FORCE_2D;
  const browser = await chromium.launch({
    executablePath: CHROME,
    // A headless browser with no display paces requestAnimationFrame at a few
    // frames a second, which would leave the test looking at a handful of
    // presentations instead of the stream it is meant to watch.
    args: [
      '--disable-frame-rate-limit',
      '--disable-gpu-vsync',
      '--disable-background-timer-throttling',
      '--disable-backgrounding-occluded-windows',
      '--use-gl=angle',
      '--use-angle=swiftshader',
      '--enable-unsafe-swiftshader',
    ],
  });

  let failures = 0;
  try {
    for (let round = 0; round < 10; round++) {
      const ctx = await browser.newContext();
      if (force2d) {
        // Refusing WebGL on purpose, so the same frames go down the 2D
        // compositor and a fault that is only on one path can be told apart.
        await ctx.addInitScript(() => {
          const real = HTMLCanvasElement.prototype.getContext;
          HTMLCanvasElement.prototype.getContext = function (type, ...rest) {
            if (String(type).includes('webgl')) return null;
            return real.call(this, type, ...rest);
          };
        });
      }
      const page = await ctx.newPage();
      const errors = [];
      page.on('pageerror', (e) => errors.push(String(e)));
      await page.goto(BASE + '/', { waitUntil: 'domcontentloaded' });

      // The mock's /api/sessions answers 200, so the console considers
      // itself signed in and shows the workspace. Only fill the console
      // password when the login screen is actually up.
      const pw = page.locator('#password');
      if (await pw.isVisible().catch(() => false)) {
        await pw.fill('anything');
        await page.click('#loginButton');
      }

      await page.waitForSelector('#newHost', { state: 'visible', timeout: 8000 });
      await page.click('#newHost');
      await page.fill('#fHost', 'mock:0');
      await page.fill('#fPort', '3389');
      await page.fill('#fUser', 'u');
      await page.fill('#fPassword', 'p');
      await page.dispatchEvent('#hostForm', 'submit');

      await page.waitForTimeout(2000);

      // Every presentation, not a sample of them: the composition that matters
      // is the one that was on screen for a single refresh.
      const seen = await page.evaluate(() => new Promise((resolve) => {
        const out = [];
        const screenEl = document.querySelector('#screen');
        const w = screenEl.width, h = screenEl.height;
        const c = document.createElement('canvas');
        c.width = w; c.height = h;
        const ctx = c.getContext('2d', { willReadFrequently: true });

        // Runs of bar-coloured pixels across the band the block travels in. The
        // block is 10px of one colour on a track of another, so a screen with
        // two of them is a screen made of two moments.
        const BAR = [232, 72, 32];
        const barRuns = (d) => {
          const top = Math.round(60 * (h / 240));
          const band = Math.round(100 * (h / 240));
          const barW = Math.max(1, Math.round(10 * (w / 320)));
          const isBar = (i) =>
            Math.abs(d[i] - BAR[0]) + Math.abs(d[i + 1] - BAR[1]) +
            Math.abs(d[i + 2] - BAR[2]) < 90;
          let runs = 0, inRun = false, pixels = 0, minX = -1, maxX = -1;
          for (let x = 0; x < w; x++) {
            let hit = false;
            for (let y = top; y < top + band; y += 2) {
              if (isBar((y * w + x) * 4)) { hit = true; break; }
            }
            if (hit && !inRun) runs++;
            inRun = hit;
            if (hit) { pixels++; if (minX < 0) minX = x; maxX = x; }
          }
          // The bar is also reported as the ranges that make it up, because
          // "two blocks" is only useful once it is known where they are.
          const isBarX = (x) => {
            for (let y = top; y < top + band; y += 2) {
              if (isBar((y * w + x) * 4)) return true;
            }
            return false;
          };
          const ranges = [];
          let start = -1;
          for (let x = 0; x <= w; x++) {
            const hit = x < w && isBarX(x);
            if (hit && start < 0) start = x;
            if (!hit && start >= 0) { ranges.push([start, x - 1]); start = -1; }
          }
          return { runs, pixels, spread: pixels ? maxX - minX : 0, barW, ranges };
        };

        const look = () => {
          ctx.drawImage(screenEl, 0, 0);
          const d = ctx.getImageData(0, 0, w, h).data;
          const r = barRuns(d);
          // A WebGL surface hands back an empty buffer unless it is read in the
          // same task that drew it, and an empty buffer reads as "no bar", which
          // a test would happily pass on. Counting the blank ones is what stops
          // this harness from measuring nothing and calling it a clean screen.
          let lit = 0;
          for (let i = 0; i < d.length; i += 4) if (d[i + 3] > 0) lit++;
          out.push({ ...r, lit });
        };
        window.__adxOnPresent(look);
        setTimeout(() => { window.__adxOnPresent(null); resolve(out); }, 1500);
      }));

      const compositor = await page.evaluate(
        () => document.querySelector('#screen').dataset.compositor);
      const torn = seen.filter((s) => s.runs > 1 ||
        (s.runs === 1 && s.spread > s.barW * 3));
      // If most presentations read back blank, the harness is looking at nothing
      // and a pass would mean nothing.
      const blank = seen.filter((s) => s.lit === 0).length;
      const unreadable = blank > seen.length / 2;
      if (torn.length || unreadable || errors.length) failures++;

      console.log(
        `round ${round}: compositor=${compositor}${force2d ? ' (forced 2d)' : ''} presentations=${seen.length} ` +
        `torn=${torn.length} blank=${blank}` +
        (unreadable ? ' UNREADABLE' : ''),
      );
      for (const t of torn.slice(0, 3)) console.log('   BAD', JSON.stringify(t));

      await ctx.close();
    }
  } finally {
    await browser.close();
    mock.kill();
  }

  if (failures) {
    console.log(`\nFAIL: ${failures} round(s) showed a screen that was not one moment.`);
    process.exit(1);
  }
  console.log('\nPASS: every presented screen held exactly one block.');
})().catch((e) => {
  console.log('harness error:', e.message);
  process.exit(2);
});
