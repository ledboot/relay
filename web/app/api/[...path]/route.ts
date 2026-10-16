import { NextRequest } from 'next/server';
export const runtime = 'nodejs';
export const dynamic = 'force-dynamic';
async function proxy(req: NextRequest, context: { params: Promise<{path: string[]}> }) {
  const { path } = await context.params;
  if (path.some(p => !/^[a-zA-Z0-9_-]+$/.test(p))) return Response.json({error:'无效路径'}, {status:400});
  if (req.method !== 'GET' && !path.includes('webhooks')) {
    const origin = req.headers.get('origin');
    const expected = process.env.PUBLIC_ORIGIN || req.nextUrl.origin;
    let allowed = origin === expected;
    if (origin && !allowed && process.env.NODE_ENV === 'development') {
      try {
        const source = new URL(origin), target = new URL(expected);
        const loopback = new Set(['localhost', '127.0.0.1', '[::1]']);
        allowed = loopback.has(source.hostname) && loopback.has(target.hostname)
          && source.protocol === target.protocol && source.port === target.port;
      } catch { allowed = false; }
    }
    if (origin && !allowed) return Response.json({error:'来源不允许'}, {status:403});
  }
  const headers = new Headers();
  for (const name of ['content-type','authorization','x-webhook-signature']) {
    const value = req.headers.get(name); if (value) headers.set(name,value);
  }
  // Enable only behind a reverse proxy that overwrites X-Forwarded-For.
  headers.set('X-Client-IP', process.env.TRUST_PROXY === 'true' ? (req.headers.get('x-forwarded-for')?.split(',')[0]?.trim() || 'shared') : 'shared');
  try {
    const response = await fetch(`${process.env.API_INTERNAL_URL || 'http://127.0.0.1:8080'}/api/${path.join('/')}${req.nextUrl.search}`, {
      method:req.method, headers, body:req.method === 'GET' ? undefined : await req.arrayBuffer(), cache:'no-store', signal:AbortSignal.timeout(55000), redirect:'error'
    });
    return new Response(response.body,{status:response.status,headers:{'Content-Type':'application/json','Cache-Control':'no-store'}});
  } catch { return Response.json({error:'服务暂时不可用，请稍后重试'}, {status:502}); }
}
export { proxy as GET, proxy as POST };
