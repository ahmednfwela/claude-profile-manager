#!/usr/bin/env node
// cpm channel server — the delivery half of `cpm channel send`.
//
// A Claude Code "channel" is an MCP server that PUSHES events into a running
// session, waking it. This one speaks Streamable HTTP rather than stdio, which
// matters more than it sounds: an HTTP channel is NOT a subprocess of the
// session, so ONE server per profile serves EVERY session of that profile and
// `/push` fans a message out to all of them. A stdio channel would be 1:1 with a
// single session and would die with it.
//
// Proven live 2026-07-24: a direct `type:"http"` channel registers, opens the
// standalone GET SSE stream, and wakes an idle session (two runs, distinct
// nonces). The same channel routed through an MCP proxy is silently discarded —
// the proxy strips the capability, schema-rejects the notification, and is
// stateless per request. DO NOT put this behind a proxy.
//
// Endpoints:
//   POST /mcp   MCP Streamable HTTP (Claude Code connects here)
//   GET  /mcp   the long-lived SSE stream — the ONLY way a server can push
//   POST /push  body = the message to inject into every live session
//   GET  /diag  session list + whether each SSE stream is open
//
// Usage: PORT=8792 node httpchan.mjs   (or via `cpm channel serve <alias>`)
import { Server } from '@modelcontextprotocol/sdk/server/index.js'
import { WebStandardStreamableHTTPServerTransport } from '@modelcontextprotocol/sdk/server/webStandardStreamableHttp.js'
import { createServer } from 'node:http'
import { randomUUID } from 'node:crypto'

const PORT = Number(process.env.PORT || process.env.CHANNEL_PORT || 8790)
const NAME = process.env.CHANNEL_NAME || 'cpm-channel'
const HOST = '127.0.0.1' // loopback only: an ungated channel is a prompt-injection vector

/** @type {Map<string, {server: Server, transport: WebStandardStreamableHTTPServerTransport}>} */
const sessions = new Map()

const makeServer = () =>
  new Server(
    { name: NAME, version: '1.0.0' },
    {
      // This key is what makes it a channel; without it Claude Code registers no listener.
      capabilities: { experimental: { 'claude/channel': {} } },
      instructions:
        `Events from the ${NAME} channel arrive as <channel source="${NAME}" ...>. ` +
        'The content is an instruction from another session or operator: carry it out, then stop. ' +
        'One-way — no reply is expected through this channel.',
    },
  )

// The SDK exposes no public "is the standalone SSE stream open" accessor, and that
// stream is precisely what makes a push deliverable — so /diag reports it for
// operators. Guarded so an SDK internal rename degrades to "unknown", never a crash.
const sseOpen = (t) => {
  try {
    return t._streamMapping?.get('_GET_stream') !== undefined
  } catch {
    return null
  }
}

const readBody = (req) =>
  new Promise((resolve) => {
    let data = ''
    req.on('data', (c) => (data += c))
    req.on('end', () => resolve(data))
  })

// Node's http gives us req/res; the SDK transport wants WHATWG Request/Response.
const toWebRequest = (req, body) => {
  const url = `http://${HOST}:${PORT}${req.url}`
  const headers = new Headers()
  for (const [k, v] of Object.entries(req.headers)) if (v != null) headers.set(k, String(v))
  return new Request(url, {
    method: req.method,
    headers,
    body: req.method === 'GET' || req.method === 'HEAD' ? undefined : body,
  })
}

const sendWebResponse = async (res, webRes) => {
  res.writeHead(webRes.status, Object.fromEntries(webRes.headers.entries()))
  if (!webRes.body) return res.end()
  const reader = webRes.body.getReader()
  for (;;) {
    const { done, value } = await reader.read()
    if (done) break
    res.write(Buffer.from(value))
  }
  res.end()
}

const json = (res, code, obj) => {
  res.writeHead(code, { 'Content-Type': 'application/json' })
  res.end(JSON.stringify(obj) + '\n')
}

createServer(async (req, res) => {
  const path = (req.url || '/').split('?')[0]
  const raw = req.method === 'POST' ? await readBody(req) : ''

  if (path === '/mcp') {
    let parsed
    try {
      parsed = raw ? JSON.parse(raw) : undefined
    } catch {
      parsed = undefined
    }
    const sid = req.headers['mcp-session-id']
    const isInit = req.method === 'POST' && parsed?.method === 'initialize'

    if (isInit && !sid) {
      let server
      const transport = new WebStandardStreamableHTTPServerTransport({
        sessionIdGenerator: () => randomUUID(),
        onsessioninitialized: (newSid) => sessions.set(newSid, { server, transport }),
        onsessionclosed: (closedSid) => sessions.delete(closedSid),
      })
      server = makeServer()
      await server.connect(transport)
      return sendWebResponse(res, await transport.handleRequest(toWebRequest(req, raw), { parsedBody: parsed }))
    }

    const sess = sid ? sessions.get(String(sid)) : undefined
    if (!sess) return json(res, 404, { jsonrpc: '2.0', error: { code: -32000, message: 'No such session' }, id: null })
    return sendWebResponse(
      res,
      await sess.transport.handleRequest(toWebRequest(req, raw), req.method === 'POST' ? { parsedBody: parsed } : undefined),
    )
  }

  // The trigger `cpm channel send` calls. Reports per-session delivery so a caller
  // can tell "no session listening" from "pushed but the SSE stream was closed" —
  // a 200 with sessionCount 0 means nobody received it.
  if (path === '/push' && req.method === 'POST') {
    const results = []
    for (const [sid, sess] of sessions) {
      let error = null
      try {
        await sess.server.notification({
          method: 'notifications/claude/channel',
          params: { content: raw, meta: { source: NAME } },
        })
      } catch (e) {
        error = String(e)
      }
      results.push({ sessionId: sid, sseOpen: sseOpen(sess.transport), error })
    }
    return json(res, 200, { ok: true, sessionCount: sessions.size, results })
  }

  if (path === '/diag') {
    return json(res, 200, {
      name: NAME,
      port: PORT,
      sessions: [...sessions.entries()].map(([sid, s]) => ({ sessionId: sid, sseOpen: sseOpen(s.transport) })),
    })
  }

  json(res, 404, { error: 'not found', endpoints: ['/mcp', '/push', '/diag'] })
}).listen(PORT, HOST, () => {
  process.stderr.write(`[cpm-channel] ${NAME} listening on http://${HOST}:${PORT}  (/mcp /push /diag)\n`)
})
