// Points the unmodified @vercel/sandbox SDK at another server, as a Node preload:
//
//   VERCEL_SANDBOX_URL=http://127.0.0.1:7820/api node --import ./target.mjs app.mjs
//
// The JS SDK has no base-URL option or environment variable: APIClient hardcodes
// https://vercel.com/api and Sandbox.create() never passes a baseUrl through
// (see docs/providers/vercel.md). Every request still goes through
// globalThis.fetch, captured when each APIClient is built, so rewriting that one
// function before the SDK loads redirects all of it without touching app code.
//
// VERCEL_SANDBOX_DOMAIN_TEMPLATE optionally rewrites the port URLs that
// sandbox.domain(port) returns (always https://<subdomain>.vercel.run in the
// SDK) when the app fetches them, e.g. "http://127.0.0.1:7821/{subdomain}".
// Pointed at a server other than hosted Vercel, it defaults to that server's
// origin with the subdomain as the first path segment, which sandboxd's
// Vercel listener serves (docs/vercel-sdk.md), so that a port check never
// leaves for the real vercel.run. "off" turns the rewrite off (run.sh does,
// recording hosted Vercel through its proxy).
const UPSTREAM = 'https://vercel.com/api';
const target = (process.env.VERCEL_SANDBOX_URL || '').replace(/\/+$/, '');
const setTemplate = process.env.VERCEL_SANDBOX_DOMAIN_TEMPLATE || '';
const domainTemplate = setTemplate === 'off' ? '' : setTemplate ||
  (target && target !== UPSTREAM ? new URL(target).origin + '/{subdomain}' : '');

if (target && target !== UPSTREAM || domainTemplate) {
  const realFetch = globalThis.fetch;
  globalThis.fetch = function patchedFetch(input, init) {
    let url = typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
    if (target && url.startsWith(UPSTREAM + '/')) {
      url = target + url.slice(UPSTREAM.length);
    } else if (domainTemplate) {
      const m = /^https:\/\/([^/]+)\.vercel\.run(\/.*)?$/.exec(url);
      if (m) url = domainTemplate.replace('{subdomain}', m[1]).replace(/\/+$/, '') + (m[2] || '/');
    }
    if (typeof input === 'string' || input instanceof URL) return realFetch(url, init);
    return realFetch(new Request(url, input), init);
  };
}
