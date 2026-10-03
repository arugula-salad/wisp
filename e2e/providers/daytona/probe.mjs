// Daytona conformance probe for the official TypeScript SDK (@daytonaio/sdk,
// pinned in package.json), through its public API only.
//
// Usage: node probe.mjs   (target from the SDK's own env: DAYTONA_API_URL and
//   DAYTONA_API_KEY; see run.sh)
// Probe-only knobs: DAYTONA_PROBE_RUN (a label value sweep.py finds leaks by),
//   DAYTONA_PROBE_STOP=0 (skip stop/start).
// Prints one "[js] <step>: OK|FAILED|SKIP ..." line per step. Exit status = failures.
import http from 'node:http'
import https from 'node:https'
import { Daytona, DaytonaError, DaytonaNotFoundError } from '@daytonaio/sdk'

const RUN = process.env.DAYTONA_PROBE_RUN || `js-${process.pid}`
const LABELS = { probe: 'wisp-daytona', run: RUN }
let fails = 0

class Skip extends Error {}

async function step(name, fn) {
  const t0 = Date.now()
  try {
    const msg = await fn()
    console.log(`[js] ${name}: OK (${Date.now() - t0}ms)${msg ? ' ' + msg : ''}`)
  } catch (e) {
    if (e instanceof Skip) {
      console.log(`[js] ${name}: SKIP ${e.message}`)
      return
    }
    fails++
    console.log(`[js] ${name}: FAILED ${e?.constructor?.name}: ${e?.message}`)
    console.log(String(e?.stack || '').split('\n').slice(1, 5).join('\n'))
  }
}

function check(cond, what) {
  if (!cond) throw new Error(`assertion failed: ${what}`)
}

async function expectError(cls, fn, what) {
  try {
    await fn()
  } catch (e) {
    if (e instanceof cls) return e
    throw new Error(`${what}: got ${e?.constructor?.name}: ${e?.message}, want ${cls.name}`)
  }
  throw new Error(`${what}: succeeded`)
}

// GET a preview URL. Its host (<port>-<id>.daytona.localhost) need not resolve:
// connect to the API's address and send the preview host as Host.
function httpGet(url, headers = {}) {
  const u = new URL(url)
  const api = new URL(process.env.DAYTONA_API_URL || 'http://127.0.0.1')
  const host = u.hostname.endsWith('.localhost') ? api.hostname : u.hostname
  // A public server's preview URL is https:// (as probe.py does).
  const client = u.protocol === 'https:' ? https : http
  return new Promise((resolve, reject) => {
    const req = client.request(
      { host, port: u.port || (u.hostname.endsWith('.localhost') ? api.port : undefined), servername: u.hostname, path: u.pathname + u.search, headers: { ...headers, Host: u.host } },
      (res) => {
        const chunks = []
        res.on('data', (c) => chunks.push(c))
        res.on('end', () => resolve({ status: res.statusCode, body: Buffer.concat(chunks).toString() }))
      },
    )
    req.on('error', reject)
    req.setTimeout(20000, () => req.destroy(new Error('timeout')))
    req.end()
  })
}

const sleep = (ms) => new Promise((r) => setTimeout(r, ms))
const daytona = new Daytona()
let sbx

await step('create', async () => {
  sbx = await daytona.create(
    { language: 'typescript', labels: LABELS, envVars: { PROBE_ENV: 'from-create' }, autoStopInterval: 30 },
    { timeout: 120 },
  )
  check(sbx.id, 'no sandbox id')
  check(sbx.state === 'started', `state ${sbx.state}`)
  check(sbx.labels['code-toolbox-language'] === 'typescript', `labels ${JSON.stringify(sbx.labels)}`)
  check(sbx.autoStopInterval === 30, `autoStopInterval ${sbx.autoStopInterval}`)
  return `id=${sbx.id} user=${sbx.user} cpu=${sbx.cpu} mem=${sbx.memory} disk=${sbx.disk}`
})

if (sbx) {
  await step('get_list', async () => {
    const got = await daytona.get(sbx.id)
    check(got.id === sbx.id && got.state === 'started', `get ${got.state}`)
    check((await daytona.get(sbx.name)).id === sbx.id, 'get by name')
    const ids = []
    for await (const s of daytona.list({ labels: { run: RUN } })) ids.push(s.id)
    check(JSON.stringify(ids) === JSON.stringify([sbx.id]), `list by label ${ids}`)
    await expectError(DaytonaNotFoundError, () => daytona.get('00000000-0000-4000-8000-000000000000'), 'get missing')
    return `listed ${ids.length}`
  })

  await step('labels_autostop', async () => {
    const out = await sbx.setLabels({ ...LABELS, 'code-toolbox-language': 'typescript', extra: 'yes' })
    check(out.extra === 'yes', `setLabels ${JSON.stringify(out)}`)
    await sbx.setAutostopInterval(45)
    await sbx.refreshData()
    check(sbx.labels.extra === 'yes' && sbx.autoStopInterval === 45, `after refresh ${sbx.autoStopInterval}`)
  })

  await step('info', async () => {
    const home = await sbx.getUserHomeDir()
    const wd = await sbx.getWorkDir()
    check(home === '/home/daytona' && wd === '/home/daytona', `home=${home} wd=${wd}`)
    const r = await sbx.process.executeCommand('whoami && echo $HOME && pwd && echo $PROBE_ENV')
    check(r.exitCode === 0, `exit ${r.exitCode}`)
    check(r.result.split(/\s+/).filter(Boolean).join(' ') === 'daytona /home/daytona /home/daytona from-create', `result ${JSON.stringify(r.result)}`)
  })

  await step('exec', async () => {
    let r = await sbx.process.executeCommand('echo hello; echo oops >&2; exit 3')
    check(r.exitCode === 3 && r.result.includes('hello'), `exit ${r.exitCode} ${JSON.stringify(r.result)}`)
    r = await sbx.process.executeCommand('pwd; echo $GREETING', '/tmp', { GREETING: 'hi there' })
    check(r.result.split('\n').slice(0, 2).join('|') === '/tmp|hi there', `cwd/env ${JSON.stringify(r.result)}`)
    r = await sbx.process.executeCommand("echo 'quoted | piped' | tr a-z A-Z")
    check(r.result.trim() === 'QUOTED | PIPED', `shell syntax ${JSON.stringify(r.result)}`)
    const t0 = Date.now()
    const e = await expectError(DaytonaError, () => sbx.process.executeCommand('sleep 30', undefined, undefined, 2), 'timeout')
    check(Date.now() - t0 < 15000, `timeout took ${Date.now() - t0}ms`)
    return `timeout -> ${e.constructor.name}`
  })

  await step('code_run', async () => {
    let r = await sbx.process.codeRun(
      'const x: number = 10, y: number = 20\nconsole.log(`Sum: ${x + y}`)\nconsole.log(JSON.stringify(process.argv.slice(2)))',
      { argv: ['a', 'b c'], env: { X: '1' } },
    )
    check(r.exitCode === 0, `exit ${r.exitCode}: ${r.result}`)
    check(r.result.split('\n').slice(0, 2).join('|') === 'Sum: 30|["a","b c"]', `result ${JSON.stringify(r.result)}`)
    r = await sbx.process.codeRun('process.exit(4)')
    check(r.exitCode === 4, `exit code ${r.exitCode}`)
  })

  await step('sessions', async () => {
    const sid = 'probe-session'
    await sbx.process.createSession(sid)
    let r = await sbx.process.executeSessionCommand(sid, { command: 'cd /tmp && export FOO=bar' })
    check(r.exitCode === 0, `first ${r.exitCode}`)
    r = await sbx.process.executeSessionCommand(sid, { command: 'pwd; echo $FOO; echo to-stderr >&2' })
    check(r.stdout.split(/\s+/).filter(Boolean).join(' ') === '/tmp bar', `state carried over: ${JSON.stringify(r.stdout)}`)
    check(r.stderr.trim() === 'to-stderr', `stderr ${JSON.stringify(r.stderr)}`)
    r = await sbx.process.executeSessionCommand(sid, { command: 'exit 7' })
    check(r.exitCode === 7, `exit code ${r.exitCode}`)

    const cmd = 'for i in 1 2 3; do echo out$i; echo err$i >&2; sleep 0.3; done'
    r = await sbx.process.executeSessionCommand(sid, { command: cmd, runAsync: true })
    const cid = r.cmdId
    check(cid && r.exitCode == null, `async ${JSON.stringify(r)}`)
    const out = []
    const err = []
    await sbx.process.getSessionCommandLogs(sid, cid, (c) => out.push(c), (c) => err.push(c))
    check(out.join('').split(/\s+/).filter(Boolean).join(' ') === 'out1 out2 out3', `followed stdout ${JSON.stringify(out)}`)
    check(err.join('').split(/\s+/).filter(Boolean).join(' ') === 'err1 err2 err3', `followed stderr ${JSON.stringify(err)}`)
    const c = await sbx.process.getSessionCommand(sid, cid)
    check(c.exitCode === 0 && c.command === cmd, `command ${JSON.stringify(c)}`)
    const logs = await sbx.process.getSessionCommandLogs(sid, cid)
    check(logs.stdout.split(/\s+/).filter(Boolean).join(' ') === 'out1 out2 out3', `logs ${JSON.stringify(logs)}`)

    r = await sbx.process.executeSessionCommand(sid, { command: 'read line; echo got:$line', runAsync: true })
    await sleep(500)
    await sbx.process.sendSessionCommandInput(sid, r.cmdId, 'typed\n')
    let done
    for (let i = 0; i < 50; i++) {
      done = await sbx.process.getSessionCommand(sid, r.cmdId)
      if (done.exitCode != null) break
      await sleep(100)
    }
    const inLogs = await sbx.process.getSessionCommandLogs(sid, r.cmdId)
    check(done.exitCode === 0 && inLogs.stdout.trim() === 'got:typed', `input ${done.exitCode} ${JSON.stringify(inLogs.stdout)}`)

    const s = await sbx.process.getSession(sid)
    check(s.sessionId === sid && s.commands.length === 5, `session ${s.commands.length} commands`)
    check((await sbx.process.listSessions()).some((x) => x.sessionId === sid), 'listSessions')
    await sbx.process.deleteSession(sid)
    await expectError(DaytonaNotFoundError, () => sbx.process.getSession(sid), 'deleted session')
    return `${s.commands.length} commands`
  })

  await step('files', async () => {
    const fs = sbx.fs
    await fs.createFolder('probe/sub', '755')
    await fs.uploadFile(Buffer.from('hello file'), 'probe/a.txt')
    await fs.uploadFiles([
      { source: Buffer.from('one'), destination: '/home/daytona/probe/sub/1.txt' },
      { source: Buffer.from([0, 1, 98, 255]), destination: 'probe/sub/2.bin' },
    ])
    const names = (await fs.listFiles('probe')).map((f) => f.name).sort()
    check(JSON.stringify(names) === '["a.txt","sub"]', `list ${names}`)
    const info = await fs.getFileDetails('/home/daytona/probe/a.txt')
    check(info.size === 10 && !info.isDir && info.owner === 'daytona', `info ${JSON.stringify(info)}`)
    check((await fs.getFileDetails('probe/sub')).isDir, 'info of a directory')
    check((await fs.downloadFile('probe/a.txt')).toString() === 'hello file', 'downloadFile')
    const res = await fs.downloadFiles([{ source: 'probe/sub/1.txt' }, { source: 'probe/sub/2.bin' }, { source: 'probe/missing' }])
    check(res[0].result?.toString() === 'one', `bulk 0 ${JSON.stringify(res[0])}`)
    check(Buffer.compare(res[1].result, Buffer.from([0, 1, 98, 255])) === 0, 'bulk binary')
    check(res[2].error && !res[2].result, `bulk missing ${JSON.stringify(res[2])}`)
    const e = await expectError(DaytonaError, () => fs.downloadFile('probe/missing'), 'download missing')
    await fs.moveFiles('probe/a.txt', 'probe/b.txt')
    check(JSON.stringify((await fs.listFiles('probe')).map((f) => f.name).sort()) === '["b.txt","sub"]', 'move')
    await fs.deleteFile('probe/b.txt')
    await fs.deleteFile('probe', true)
    await expectError(DaytonaNotFoundError, () => fs.getFileDetails('probe'), 'deleted dir')
    return `missing -> ${e.constructor.name}`
  })

  await step('preview', async () => {
    await sbx.process.createSession('web')
    await sbx.process.executeSessionCommand('web', {
      command: 'mkdir -p /tmp/www && echo preview-ok > /tmp/www/index.html && cd /tmp/www && python3 -m http.server 8080',
      runAsync: true,
    })
    const link = await sbx.getPreviewLink(8080)
    check(link.url && link.token, `link ${JSON.stringify(link)}`)
    let res
    for (let i = 0; i < 50; i++) {
      res = await httpGet(link.url + '/index.html', { 'x-daytona-preview-token': link.token })
      if (res.status === 200) break
      await sleep(200)
    }
    check(res.status === 200 && res.body.trim() === 'preview-ok', `with token ${res.status} ${res.body.slice(0, 200)}`)
    const anon = await httpGet(link.url + '/index.html')
    check(anon.status === 401 || anon.status === 403, `without token ${anon.status}`)
    await sbx.process.deleteSession('web')
    return link.url
  })

  await step('stop_start', async () => {
    if (process.env.DAYTONA_PROBE_STOP === '0') throw new Skip('DAYTONA_PROBE_STOP=0')
    await sbx.process.executeCommand('echo kept > /home/daytona/persist.txt')
    await sbx.stop()
    check(sbx.state === 'stopped', `after stop ${sbx.state}`)
    check((await daytona.get(sbx.id)).state === 'stopped', 'get after stop')
    await expectError(DaytonaError, () => sbx.process.executeCommand('true'), 'exec when stopped')
    await sbx.start()
    check(sbx.state === 'started', `after start ${sbx.state}`)
    const r = await sbx.process.executeCommand('cat /home/daytona/persist.txt')
    check(r.result.trim() === 'kept', `file across stop/start ${JSON.stringify(r.result)}`)
  })

  await step('delete', async () => {
    await sbx.delete()
    await expectError(DaytonaNotFoundError, () => daytona.get(sbx.id), 'get after delete')
  })
}
process.exit(fails)
