// Probes the official @vercel/sandbox SDK (pinned in package.json), unmodified.
//
//   node --import ./target.mjs probe.mjs
//
// Credentials come from VERCEL_TOKEN, VERCEL_TEAM_ID and VERCEL_PROJECT_ID (the JS
// SDK only reads VERCEL_OIDC_TOKEN from the environment by itself, so they are
// passed explicitly, the way a non-Vercel app would). The target server is picked
// by target.mjs from VERCEL_SANDBOX_URL. Every sandbox is non-persistent, stopped
// and deleted before exit; any snapshot taken is deleted too.
//
// Prints one "[js] <step>: OK|FAILED ..." line per step; run.sh counts FAILED.
import { Sandbox, Snapshot, APIError } from '@vercel/sandbox';
import { Writable } from 'node:stream';

const creds = {
  token: process.env.VERCEL_TOKEN,
  teamId: process.env.VERCEL_TEAM_ID,
  projectId: process.env.VERCEL_PROJECT_ID,
};
const prefix = `wisp-probe-js-${Date.now().toString(36)}`;
const created = []; // sandboxes to stop and delete in finally
const snapshots = [];
let failures = 0;

const log = (s) => console.log(`[js] ${s}`);
async function step(name, fn) {
  const t0 = Date.now();
  try {
    const detail = await fn();
    log(`${name}: OK (${Date.now() - t0}ms)${detail ? ' ' + detail : ''}`);
  } catch (e) {
    failures++;
    const extra = e instanceof APIError ? ` status=${e.response?.status} body=${JSON.stringify(e.json ?? e.text)}` : '';
    log(`${name}: FAILED: ${e.message}${extra}`);
  }
}
const must = (cond, msg) => { if (!cond) throw new Error(msg); };
const collector = () => { let s = ''; const w = new Writable({ write(c, _e, cb) { s += c; cb(); } }); w.text = () => s; return w; };
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

let sbx;
try {
  await step('create', async () => {
    sbx = await Sandbox.create({
      ...creds, name: `${prefix}-a`, persistent: false, ports: [3000],
      timeout: 4 * 60 * 1000, resources: { vcpus: 1 }, tags: { probe: 'wisp' },
    });
    created.push(sbx);
    must(sbx.status === 'running' || sbx.status === 'pending', `status ${sbx.status}`);
    return `name=${sbx.name} session=${sbx.currentSession().sessionId} status=${sbx.status} cwd=${sbx.cwd} vcpus=${sbx.vcpus} memory=${sbx.memory} region=${sbx.region} routes=${JSON.stringify(sbx.routes)}`;
  });
  if (!sbx) throw new Error('no sandbox; stopping');

  await step('environment', async () => {
    const r = await sbx.runCommand('sh', ['-c', '. /etc/os-release; echo "$PRETTY_NAME"; id; echo "HOME=$HOME"; pwd; command -v node python3; uname -r']);
    return JSON.stringify(r.exitCode) + ' ' + JSON.stringify((await r.stdout()).trim().split('\n'));
  });

  await step('runCommand waited (streamed logs, exit code)', async () => {
    const out = collector(), err = collector();
    const r = await sbx.runCommand({ cmd: 'sh', args: ['-c', 'echo out1; echo err1 >&2; sleep 1; echo out2; exit 3'], stdout: out, stderr: err });
    must(r.exitCode === 3, `exitCode ${r.exitCode}`);
    must(out.text() === 'out1\nout2\n', `stdout ${JSON.stringify(out.text())}`);
    must(err.text() === 'err1\n', `stderr ${JSON.stringify(err.text())}`);
    must((await r.output('both')).includes('out2'), 'output(both)');
    return `cmdId=${r.cmdId} exit=${r.exitCode} durationMs=${r.durationMs}`;
  });

  await step('runCommand env + cwd + sudo', async () => {
    const r = await sbx.runCommand({ cmd: 'sh', args: ['-c', 'echo "$FOO"; pwd; id -u'], env: { FOO: 'bar' }, cwd: '/tmp' });
    const lines = (await r.stdout()).trim().split('\n');
    must(lines[0] === 'bar' && lines[1] === '/tmp', JSON.stringify(lines));
    const s = await sbx.runCommand({ cmd: 'id', args: ['-u'], sudo: true });
    must((await s.stdout()).trim() === '0', 'sudo is not root');
    return `uid=${lines[2]} sudo uid=0`;
  });

  await step('runCommand missing binary', async () => {
    try {
      const r = await sbx.runCommand('definitely-not-a-binary');
      return `exit=${r.exitCode} stderr=${JSON.stringify(await r.stderr())}`;
    } catch (e) {
      return `throws ${e.constructor.name} status=${e.response?.status} body=${JSON.stringify(e.json ?? e.message)}`;
    }
  });

  await step('detached command + logs + kill + wait', async () => {
    const cmd = await sbx.runCommand({ cmd: 'sh', args: ['-c', 'trap "echo got-term; exit 42" TERM; i=0; while true; do i=$((i+1)); echo tick $i; sleep 0.5; done'], detached: true });
    must(cmd.exitCode === null, `detached exitCode ${cmd.exitCode}`);
    const seen = [];
    const logs = cmd.logs();
    for await (const l of logs) { seen.push(l.data.trim()); if (seen.length >= 2) break; }
    const again = await sbx.getCommand(cmd.cmdId);
    must(again.exitCode === null, 'getCommand says finished');
    await cmd.kill('SIGTERM');
    const done = await cmd.wait();
    return `cmdId=${cmd.cmdId} logs=${JSON.stringify(seen)} exitAfterSIGTERM=${done.exitCode}`;
  });

  await step('detached kill SIGKILL', async () => {
    const cmd = await sbx.runCommand({ cmd: 'sleep', args: ['600'], detached: true });
    await cmd.kill('SIGKILL');
    const done = await cmd.wait();
    return `exit=${done.exitCode}`;
  });

  await step('writeFiles + readFile', async () => {
    await sbx.writeFiles([
      { path: 'probe/hello.txt', content: Buffer.from('hello from wisp\n') },
      { path: '/tmp/probe-abs/bin.dat', content: new Uint8Array([0, 1, 2, 255]) },
      { path: 'probe/run.sh', content: '#!/bin/sh\necho ran\n', mode: 0o755 },
    ]);
    const buf = await sbx.readFileToBuffer({ path: 'probe/hello.txt' });
    must(buf?.toString() === 'hello from wisp\n', `read ${JSON.stringify(buf?.toString())}`);
    const bin = await sbx.readFileToBuffer({ path: '/tmp/probe-abs/bin.dat' });
    must(bin && Buffer.compare(bin, Buffer.from([0, 1, 2, 255])) === 0, 'binary roundtrip');
    const r = await sbx.runCommand('sh', ['-c', `${sbx.cwd}/probe/run.sh; stat -c '%a %U' ${sbx.cwd}/probe/hello.txt ${sbx.cwd}/probe/run.sh`]);
    const missing = await sbx.readFileToBuffer({ path: 'probe/nope.txt' });
    must(missing === null, 'missing file should read as null');
    return `exec+stat=${JSON.stringify((await r.stdout()).trim().split('\n'))} missing=null`;
  });

  await step('mkDir', async () => {
    await sbx.mkDir('probe-dir');
    await sbx.mkDir('/tmp/probe-dir-abs');
    const r = await sbx.runCommand('sh', ['-c', `test -d ${sbx.cwd}/probe-dir && test -d /tmp/probe-dir-abs && echo yes`]);
    must((await r.stdout()).trim() === 'yes', 'dirs missing');
    let again = 'ok';
    try { await sbx.mkDir('probe-dir'); } catch (e) { again = `status=${e.response?.status} body=${JSON.stringify(e.json)}`; }
    let nested = 'ok';
    try { await sbx.mkDir('no/such/parent'); } catch (e) { nested = `status=${e.response?.status} body=${JSON.stringify(e.json)}`; }
    return `existing->${again} nested->${nested}`;
  });

  await step('port via domain()', async () => {
    await sbx.writeFiles([{ path: 'www/index.html', content: 'served-by-sandbox\n' }]);
    await sbx.runCommand({ cmd: 'python3', args: ['-m', 'http.server', '3000', '--directory', `${sbx.cwd}/www`], detached: true });
    const url = sbx.domain(3000);
    let last = '';
    for (let i = 0; i < 30; i++) {
      try {
        const res = await fetch(url);
        const body = await res.text();
        if (res.status === 200 && body === 'served-by-sandbox\n') return `url=${url.replace(/^https:\/\/[^.]+/, 'https://<subdomain>')} status=200`;
        last = `status=${res.status} body=${JSON.stringify(body.slice(0, 120))}`;
      } catch (e) { last = e.message; }
      await sleep(1000);
    }
    throw new Error(`never served: ${last}`);
  });

  await step('domain() for unexposed port', async () => {
    try { sbx.domain(4000); return 'no error?!'; } catch (e) { return `throws "${e.message}"`; }
  });

  await step('extendTimeout', async () => {
    const before = sbx.currentSession().timeout;
    await sbx.extendTimeout(60 * 1000);
    const after = sbx.currentSession().timeout;
    must(after === before + 60000, `timeout ${before} -> ${after}`);
    return `timeout ${before} -> ${after}`;
  });

  await step('get + list', async () => {
    const got = await Sandbox.get({ ...creds, name: sbx.name });
    must(got.name === sbx.name, 'get returned another sandbox');
    const page = await Sandbox.list({ ...creds, namePrefix: prefix, sortBy: 'name', limit: 10 });
    const names = page.sandboxes.map((s) => `${s.name}:${s.status}`);
    must(names.some((n) => n.startsWith(sbx.name + ':')), `not listed: ${names}`);
    return `get.status=${got.status} list=${JSON.stringify(names)} pagination=${JSON.stringify(page.pagination)}`;
  });

  await step('get missing sandbox', async () => {
    try { await Sandbox.get({ ...creds, name: `${prefix}-missing` }); return 'no error?!'; } catch (e) {
      must(e instanceof APIError, e.message);
      return `status=${e.response.status} body=${JSON.stringify(e.json)}`;
    }
  });

  let snap;
  await step('snapshot (stops the session)', async () => {
    snap = await sbx.snapshot({ expiration: 24 * 60 * 60 * 1000 }); // the API's minimum non-zero expiration
    snapshots.push(snap);
    return `snapshotId=${snap.snapshotId} status=${snap.status} sizeBytes=${snap.sizeBytes} sandbox.status=${sbx.status}`;
  });

  if (snap) {
    await step('create from snapshot', async () => {
      const b = await Sandbox.create({ ...creds, name: `${prefix}-b`, persistent: false, source: { type: 'snapshot', snapshotId: snap.snapshotId }, timeout: 2 * 60 * 1000, resources: { vcpus: 1 } });
      created.push(b);
      const buf = await b.readFileToBuffer({ path: 'probe/hello.txt' });
      must(buf?.toString() === 'hello from wisp\n', 'snapshot lost the file');
      const st = await b.stop();
      return `name=${b.name} sourceSnapshotId=${b.currentSession().sourceSnapshotId} stop.status=${st.status}`;
    });
  }

  await step('stop', async () => {
    const st = await sbx.stop();
    return `status=${st.status} snapshot=${st.snapshot ? st.snapshot.id : 'none'}`;
  });
} finally {
  for (const s of snapshots) {
    try { await s.delete(); log(`cleanup: deleted snapshot ${s.snapshotId}`); } catch (e) { log(`cleanup: snapshot delete FAILED: ${e.message}`); failures++; }
  }
  for (const s of created) {
    try { if (s.status === 'running' || s.status === 'pending') await s.stop(); } catch (e) { log(`cleanup: stop ${s.name}: ${e.message}`); }
    try { await s.delete(); log(`cleanup: deleted ${s.name}`); } catch (e) { log(`cleanup: delete ${s.name} FAILED: ${e.message}`); failures++; }
  }
  try {
    const left = (await Sandbox.list({ ...creds, namePrefix: prefix, sortBy: 'name' })).sandboxes.filter((s) => s.status === 'running' || s.status === 'pending');
    log(left.length ? `cleanup: FAILED: still running ${left.map((s) => s.name)}` : 'cleanup: none left running: OK');
    if (left.length) failures++;
  } catch (e) { log(`cleanup: list FAILED: ${e.message}`); failures++; }
}
log(`${failures} failure(s)`);
process.exit(failures ? 1 : 0);
