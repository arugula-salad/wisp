// E2B conformance probe for the official JS SDK (`e2b`, pinned in package.json).
//
// Usage: node probe.mjs   (target from the SDK's own env vars; see run.sh and probe.py
// for the E2B_PROBE_* knobs, which mean the same thing here).
// Prints one "[js] <step>: OK|FAILED|SKIP ..." line per step. Exit status = failures.
import http from 'node:http'
import https from 'node:https'
import { createRequire } from 'node:module'
import { Sandbox, CommandExitError, NotFoundError } from 'e2b'

const sdkVersion = createRequire(import.meta.url)('e2b/package.json').version
const RUN = process.env.E2B_PROBE_RUN || `js-${process.pid}`
const META = { probe: 'wisp-e2b', run: RUN }
let fails = 0
let sbx
const sleep = (ms) => new Promise((r) => setTimeout(r, ms))
class Skip extends Error {}
const check = (cond, what) => {
  if (!cond) throw new Error(`assertion: ${typeof what === 'string' ? what : JSON.stringify(what)}`)
}

async function step(name, fn) {
  const t0 = Date.now()
  try {
    const msg = await fn()
    console.log(`[js] ${name}: OK (${Date.now() - t0}ms)${msg ? ' ' + msg : ''}`)
  } catch (e) {
    if (e instanceof Skip) return console.log(`[js] ${name}: SKIP ${e.message}`)
    fails++
    console.log(`[js] ${name}: FAILED ${e?.constructor?.name}: ${e?.message}`)
  }
}

// Plain HTTP request, either direct or through E2B_PROBE_PORT_VIA with an explicit Host.
function request(url, { host, method = 'GET', body, headers = {} } = {}) {
  const via = process.env.E2B_PROBE_PORT_VIA
  const u = new URL(url)
  let mod, opts
  if (via && host) {
    const v = new URL(via)
    mod = http
    opts = { hostname: v.hostname, port: v.port, path: u.pathname + u.search, headers: { ...headers, Host: host } }
  } else {
    mod = u.protocol === 'https:' ? https : http
    opts = { hostname: u.hostname, port: u.port, path: u.pathname + u.search, headers }
  }
  if (body) opts.headers['Content-Length'] = Buffer.byteLength(body)
  return new Promise((resolve, reject) => {
    const req = mod.request({ ...opts, method, timeout: 20000 }, (res) => {
      const chunks = []
      res.on('data', (c) => chunks.push(c))
      res.on('end', () => resolve({ status: res.statusCode, data: Buffer.concat(chunks) }))
    })
    req.on('error', reject)
    req.on('timeout', () => req.destroy(new Error('timeout')))
    if (body) req.write(body)
    req.end()
  })
}

async function create() {
  sbx = await Sandbox.create({ timeoutMs: 300_000, metadata: META, envs: { PROBE_ENV: 'from-create' } })
  const info = await sbx.getInfo()
  return `id=${sbx.sandboxId} template=${info.templateId} name=${info.name} envd=${info.envdVersion} domain=${sbx.sandboxDomain} cpu=${info.cpuCount} mem=${info.memoryMB}`
}

async function execStream() {
  const out = [], err = [], times = []
  const r = await sbx.commands.run(
    'for i in 1 2 3; do echo out-$i; echo err-$i >&2; sleep 0.4; done; echo $PROBE_ENV; whoami; pwd',
    { onStdout: (s) => { out.push(s); times.push(Date.now()) }, onStderr: (s) => err.push(s) }
  )
  check(r.exitCode === 0, `exit ${r.exitCode}`)
  check(r.stdout.includes('out-3') && r.stderr.includes('err-2'), r)
  check(r.stdout.includes('from-create'), 'create-time env var missing')
  check(times.length >= 2 && times.at(-1) - times[0] > 500, `stdout not streamed: ${times.length} callbacks`)
  const lines = r.stdout.trim().split('\n')
  return `${out.length} stdout / ${err.length} stderr callbacks over ${((times.at(-1) - times[0]) / 1000).toFixed(1)}s; user=${lines.at(-2)} cwd=${lines.at(-1)}`
}

async function execExitCode() {
  try {
    await sbx.commands.run('echo partial; echo bad >&2; exit 7')
    throw new Error('no CommandExitError for exit 7')
  } catch (e) {
    if (!(e instanceof CommandExitError)) throw e
    check(e.exitCode === 7 && e.stdout.includes('partial') && e.stderr.includes('bad'), { code: e.exitCode })
  }
  const r = await sbx.commands.run('pwd; echo $FOO', { cwd: '/tmp', envs: { FOO: 'bar' }, user: 'root' })
  check(r.stdout.split(/\s+/).filter(Boolean).join(',') === '/tmp,bar', r.stdout)
  return 'exit 7 -> CommandExitError; cwd/envs/user honoured'
}

async function background() {
  const h = await sbx.commands.run('echo started; sleep 600', { background: true })
  await sleep(1000)
  const procs = await sbx.commands.list()
  const mine = procs.find((p) => p.pid === h.pid)
  check(mine, `pid ${h.pid} not listed`)
  check(await sbx.commands.kill(h.pid), 'kill returned false')
  let code
  try {
    await h.wait()
    throw new Error('killed process exited 0')
  } catch (e) {
    if (!(e instanceof CommandExitError)) throw e
    code = e.exitCode
  }
  check(!(await sbx.commands.list()).some((p) => p.pid === h.pid), 'still listed after kill')
  check((await sbx.commands.kill(h.pid)) === false, 'second kill should be false')
  return `pid=${h.pid} cmd=${mine.cmd} args=${JSON.stringify(mine.args)} -> killed, exitCode=${code}`
}

async function stdinPty() {
  // stdin + reconnect (Process.Connect) + CloseStdin
  const h = await sbx.commands.run('cat', { background: true, stdin: true })
  await sbx.commands.sendStdin(h.pid, 'line-1\n')
  const h2 = await sbx.commands.connect(h.pid)
  await sbx.commands.sendStdin(h.pid, 'line-2\n')
  await sbx.commands.closeStdin(h.pid)
  const r = await h2.wait()
  check(r.stdout === 'line-1\nline-2\n' || r.stdout === 'line-2\n', `cat via reconnect saw ${JSON.stringify(r.stdout)}`)
  // PTY: Start with pty, SendInput(pty), Update (resize), output comes back as pty bytes
  let out = ''
  const p = await sbx.pty.create({ cols: 80, rows: 24, onData: (d) => { out += new TextDecoder().decode(d) } })
  await sbx.pty.resize(p.pid, { cols: 100, rows: 30 })
  await sbx.pty.sendInput(p.pid, new TextEncoder().encode('stty size; echo pty-$((40+2))\nexit\n'))
  await p.wait()
  check(out.includes('pty-42') && out.includes('30 100'), out.slice(-200))
  return `stdin+reconnect saw ${JSON.stringify(r.stdout)}; pty resized to 30x100 and echoed`
}

async function files() {
  const base = `/tmp/probe-${RUN}`
  const w = await sbx.files.write(`${base}/a/hello.txt`, 'hello e2b\n')
  check(w.path === `${base}/a/hello.txt`, w)
  check((await sbx.files.read(`${base}/a/hello.txt`)) === 'hello e2b\n', 'read mismatch')
  check((await sbx.files.makeDir(`${base}/dir`)) === true, 'makeDir')
  check((await sbx.files.makeDir(`${base}/dir`)) === false, 'makeDir twice should be false')
  const names = (await sbx.files.list(base)).map((e) => e.name).sort()
  check(names.join() === 'a,dir', names)
  const deep = (await sbx.files.list(base, { depth: 2 })).map((e) => e.path)
  check(deep.includes(`${base}/a/hello.txt`), deep)
  const info = await sbx.files.getInfo(`${base}/a/hello.txt`)
  check(info.size === 10 && info.type === 'file', info)
  await sbx.files.rename(`${base}/a/hello.txt`, `${base}/dir/moved.txt`)
  check(!(await sbx.files.exists(`${base}/a/hello.txt`)) && (await sbx.files.exists(`${base}/dir/moved.txt`)), 'rename')
  await sbx.files.remove(`${base}/dir/moved.txt`)
  check(!(await sbx.files.exists(`${base}/dir/moved.txt`)), 'remove')
  try {
    await sbx.files.read(`${base}/nope`)
    throw new Error('read of missing file succeeded')
  } catch (e) {
    if (!(e instanceof NotFoundError)) throw e
  }
  const rel = await sbx.files.write('probe-rel.txt', 'rel')
  return `write/read/list(depth 1,2)/stat/rename/remove OK; mode=${info.mode?.toString(8)} owner=${info.owner}; relative path resolved to ${rel.path}`
}

async function uploadDownload() {
  const blob = new Uint8Array(256 * 64).map((_, i) => i % 256)
  await sbx.files.write(`/tmp/probe-${RUN}/blob.bin`, blob.buffer)
  const got = await sbx.files.read(`/tmp/probe-${RUN}/blob.bin`, { format: 'bytes' })
  check(Buffer.compare(Buffer.from(got), Buffer.from(blob)) === 0, 'binary roundtrip mismatch')
  // Streamed upload takes the application/octet-stream path.
  const stream = new ReadableStream({ start(c) { c.enqueue(new TextEncoder().encode('streamed-body')); c.close() } })
  await sbx.files.write(`/tmp/probe-${RUN}/streamed.txt`, stream)
  check((await sbx.files.read(`/tmp/probe-${RUN}/streamed.txt`)) === 'streamed-body', 'stream upload mismatch')
  const up = await sbx.uploadUrl(`/tmp/probe-${RUN}/via-url.txt`, { useSignatureExpiration: 120 })
  const down = await sbx.downloadUrl(`/tmp/probe-${RUN}/via-url.txt`, { useSignatureExpiration: 120 })
  const boundary = 'probeboundary'
  const body = `--${boundary}\r\nContent-Disposition: form-data; name="file"; filename="via-url.txt"\r\nContent-Type: application/octet-stream\r\n\r\nuploaded-by-url\r\n--${boundary}--\r\n`
  let r = await request(up, { method: 'POST', body, headers: { 'Content-Type': `multipart/form-data; boundary=${boundary}` } })
  check(r.status === 200, `upload_url POST -> ${r.status} ${r.data.toString().slice(0, 200)}`)
  r = await request(down)
  check(r.status === 200 && r.data.toString() === 'uploaded-by-url', `download_url GET -> ${r.status} ${r.data.toString().slice(0, 200)}`)
  return `${blob.length} byte binary roundtrip; streamed (octet-stream) upload; signed upload/download URLs OK`
}

async function port() {
  await sbx.files.write('/tmp/www/index.html', 'port-ok\n')
  await sbx.commands.run('cd /tmp/www && python3 -m http.server 8080', { background: true })
  const host = sbx.getHost(8080)
  const scheme = process.env.E2B_PROBE_PORT_SCHEME || 'https'
  let last
  for (let i = 0; i < 30; i++) {
    try {
      const r = await request(`${scheme}://${host}/`, { host })
      last = `${r.status} ${r.data.toString().slice(0, 120)}`
      if (r.status === 200 && r.data.toString() === 'port-ok\n') return `GET ${scheme}://${host}/ -> 200`
    } catch (e) {
      last = String(e)
    }
    await sleep(1000)
  }
  throw new Error(`port 8080 never served: ${last}`)
}

async function setTimeoutStep() {
  await sbx.setTimeout(600_000)
  const left = ((await sbx.getInfo()).endAt.getTime() - Date.now()) / 1000
  check(left > 550 && left < 650, `endAt ${left.toFixed(0)}s away, wanted ~600`)
  return `endAt moved to now+${left.toFixed(0)}s`
}

async function metrics() {
  const m = await sbx.getMetrics()
  return `${m.length} samples` + (m.length ? `, last cpu=${m.at(-1).cpuUsedPct}% mem=${m.at(-1).memUsed}` : '')
}

async function listing() {
  const items = await Sandbox.list({ query: { metadata: { run: RUN } } }).nextItems()
  const s = items.find((x) => x.sandboxId === sbx.sandboxId)
  check(s, `${sbx.sandboxId} not in ${items.map((x) => x.sandboxId)}`)
  check(s.metadata.probe === 'wisp-e2b', s.metadata)
  return `found by metadata; state=${s.state}`
}

async function pauseResume() {
  if (process.env.E2B_PROBE_PAUSE === '0') throw new Skip('E2B_PROBE_PAUSE=0')
  await sbx.files.write('/tmp/before-pause.txt', 'kept')
  const h = await sbx.commands.run('sleep 900', { background: true })
  check((await sbx.pause()) === true, 'pause returned false')
  const st = (await sbx.getInfo()).state
  check(st === 'paused', `state ${st}`)
  const again = await sbx.pause()
  await sbx.connect()
  const st2 = (await sbx.getInfo()).state
  check((await sbx.files.read('/tmp/before-pause.txt')) === 'kept', 'file lost across pause')
  const alive = (await sbx.commands.list()).some((p) => p.pid === h.pid)
  const r = await sbx.commands.run('echo resumed')
  check(r.stdout.trim() === 'resumed', r.stdout)
  await sbx.commands.kill(h.pid)
  return `paused (state=${st}), second pause -> ${again}, connect resumed (state=${st2}), files kept, process survived=${alive}`
}

async function kill() {
  check((await sbx.kill()) === true, 'kill returned false')
  try {
    await sbx.getInfo()
    throw new Error('getInfo after kill succeeded')
  } catch (e) {
    if (!(e instanceof NotFoundError)) throw e
  }
  check((await Sandbox.kill(sbx.sandboxId)) === false, 'second kill should be false')
  let msg
  try {
    await Sandbox.connect(sbx.sandboxId)
    throw new Error('connect after kill succeeded')
  } catch (e) {
    if (!(e instanceof NotFoundError)) throw e
    msg = e.message
  }
  return `killed; getInfo -> NotFound; kill again -> false; connect -> ${msg.slice(0, 80)}`
}

console.log(`[js] sdk e2b@${sdkVersion} api=${process.env.E2B_API_URL || 'https://api.' + (process.env.E2B_DOMAIN || 'e2b.app')} sandbox_url=${process.env.E2B_SANDBOX_URL || '(default)'}`)
try {
  await step('create', create)
  if (sbx) {
    for (const [name, fn] of [
      ['exec_stream', execStream], ['exec_exit_code', execExitCode], ['background_kill', background],
      ['stdin_pty', stdinPty], ['files', files], ['upload_download', uploadDownload], ['port', port],
      ['set_timeout', setTimeoutStep], ['metrics', metrics], ['list', listing],
      ['pause_resume', pauseResume], ['kill', kill],
    ]) await step(name, fn)
  }
} finally {
  if (sbx) await Sandbox.kill(sbx.sandboxId).catch((e) => console.log(`[js] cleanup kill: ${e}`))
}
console.log(`[js] == ${fails} failure(s)`)
process.exit(fails)
