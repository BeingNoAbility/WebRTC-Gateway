import { DurableObject } from 'cloudflare:workers';

/**
 * Cloudflare Worker: Multi-Node P2P Signaling Server
 * 
 * Backed by Cloudflare SQLite-backed Durable Objects.
 * Supports multiple agents registered under different node_ids simultaneously.
 * Accurately routes WebRTC Offer/Answer/Candidate by node_id.
 */

export class SignalingRoom extends DurableObject {
  constructor(ctx, env) {
    super(ctx, env);
    // agents: node_id (string) -> WebSocket
    this.agents = new Map();
    // agentNodeIds: WebSocket -> node_id (string)
    this.agentNodeIds = new Map();
    // nodeSecrets: node_id (string) -> registered secret (anti-hijack protection)
    this.nodeSecrets = new Map();
    // nodeVisitorAuth: node_id (string) -> boolean (whether visitors require password)
    this.nodeVisitorAuth = new Map();

    // clients: WebSocket -> { sessionId: string, targetNodeId: string }
    this.clients = new Map();
    // clientBySession: sessionId (string) -> WebSocket
    this.clientBySession = new Map();
    // ipConns: clientIP (string) -> count (number)
    this.ipConns = new Map();
    // failedAttempts: clientIP (string) -> count (number)
    this.failedAttempts = new Map();
  }

  async fetch(request) {
    const clientIP = request.headers.get('cf-connecting-ip') || 'unknown';
    const currentConns = this.ipConns.get(clientIP) || 0;
    if (currentConns >= 64) {
      return new Response('429 Too Many Requests: Concurrent connection limit exceeded', { status: 429 });
    }
    this.ipConns.set(clientIP, currentConns + 1);

    const pair = new WebSocketPair();
    const [client, server] = Object.values(pair);

    server.accept();
    this.handleWS(server, clientIP);

    return new Response(null, {
      status: 101,
      webSocket: client,
    });
  }

  handleWS(ws, clientIP) {
    let role = '';
    let sessionId = crypto.randomUUID();
    let agentNodeId = '';

    let ipCleaned = false;
    const cleanupIP = () => {
      if (ipCleaned) return;
      ipCleaned = true;
      const c = this.ipConns.get(clientIP) || 0;
      if (c <= 1) this.ipConns.delete(clientIP);
      else this.ipConns.set(clientIP, c - 1);
    };

    ws.addEventListener('close', cleanupIP);
    ws.addEventListener('error', cleanupIP);

    ws.addEventListener('message', (event) => {
      try {
        const msg = JSON.parse(event.data);

        switch (msg.type) {
          case 'register': {
            role = 'agent';
            const nodeId = msg.node_id;
            const secret = msg.secret || '';
            if (!nodeId) return;
            agentNodeId = nodeId;

            // Global cluster secret verification
            if (this.env && this.env.SIGNAL_SECRET && this.env.SIGNAL_SECRET !== secret) {
              try {
                ws.send(JSON.stringify({
                  type: 'error',
                  code: 4003,
                  message: 'unauthorized: invalid cluster signal secret',
                }));
                ws.close(4003, 'Unauthorized');
              } catch {}
              return;
            }

            // Anti-hijack protection: verify node secret if already bound
            const boundSecret = this.nodeSecrets.get(nodeId);
            if (boundSecret && boundSecret !== secret) {
              const fails = (this.failedAttempts.get(clientIP) || 0) + 1;
              this.failedAttempts.set(clientIP, fails);
              try {
                ws.send(JSON.stringify({
                  type: 'error',
                  error: fails >= 10 
                    ? 'Too many failed authentication attempts, throttled'
                    : `Unauthorized: node_id "${nodeId}" is protected by a different secret`,
                }));
                ws.close(4003, 'Unauthorized');
              } catch {}
              return;
            }

            if (secret) {
              this.nodeSecrets.set(nodeId, secret);
            }

            // If an old agent connection existed for this nodeId, close it cleanly
            const oldWs = this.agents.get(nodeId);
            if (oldWs && oldWs !== ws) {
              try {
                oldWs.send(JSON.stringify({ type: 'error', error: 'Replaced by newer agent session' }));
                oldWs.close(1000, 'Replaced');
              } catch {}
              this.agentNodeIds.delete(oldWs);
            }

            this.agents.set(nodeId, ws);
            this.agentNodeIds.set(ws, nodeId);
            const requireVisitorAuth = msg.require_visitor_auth !== undefined ? !!msg.require_visitor_auth : (msg.requires_secret !== undefined ? !!msg.requires_secret : !!secret);
            this.nodeVisitorAuth.set(nodeId, requireVisitorAuth);
            ws.send(JSON.stringify({ type: 'registered', node_id: nodeId }));

            // Notify all clients waiting for this node
            for (const [clWs, clientInfo] of this.clients.entries()) {
              if (clientInfo.targetNodeId === nodeId) {
                try {
                  clWs.send(
                    JSON.stringify({
                      type: 'peer_status',
                      node_id: nodeId,
                      session_id: clientInfo.sessionId,
                      status: 'online',
                      requires_secret: requireVisitorAuth,
                    })
                  );
                } catch {}
              }
            }
            break;
          }

          case 'join': {
            role = 'client';
            const targetNodeId = msg.node_id;
            this.clients.set(ws, { sessionId, targetNodeId });
            this.clientBySession.set(sessionId, ws);

            const isOnline = this.agents.has(targetNodeId);
            const requiresSecret = this.nodeVisitorAuth.has(targetNodeId) ? this.nodeVisitorAuth.get(targetNodeId) : !!this.nodeSecrets.get(targetNodeId);
            ws.send(
              JSON.stringify({
                type: 'peer_status',
                node_id: targetNodeId,
                session_id: sessionId,
                status: isOnline ? 'online' : 'offline',
                requires_secret: requiresSecret,
              })
            );
            break;
          }

          case 'offer': {
            msg.session_id = sessionId;
            msg.client_ip = clientIP === 'unknown' ? '' : clientIP;
            const targetNodeId = msg.node_id;
            const targetAgent = this.agents.get(targetNodeId);
            if (targetAgent) {
              targetAgent.send(JSON.stringify(msg));
            } else {
              ws.send(JSON.stringify({ type: 'error', error: `Target agent [${targetNodeId}] is offline` }));
            }
            break;
          }

          case 'answer': {
            // Agent -> Client (Strict node attribution validation)
            if (role === 'agent') {
              const clWs = this.clientBySession.get(msg.session_id);
              if (clWs) {
                const clientInfo = this.clients.get(clWs);
                if (clientInfo && clientInfo.targetNodeId === agentNodeId) {
                  clWs.send(JSON.stringify(msg));
                }
              }
            }
            break;
          }

          case 'candidate': {
            if (role === 'agent') {
              // Agent -> Client (Strict node attribution validation)
              const clWs = this.clientBySession.get(msg.session_id);
              if (clWs) {
                const clientInfo = this.clients.get(clWs);
                if (clientInfo && clientInfo.targetNodeId === agentNodeId) {
                  clWs.send(JSON.stringify(msg));
                }
              }
            } else {
              // Client -> Agent
              msg.session_id = sessionId;
              const targetNodeId = msg.node_id;
              const targetAgent = this.agents.get(targetNodeId);
              if (targetAgent) {
                targetAgent.send(JSON.stringify(msg));
              }
            }
            break;
          }

          case 'error': {
            // Relay error message (e.g. auth rejection from agent to client with attribution validation)
            if (role === 'agent' && msg.session_id) {
              const clWs = this.clientBySession.get(msg.session_id);
              if (clWs) {
                const clientInfo = this.clients.get(clWs);
                if (clientInfo && clientInfo.targetNodeId === agentNodeId) {
                  clWs.send(JSON.stringify(msg));
                }
              }
            }
            break;
          }
        }
      } catch (err) {
        console.error('Signaling message error:', err);
      }
    });

    ws.addEventListener('close', () => {
      if (role === 'agent') {
        const nodeId = this.agentNodeIds.get(ws);
        if (nodeId) {
          if (this.agents.get(nodeId) === ws) {
            this.agents.delete(nodeId);
            // Notify clients targeting this node that it went offline
            for (const [clWs, clientInfo] of this.clients.entries()) {
              if (clientInfo.targetNodeId === nodeId) {
                try {
                  clWs.send(
                    JSON.stringify({
                      type: 'peer_status',
                      node_id: nodeId,
                      session_id: clientInfo.sessionId,
                      status: 'offline',
                    })
                  );
                } catch {}
              }
            }
          }
          this.agentNodeIds.delete(ws);
        }
      } else {
        const info = this.clients.get(ws);
        if (info) {
          this.clientBySession.delete(info.sessionId);
          this.clients.delete(ws);
        }
      }
    });
  }
}

export default {
  async fetch(request, env) {
    const url = new URL(request.url);

    // 1. WebSocket Signaling endpoint (/ws)
    if (url.pathname === '/ws') {
      const upgradeHeader = request.headers.get('Upgrade');
      if (!upgradeHeader || upgradeHeader.toLowerCase() !== 'websocket') {
        return new Response('Expected WebSocket upgrade at /ws', { status: 426 });
      }

      // Unify all nodes and clients under the global signaling broker room
      const id = env.ROOM.idFromName('global-broker');
      const stub = env.ROOM.get(id);

      return stub.fetch(request);
    }

    // 2. Health check endpoint (/health or /api/status)
    if (url.pathname === '/health' || url.pathname === '/api/status') {
      return new Response(
        JSON.stringify({
          status: 'online',
          mode: 'integrated-worker-and-assets',
          time: new Date().toISOString(),
        }),
        {
          headers: { 'content-type': 'application/json', 'access-control-allow-origin': '*' },
        }
      );
    }

    // 3. Static Portal Assets (HTML, JS, Service Worker, etc.)
    if (env.ASSETS) {
      return env.ASSETS.fetch(request);
    }

    return new Response(
      JSON.stringify({
        status: 'online',
        edge: 'cloudflare-durable-object',
        time: new Date().toISOString(),
      }),
      {
        headers: { 'content-type': 'application/json', 'access-control-allow-origin': '*' },
      }
    );
  },
};
