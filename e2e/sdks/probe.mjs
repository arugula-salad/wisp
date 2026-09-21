// Usage: node probe.mjs <baseURL> <token> <sprite>
const [baseURL, token, name] = process.argv.slice(2);
const urls = [];
const Orig = globalThis.WebSocket;
globalThis.WebSocket = class extends Orig { constructor(u, ...a) { urls.push(new URL(String(u)).pathname.split('/').pop()); super(u, ...a); } };
const { SpritesClient } = await import(process.env.SDK + '/dist/index.js');
const withTimeout = (p, ms, what) => Promise.race([p, new Promise((_, r) => setTimeout(() => r(new Error(`HUNG: ${what} still pending after ${ms}ms`)), ms))]);
const tally = () => { const t = {}; for (const u of urls.splice(0)) t[u] = (t[u] || 0) + 1; return JSON.stringify(t); };

for (const controlMode of [false, true]) {
  const sprite = new SpritesClient(token, { baseURL, controlMode }).sprite(name);
  const label = `controlMode=${controlMode}`;
  try {
    for (let i = 0; i < 10; i++) {
      const r = await withTimeout(sprite.execFile('sh', ['-c', `echo seq-${i}`]), 8000, `exec ${i}`);
      if (r.stdout.trim() !== `seq-${i}`) throw new Error(`wrong output ${JSON.stringify(r.stdout)}`);
    }
    const many = await withTimeout(Promise.all([...Array(15).keys()].map(i => sprite.execFile('sh', ['-c', `sleep 0.2; echo par-${i}`]))), 20000, '15 concurrent execs');
    if (!many.every((r, i) => r.stdout.trim() === `par-${i}`)) throw new Error('concurrent outputs crossed');
    let code = 0; try { await sprite.execFile('sh', ['-c', 'exit 7']); } catch (e) { code = e.result?.exitCode ?? e.exitCode ?? -1; }
    console.log(`[js] ${label}: 10 sequential + 15 concurrent execs OK, exit code propagated=${code}; sockets opened: ${tally()}`);
  } catch (e) { console.log(`[js] ${label}: EXEC FAILED: ${e.message}; sockets: ${tally()}`); }

  try {
    const sess = await withTimeout(sprite.proxyPort(18990 + (controlMode ? 1 : 0), 8080), 8000, 'proxyPort');
    let ok = 0;
    for (let i = 0; i < 10; i++) {
      const body = await withTimeout(fetch(`http://127.0.0.1:${sess.localPort}/`, { headers: { connection: 'close' } }).then(r => r.text()), 5000, `GET ${i} through proxy`);
      if (body.trim() === 'proxied-ok') ok++;
    }
    sess.close();
    console.log(`[js] ${label}: proxyPort ${ok}/10 requests OK; sockets opened: ${tally()}`);
  } catch (e) { console.log(`[js] ${label}: PROXY FAILED: ${e.message}; sockets: ${tally()}`); }

  // The VM is restored out from under a running exec: the client must find out, not hang.
  try {
    const cs = await sprite.createCheckpoint('probe'); for await (const _ of cs) { /* drain */ }
    const cps = await sprite.listCheckpoints();
    const running = sprite.execFile('sleep', ['300']).then(() => 'exited 0?!', e => `ended: ${String(e.message).slice(0, 70)}`);
    await new Promise(r => setTimeout(r, 700));
    const rs = await sprite.restoreCheckpoint(cps[0].id); for await (const _ of rs) { /* drain */ }
    const t0 = Date.now();
    const how = await withTimeout(running, 15000, 'exec after its VM was restored away');
    console.log(`[js] ${label}: exec under a restored VM -> ${how} (after ${Date.now() - t0}ms); sockets: ${tally()}`);
  } catch (e) { console.log(`[js] ${label}: VANISH: ${e.message}; sockets: ${tally()}`); }
}
process.exit(0);
