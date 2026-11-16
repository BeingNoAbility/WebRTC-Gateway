import { P2PMux, P2PStream } from '../protocol/mux';

const DEFAULT_NODE_ID = 'myhome';
const DEFAULT_SIGNAL_URL = (typeof window !== 'undefined' && window.location && window.location.host)
  ? `${window.location.protocol === 'https:' ? 'wss:' : 'ws:'}//${window.location.host}/ws`
  : 'ws://127.0.0.1:8888/ws';

async function computeHMACSHA256Hex(keyStr: string, message: string): Promise<string> {
  const enc = new TextEncoder();
  const keyData = enc.encode(keyStr);
  const msgData = enc.encode(message);
  const cryptoKey = await crypto.subtle.importKey(
    'raw',
    keyData,
    { name: 'HMAC', hash: 'SHA-256' },
    false,
    ['sign']
  );
  const signature = await crypto.subtle.sign('HMAC', cryptoKey, msgData);
  const hashArray = Array.from(new Uint8Array(signature));
  return hashArray.map((b) => b.toString(16).padStart(2, '0')).join('');
}

interface WebRTCHUDInfo {
  transportType: 'direct' | 'relay' | 'unknown';
  localType: string;
  remoteType: string;
  localAddress: string;
  remoteAddress: string;
  protocol: string;
  rttMs: number;
  rxRateKB: number;
  txRateKB: number;
  totalRxMB: number;
  totalTxMB: number;
  packetsLost: number;
  iceState: string;
  connState: string;
}

class PortalApp {
  private peerId: string;
  private signalUrl: string;
  private secret: string = '';
  private shareExp: number = 0;
  private shareSign: string = '';
  private isIceRestarting: boolean = false;
  private iceRestartTimer: any = null;
  private clientToken: string = '';
  private initialSubpath: string = '/';
  private ws: WebSocket | null = null;
  private pc: RTCPeerConnection | null = null;

  // WebRTC HUD Diagnostics & Real-time Metrics
  private hudStatsTimer: any = null;
  private lastStatsTime: number = 0;
  private lastBytesReceived: number = 0;
  private lastBytesSent: number = 0;
  private hudInfo: WebRTCHUDInfo = {
    transportType: 'unknown',
    localType: '',
    remoteType: '',
    localAddress: '',
    remoteAddress: '',
    protocol: 'UDP',
    rttMs: 0,
    rxRateKB: 0,
    txRateKB: 0,
    totalRxMB: 0,
    totalTxMB: 0,
    packetsLost: 0,
    iceState: 'disconnected',
    connState: 'new',
  };
  private dc: RTCDataChannel | null = null;
  private mux: P2PMux | null = null;
  private pingInterval: any = null;
  private swRegistration: ServiceWorkerRegistration | null = null;

  // UI Elements
  private connBadge = document.getElementById('connBadge') as HTMLElement;
  private peerVal = document.getElementById('peerVal') as HTMLElement;
  private signalVal = document.getElementById('signalVal') as HTMLElement;
  private rttVal = document.getElementById('rttVal') as HTMLElement;
  private streamsVal = document.getElementById('streamsVal') as HTMLElement;
  private logBox = document.getElementById('logBox') as HTMLElement;
  private btnPing = document.getElementById('btnPing') as HTMLButtonElement;
  private btnEcho = document.getElementById('btnEcho') as HTMLButtonElement;
  private btnClearLog = document.getElementById('btnClearLog') as HTMLButtonElement;
  private testOutput = document.getElementById('testOutput') as HTMLElement;

  private appFrame: HTMLIFrameElement | null = null;
  private vaultContainer: HTMLElement | null = null;
  private activeVaultAbortController: AbortController | null = null;
  private activeStreamsCount = 0;

  constructor() {
    const config = this.resolveInitialConfig();
    this.peerId = config.peerId;
    this.signalUrl = config.signalUrl;
    this.secret = config.secret;
    this.shareExp = config.exp || 0;
    this.shareSign = config.sign || '';
    this.initialSubpath = config.path || '/';

    // Long-lived client token for persisting HttpOnly sessions across page reloads (F5)
    let token = '';
    try {
      token = sessionStorage.getItem('p2p_client_token') || '';
      if (!token) {
        token = 'cl_' + (typeof crypto !== 'undefined' && crypto.randomUUID ? crypto.randomUUID() : Math.random().toString(36).substring(2) + Date.now().toString(36));
        sessionStorage.setItem('p2p_client_token', token);
      }
    } catch {
      token = 'cl_' + Math.random().toString(36).substring(2);
    }
    this.clientToken = token;

    if (this.peerVal) this.peerVal.textContent = this.peerId;
    if (this.signalVal) this.signalVal.textContent = this.signalUrl;

    // Listen for route synchronization from inner iframe SPA
    if (typeof window !== 'undefined') {
      window.addEventListener('message', (e) => {
        if (e.data && e.data.type === 'P2P_ROUTE_SYNC') {
          const rawPath = e.data.path;
          if (
            typeof rawPath === 'string' &&
            rawPath.startsWith('/') &&
            !rawPath.startsWith('//') &&
            !rawPath.toLowerCase().includes('javascript:')
          ) {
            this.syncRouteToHash(rawPath);
          }
        }
      });
    }

    this.bindEvents();
    this.initServiceWorker();
    this.startSignaling();
    this.listenChildMessages();
    this.createSettingsModal();
  }

  // Synchronize internal iframe route changes to outer URL Hash (e.g. #myhome?path=/settings)
  private syncRouteToHash(subPath: string) {
    if (!this.peerId) return;
    try {
      const hashParams = new URLSearchParams();
      if (this.secret) {
        hashParams.set('secret', this.secret);
      } else if (this.shareSign && this.shareExp) {
        hashParams.set('exp', this.shareExp.toString());
        hashParams.set('sign', this.shareSign);
      }
      if (subPath && subPath !== '/') {
        hashParams.set('path', subPath);
      }
      const qs = hashParams.toString();
      const newHash = '#' + this.peerId + (qs ? '?' + qs : '');
      if (window.location.hash !== newHash) {
        window.history.replaceState(null, '', newHash);
      }
    } catch {}
  }

  // Extracts explicit Node ID from subdomain with ".p2p." delimiter (e.g. bili.p2p.example.com)
  private extractExplicitP2PSubdomain(): string | null {
    try {
      const hostname = window.location.hostname.toLowerCase().trim();
      if (!hostname) return null;
      const reserved = new Set([
        'p2p', 'www', 'webrtc', 'webrtc-p2p-signal', 'signal', 'signaling',
        'gateway', 'portal', 'api', 'admin', 'proxy', 'vpn', 'tunnel'
      ]);
      const p2pIndex = hostname.indexOf('.p2p.');
      if (p2pIndex > 0) {
        const sub = hostname.substring(0, p2pIndex);
        const parts = sub.split('.');
        const candidate = parts[parts.length - 1];
        if (candidate && !reserved.has(candidate)) {
          return candidate;
        }
      }
    } catch {}
    return null;
  }

  // Extracts Node ID from subdomain (supports multi-level custom subdomains)
  // Non-explicit subdomains are checked only as fallback after stored peer ID
  private extractNodeIdFromHostname(): string | null {
    try {
      const hostname = window.location.hostname.toLowerCase().trim();
      if (!hostname) return null;

      // Ignore raw IP addresses and bare localhost
      if (/^(\d{1,3}\.){3}\d{1,3}$/.test(hostname) || hostname === 'localhost') {
        return null;
      }

      // Exclude common PaaS/host roots where 1st label is project/account name, not peer ID
      const paasSuffixes = [
        '.pages.dev', '.workers.dev', '.eu.org', '.github.io',
        '.vercel.app', '.netlify.app', '.onrender.com', '.fly.dev'
      ];
      for (const suffix of paasSuffixes) {
        if (hostname.endsWith(suffix)) {
          const prefix = hostname.slice(0, -suffix.length);
          if (!prefix.includes('.')) {
            return null;
          }
        }
      }

      // Reserved keywords that represent portal infrastructure, not a node ID
      const reserved = new Set([
        'p2p', 'www', 'webrtc', 'webrtc-p2p-signal', 'signal', 'signaling',
        'gateway', 'portal', 'api', 'admin', 'proxy', 'vpn', 'tunnel',
        'webrtc-gateway', 'p2p-gateway', 'app', 'web', 'client', 'relay',
        'service', 'hub', 'test', 'demo', 'node', 'nodes', 'server', 'direct',
        'static', 'assets', 'cdn'
      ]);

      // Multi-level subdomain (e.g. "immich.example.com", "bili.localhost")
      const parts = hostname.split('.');
      if (parts.length >= 3 || (parts.length === 2 && parts[1] === 'localhost')) {
        const firstLabel = parts[0];
        if (firstLabel && !reserved.has(firstLabel)) {
          return firstLabel;
        }
      }
    } catch {}
    return null;
  }

  // Resolves configuration with priority: Query -> Hash (#nodeId) -> Explicit .p2p. Subdomain -> Stored Peer -> Subdomain -> Built-in Defaults
  private resolveInitialConfig(): {
    peerId: string;
    signalUrl: string;
    secret: string;
    path: string;
    exp?: number;
    sign?: string;
  } {
    const params = new URLSearchParams(window.location.search);

    // 1. Peer ID and Secret resolution:
    // Priority 1: Query parameter ?peer= or ?node= (explicit in virtualized link / window.open)
    let peerId = params.get('peer') || params.get('node');
    let secret = '';
    let initialPath = '/';
    let exp = 0;
    let sign = '';

    // Parse path from query if present and safe
    const queryPath = params.get('path');
    if (queryPath && queryPath.startsWith('/') && !queryPath.startsWith('//') && !queryPath.toLowerCase().includes('javascript:')) {
      initialPath = queryPath;
    }

    // Security Hardening: Deprecate ?secret= in query string.
    // If legacy query has secret/token, migrate to Hash and strip from search immediately.
    const querySecret = params.get('secret') || params.get('token');
    if (querySecret) {
      secret = querySecret;
      try {
        params.delete('secret');
        params.delete('token');
        const cleanSearch = params.toString() ? '?' + params.toString() : '';
        const newUrl = window.location.pathname + cleanSearch + window.location.hash;
        window.history.replaceState(null, '', newUrl);
      } catch {}
    }

    // Priority 2: URL hash (#nodeId or #nodeId?secret=xxx&exp=xxx&sign=yyy&path=/subpath)
    if (window.location.hash) {
      const rawHash = window.location.hash.replace(/^#\/?/, '').trim();
      if (rawHash) {
        const parts = rawHash.split(/[?&]/);
        if (!peerId) peerId = parts[0];
        for (let i = 1; i < parts.length; i++) {
          const [k, v] = parts[i].split('=');
          if ((k === 'secret' || k === 'token') && v) {
            secret = decodeURIComponent(v);
          } else if (k === 'exp' && v) {
            exp = parseInt(decodeURIComponent(v), 10) || 0;
          } else if (k === 'sign' && v) {
            sign = decodeURIComponent(v);
          } else if (k === 'path' && v) {
            const dec = decodeURIComponent(v);
            if (dec.startsWith('/') && !dec.startsWith('//') && !dec.toLowerCase().includes('javascript:')) {
              initialPath = dec;
            }
          }
        }
      }
    }

    // Priority 3: Explicit .p2p. subdomain (e.g. bili.p2p.example.com)
    if (!peerId) {
      const explicitSub = this.extractExplicitP2PSubdomain();
      if (explicitSub) {
        peerId = explicitSub;
      }
    }

    // Priority 4: Cached active peer from sessionStorage / localStorage (preserves active node across new windows/tabs)
    if (!peerId) {
      peerId = sessionStorage.getItem('p2p_active_peer_id') || localStorage.getItem('p2p_peer_id') || '';
    }

    // Priority 5: Fallback generic custom subdomain (e.g. immich.mycustomdomain.com)
    if (!peerId) {
      const subdomainNode = this.extractNodeIdFromHostname();
      if (subdomainNode) {
        peerId = subdomainNode;
      }
    }

    // Priority 6: Built-in default
    if (!peerId) {
      peerId = DEFAULT_NODE_ID;
    }

    if (!secret && peerId) {
      secret = sessionStorage.getItem('p2p_secret') || localStorage.getItem('p2p_secret_' + peerId) || '';
    }

    // 2. Signaling URL: ?signal= -> localStorage -> auto-detected co-located /ws on server hosts
    const isStaticPlatform =
      window.location.hostname.includes('pages.dev') ||
      window.location.hostname.includes('github.io') ||
      window.location.hostname.includes('vercel.app');

    let signalUrl = params.get('signal');
    if (!signalUrl) {
      const cached = localStorage.getItem('p2p_signal_url');
      if (cached) {
        signalUrl = cached;
      } else if (!isStaticPlatform && window.location.host) {
        const proto = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
        signalUrl = `${proto}//${window.location.host}/ws`;
      } else {
        signalUrl = DEFAULT_SIGNAL_URL;
      }
    }

    // Persist active settings in sessionStorage and localStorage for future visits / windows
    try {
      sessionStorage.setItem('p2p_active_peer_id', peerId);
      localStorage.setItem('p2p_peer_id', peerId);
      localStorage.setItem('p2p_signal_url', signalUrl);
      if (secret) {
        sessionStorage.setItem('p2p_secret', secret);
        localStorage.setItem('p2p_secret_' + peerId, secret);
      }
    } catch {}

    return { peerId, signalUrl, secret, path: initialPath, exp, sign };
  }

  private log(msg: string) {
    const time = new Date().toLocaleTimeString();
    if (this.logBox) {
      this.logBox.textContent += `[${time}] ${msg}\n`;
      this.logBox.scrollTop = this.logBox.scrollHeight;
    }
    console.log(`[Portal] ${msg}`);
  }

  private bindEvents() {
    if (this.btnClearLog) {
      this.btnClearLog.onclick = () => {
        if (this.logBox) this.logBox.textContent = '';
      };
    }

    if (this.btnPing) {
      this.btnPing.onclick = async () => {
        if (!this.mux) return;
        try {
          const rtt = await this.mux.ping();
          this.rttVal.textContent = `${rtt.toFixed(1)} ms`;
          this.log(`Manual Ping successful: RTT = ${rtt.toFixed(2)} ms`);
        } catch (err: any) {
          this.log(`Ping failed: ${err.message}`);
        }
      };
    }

    if (this.btnEcho) {
      this.btnEcho.onclick = async () => {
        await this.runEchoTest();
      };
    }

    // Portal header & card direct settings trigger
    document.getElementById('btnHeaderSettings')?.addEventListener('click', () => {
      this.openSettingsModal();
    });
    document.getElementById('cardPeer')?.addEventListener('click', () => {
      this.openSettingsModal();
    });
    document.getElementById('cardSignal')?.addEventListener('click', () => {
      this.openSettingsModal();
    });

    // Listen to hash changes (e.g. #linux_node -> #win_node or #nodeId?secret=xxx or #nodeId?exp=xxx&sign=yyy)
    window.addEventListener('hashchange', () => {
      const rawHash = window.location.hash.replace(/^#\/?/, '').trim();
      if (rawHash) {
        const parts = rawHash.split(/[?&]/);
        const newHash = parts[0];
        let newSecret = '';
        let newExp = 0;
        let newSign = '';
        for (let i = 1; i < parts.length; i++) {
          const [k, v] = parts[i].split('=');
          if ((k === 'secret' || k === 'token') && v) {
            newSecret = decodeURIComponent(v);
          } else if (k === 'exp' && v) {
            newExp = parseInt(decodeURIComponent(v), 10) || 0;
          } else if (k === 'sign' && v) {
            newSign = decodeURIComponent(v);
          }
        }
        if (
          newHash &&
          (newHash !== this.peerId ||
            (newSecret && newSecret !== this.secret) ||
            (newSign && newSign !== this.shareSign))
        ) {
          this.log(`URL hash changed to #${rawHash}, switching peer...`);
          this.peerId = newHash;
          this.shareExp = newExp;
          this.shareSign = newSign;
          if (newSecret) {
            this.secret = newSecret;
            try {
              sessionStorage.setItem('p2p_secret', newSecret);
              localStorage.setItem('p2p_secret_' + newHash, newSecret);
            } catch {}
          } else if (!newSign) {
            this.secret = sessionStorage.getItem('p2p_secret') || localStorage.getItem('p2p_secret_' + newHash) || '';
          }
          try {
            sessionStorage.setItem('p2p_active_peer_id', newHash);
            localStorage.setItem('p2p_peer_id', newHash);
          } catch {}
          const sw = navigator.serviceWorker?.controller;
          if (sw) {
            sw.postMessage({ type: 'NODE_CHANGE', peerId: newHash, secret: this.secret });
          }
          this.reconnect();
        }
      }
    });
  }

  private async initServiceWorker() {
    if ('serviceWorker' in navigator) {
      try {
        this.log('Registering Service Worker (sw.js)...');
        this.swRegistration = await navigator.serviceWorker.register('/sw.js', { scope: '/' });
        await navigator.serviceWorker.ready;
        this.log('Service Worker is active and controlling scope /');

        const attachTunnel = () => {
          const sw = navigator.serviceWorker.controller;
          if (!sw) return;
          const channel = new MessageChannel();
          channel.port1.onmessage = (event) => {
            this.handleSWMessage(event);
          };
          sw.postMessage({ type: 'PORTAL_ATTACH', peerId: this.peerId, secret: this.secret }, [channel.port2]);
          this.log('Attached dedicated MessagePort tunnel to Service Worker');
        };

        attachTunnel();
        navigator.serviceWorker.addEventListener('controllerchange', attachTunnel);

        // Fallback listener for requests from Service Worker
        navigator.serviceWorker.addEventListener('message', (event) => {
          this.handleSWMessage(event);
        });
      } catch (err: any) {
        this.log(`Failed to register Service Worker: ${err.message}`);
      }
    } else {
      this.log('Warning: Service Worker not supported or disabled (Insecure Context).');
      this.showInsecureContextBanner();
    }
  }

  private startSignaling() {
    this.log(`Connecting to signaling server at ${this.signalUrl}...`);
    this.signalVal.textContent = 'Connecting...';

    this.ws = new WebSocket(this.signalUrl);

    this.ws.onopen = () => {
      this.log('Signaling WebSocket connected. Joining peer room...');
      this.signalVal.textContent = 'Connected (Joining...)';
      this.ws?.send(
        JSON.stringify({
          type: 'join',
          node_id: this.peerId,
          secret: this.secret,
        })
      );
    };

    this.ws.onmessage = async (event) => {
      try {
        const msg = JSON.parse(event.data);
        await this.handleSignalMessage(msg);
      } catch (err: any) {
        this.log(`Signaling parse error: ${err.message}`);
      }
    };

    this.ws.onerror = () => {
      this.log('Signaling WebSocket error.');
      this.signalVal.textContent = 'Error';
    };

    this.ws.onclose = () => {
      this.log('Signaling WebSocket closed.');
      this.signalVal.textContent = 'Disconnected';
    };
  }

  private async handleSignalMessage(msg: any) {
    switch (msg.type) {
      case 'peer_status':
        if (msg.node_id && msg.node_id !== this.peerId) {
          return;
        }
        if (msg.status === 'online') {
          this.hideOfflinePrompt();
          if (msg.requires_secret && !this.secret && !this.shareSign) {
            this.log(`Node [${this.peerId}] requires an access token (secret).`);
            if (this.signalVal) this.signalVal.textContent = 'Auth Required';
            this.showAuthPrompt(this.peerId);
            return;
          }
          this.hideAuthPrompt();
          this.log(`Target peer [${this.peerId}] is ONLINE. Initializing WebRTC Offer...`);
          if (this.signalVal) this.signalVal.textContent = 'Peer Online';
          await this.initWebRTC();
        } else {
          this.log(`Target peer [${this.peerId}] is OFFLINE.`);
          if (this.signalVal) this.signalVal.textContent = 'Peer Offline';
          this.showOfflinePrompt(this.peerId);
        }
        break;

      case 'answer':
        this.log('Received WebRTC Answer from Agent. Setting remote description...');
        if (this.pc && msg.sdp) {
          await this.pc.setRemoteDescription(new RTCSessionDescription(msg.sdp));
          this.log('Remote description applied successfully.');
        }
        break;

      case 'candidate':
        if (this.pc && msg.candidate) {
          await this.pc.addIceCandidate(new RTCIceCandidate(msg.candidate));
        }
        break;

      case 'error':
        this.log(`Signaling error message: ${msg.error}`);
        if (msg.error && msg.error.toLowerCase().includes('unauthorized')) {
          this.log(`Access rejected by node: ${msg.error}`);
          if (this.signalVal) this.signalVal.textContent = 'Auth Failed (401)';
          this.updateConnState('disconnected');
          const promptMsg = this.shareSign
            ? '分享链接已失效或签名不匹配，请输入访问口令'
            : '访问口令不正确或缺失，请重新输入';
          this.showAuthPrompt(this.peerId, promptMsg);
        }
        break;
    }
  }

  private async initWebRTC() {
    const iceServers: RTCIceServer[] = [
      { urls: 'stun:stun.qq.com:18302' },
      { urls: 'stun:stun.miwifi.com:3478' },
      { urls: 'stun:stun.bilibili.com:3478' },
      { urls: 'stun:stun.cloudflare.com:3478' },
      { urls: 'stun:stun.l.google.com:19302' },
    ];

    try {
      const customStun = localStorage.getItem('p2p_stun_servers');
      if (customStun) {
        customStun.split(',').forEach((s) => {
          s = s.trim();
          if (s) iceServers.unshift({ urls: s.startsWith('stun:') ? s : 'stun:' + s });
        });
      }

      const turnUrl = localStorage.getItem('p2p_turn_url');
      const turnUser = localStorage.getItem('p2p_turn_user');
      const turnPass = localStorage.getItem('p2p_turn_pass');
      if (turnUrl) {
        const server: RTCIceServer = {
          urls: turnUrl.startsWith('turn:') || turnUrl.startsWith('turns:') ? turnUrl : 'turn:' + turnUrl,
        };
        if (turnUser) {
          server.username = turnUser;
          server.credential = turnPass || '';
        }
        iceServers.push(server);
        this.log(`Configured TURN relay: ${server.urls}`);
      }
    } catch {}

    const config: RTCConfiguration = { iceServers };

    this.pc = new RTCPeerConnection(config);
    this.updateConnState('connecting');

    this.pc.onicecandidate = (event) => {
      if (event.candidate && this.ws?.readyState === WebSocket.OPEN) {
        this.ws.send(
          JSON.stringify({
            type: 'candidate',
            node_id: this.peerId,
            candidate: event.candidate,
          })
        );
      }
    };

    this.pc.oniceconnectionstatechange = () => {
      const state = this.pc?.iceConnectionState;
      this.log(`ICE Connection State: ${state}`);
      if (state === 'connected' || state === 'completed') {
        if (this.isIceRestarting) {
          this.log('ICE Restart succeeded! PeerConnection fully recovered with 0 session loss.');
          this.isIceRestarting = false;
          if (this.iceRestartTimer) clearTimeout(this.iceRestartTimer);
        }
        this.updateConnState('connected');
      } else if (state === 'disconnected') {
        this.log('ICE connection disconnected (transient or network roaming). Scheduling ICE Restart...');
        this.updateConnState('connecting');
        setTimeout(() => {
          if (this.pc?.iceConnectionState === 'disconnected' || this.pc?.iceConnectionState === 'failed') {
            this.triggerIceRestart();
          }
        }, 1200);
      } else if (state === 'failed') {
        this.log('ICE connection failed! Triggering immediate ICE Restart...');
        this.triggerIceRestart();
      }
    };

    this.pc.onconnectionstatechange = () => {
      const state = this.pc?.connectionState;
      this.log(`PeerConnection State: ${state}`);
      if (state === 'connected') {
        if (this.isIceRestarting) {
          this.isIceRestarting = false;
          if (this.iceRestartTimer) clearTimeout(this.iceRestartTimer);
        }
        this.updateConnState('connected');
      } else if (state === 'failed') {
        this.triggerIceRestart();
      }
    };

    // Create DataChannel with label "p2pmux"
    this.dc = this.pc.createDataChannel('p2pmux', {
      ordered: true,
    });

    this.setupDataChannel(this.dc);

    // Create Offer
    const offer = await this.pc.createOffer();
    await this.pc.setLocalDescription(offer);

    this.log('Generated WebRTC Offer. Sending to Agent via Signaling...');
    this.ws?.send(
      JSON.stringify({
        type: 'offer',
        node_id: this.peerId,
        sdp: offer,
        secret: this.secret,
        client_token: this.clientToken,
        exp: this.shareExp || undefined,
        sign: this.shareSign || undefined,
      })
    );
  }

  private setupDataChannel(dc: RTCDataChannel) {
    dc.onopen = () => {
      this.log('DataChannel [p2pmux] OPENED! Direct P2P tunnel established.');
      this.updateConnState('connected');

      this.mux = new P2PMux(dc, true);
      if (this.btnPing) this.btnPing.disabled = false;
      if (this.btnEcho) this.btnEcho.disabled = false;

      this.startPingLoop();
      this.mountAppViewport();
    };

    dc.onclose = () => {
      this.log('DataChannel closed.');
      this.updateConnState('disconnected');
      if (this.pingInterval) clearInterval(this.pingInterval);
      if (this.btnPing) this.btnPing.disabled = true;
      if (this.btnEcho) this.btnEcho.disabled = true;
    };

    dc.onerror = (err) => {
      this.log(`DataChannel error: ${err}`);
    };
  }

  private updateConnState(state: 'connected' | 'connecting' | 'disconnected') {
    if (this.connBadge) {
      this.connBadge.className = `badge ${state}`;
      this.connBadge.textContent = state.toUpperCase();
    }
  }

  private startPingLoop() {
    if (this.pingInterval) clearInterval(this.pingInterval);
    this.pingInterval = setInterval(async () => {
      if (!this.mux) return;
      try {
        const rtt = await this.mux.ping();
        if (this.rttVal) this.rttVal.textContent = `${rtt.toFixed(1)} ms`;
      } catch {}
    }, 2500);
  }

  // Mounts seamless full-screen app viewport iframe
  private mountAppViewport() {
    if (!('serviceWorker' in navigator) || (typeof window !== 'undefined' && !window.isSecureContext)) {
      this.log('Service Worker is inactive (Insecure Context). App viewport mounting blocked to prevent blank recursion.');
      this.showInsecureContextBanner();
      return;
    }

    const params = new URLSearchParams(window.location.search);
    const shouldMountFrame = params.get('mode') !== 'debug';
    if (!shouldMountFrame) return;

    const startPath = this.initialSubpath || params.get('path') || '/';
    if (this.appFrame) {
      this.appFrame.src = startPath;
      this.createFloatingPill();
      return;
    }

    this.log('Mounting App Shell Viewport iframe...');
    const frame = document.createElement('iframe');
    frame.id = 'appViewport';
    frame.style.cssText = `
      position: fixed;
      top: 0;
      left: 0;
      width: 100vw;
      height: 100vh;
      border: none;
      z-index: 100;
      background: #ffffff;
    `;
    // Hardware browser-level anti-frame-busting via sandbox:
    // By deliberately omitting 'allow-top-navigation', scripts cannot hijack the outer tunnel window
    frame.setAttribute(
      'sandbox',
      'allow-scripts allow-forms allow-same-origin allow-downloads allow-modals allow-popups allow-popups-to-escape-sandbox allow-presentation'
    );
    frame.setAttribute(
      'allow',
      'fullscreen; clipboard-read; clipboard-write; autoplay; camera; microphone; display-capture; web-share; publickey-credentials-get; publickey-credentials-create; identity-credentials-get; cross-origin-isolated; storage-access; encrypted-media; presentation; screen-wake-lock; picture-in-picture'
    );
    frame.setAttribute('allowfullscreen', 'true');

    frame.src = startPath; // Intercepted by Service Worker
    document.body.appendChild(frame);
    this.appFrame = frame;

    frame.addEventListener('load', () => {
      try {
        if (frame.contentDocument?.title) {
          document.title = frame.contentDocument.title;
        }
      } catch {}
      try {
        frame.contentWindow?.postMessage(
          {
            type: 'P2P_PORTAL_CONTEXT',
            nodeId: this.peerId,
            portalOrigin: window.location.origin,
            secret: this.secret,
          },
          '*'
        );
      } catch {}
    });

    // Create floating status pill
    this.createFloatingPill();
  }

  private showInsecureContextBanner() {
    if (document.getElementById('insecureBanner')) return;
    const banner = document.createElement('div');
    banner.id = 'insecureBanner';
    banner.style.cssText = `
      position: fixed;
      top: 20px;
      left: 50%;
      transform: translateX(-50%);
      width: 90%;
      max-width: 680px;
      background: #1c1917;
      border: 1px solid #d29922;
      border-radius: 10px;
      padding: 18px 22px;
      color: #f0f6fc;
      z-index: 10000;
      box-shadow: 0 12px 32px rgba(0,0,0,0.6);
      font-size: 13px;
      line-height: 1.6;
    `;
    const curHost = window.location.host;
    const curPort = window.location.port || '8888';
    banner.innerHTML = `
      <div style="display: flex; align-items: flex-start; gap: 14px;">
        <span style="font-size: 26px; line-height: 1;">⚠️</span>
        <div style="flex: 1;">
          <h4 style="font-size: 15px; font-weight: 600; color: #e3b341; margin-bottom: 6px;">
            当前处于非安全上下文（Insecure Context），已暂停加载目标网页
          </h4>
          <p style="color: #c9d1d9; margin-bottom: 10px;">
            根据 W3C Web 标准与现代浏览器安全规范：在非 HTTPS 且非 localhost 的局域网纯 HTTP IP（如 <code>${window.location.origin}</code>）下，<strong>Service Worker 将被强制禁用</strong>。本网关依赖 Service Worker 虚拟化拦截和转发请求，请选择以下任一方式使用：
          </p>
          <div style="background: #0d1117; border: 1px solid #30363d; border-radius: 6px; padding: 12px; margin-bottom: 10px;">
            <div style="margin-bottom: 8px;">
              <span style="color: #58a6ff; font-weight: 600;">方案 1（本机访问，最快）</span>：若浏览器与信令服务运行在同一台设备，请直接在地址栏打开 
              <a href="http://localhost:${curPort}/#${this.peerId}" style="color: #388bfd; text-decoration: underline;">http://localhost:${curPort}/#${this.peerId}</a> 或 <code>http://127.0.0.1:${curPort}/#${this.peerId}</code>（localhost 属于标准安全上下文）。
            </div>
            <div style="margin-bottom: 8px;">
              <span style="color: #3fb950; font-weight: 600;">方案 2（生产推荐，零配置）</span>：部署在支持 HTTPS 的域名或免费部署至 Cloudflare Workers（如 <code>https://p2p.yourdomain.com/#${this.peerId}</code>）。
            </div>
            <div>
              <span style="color: #d29922; font-weight: 600;">方案 3（局域网跨设备 HTTP 免证书测试）</span>：在 Chrome/Edge 浏览器地址栏打开：<br/>
              <code>chrome://flags/#unsafely-treat-insecure-origin-as-secure</code>（Edge 为 <code>edge://flags/...</code>）<br/>
              将 <code>http://${curHost}</code> 填入白名单输入框，并在右侧下拉框选择 <strong>Enabled</strong>，点击 <strong>Relaunch</strong> 重启浏览器即可！
            </div>
          </div>
          <div style="display: flex; justify-content: flex-end;">
            <button id="btnCloseInsecureBanner" style="background: #21262d; border: 1px solid #30363d; color: #c9d1d9; padding: 6px 14px; border-radius: 6px; font-size: 12px; cursor: pointer;">
              保留在控制台调试视图
            </button>
          </div>
        </div>
      </div>
    `;
    document.body.appendChild(banner);
    document.getElementById('btnCloseInsecureBanner')?.addEventListener('click', () => {
      banner.style.display = 'none';
    });
  }

  private startHUDStatsMonitor() {
    if (this.hudStatsTimer) return;
    this.hudStatsTimer = setInterval(async () => {
      await this.updateWebRTCStats();
    }, 1000);
  }

  private async updateWebRTCStats() {
    if (!this.pc) return;
    try {
      const stats = await this.pc.getStats();
      let selectedPair: any = null;
      const candidates = new Map<string, any>();

      stats.forEach((report: any) => {
        if (report.type === 'candidate-pair' && (report.selected || (report.nominated && report.state === 'succeeded'))) {
          selectedPair = report;
        }
        if (report.type === 'local-candidate' || report.type === 'remote-candidate') {
          candidates.set(report.id, report);
        }
      });

      if (!selectedPair) {
        stats.forEach((report: any) => {
          if (report.type === 'candidate-pair' && report.state === 'succeeded') {
            selectedPair = report;
          }
        });
      }

      const now = performance.now();
      const timeDelta = this.lastStatsTime > 0 ? (now - this.lastStatsTime) / 1000 : 1;

      let rxRate = 0;
      let txRate = 0;
      let totalRx = 0;
      let totalTx = 0;
      let rtt = 0;

      if (selectedPair) {
        totalRx = selectedPair.bytesReceived || 0;
        totalTx = selectedPair.bytesSent || 0;
        if (this.lastStatsTime > 0 && timeDelta > 0) {
          rxRate = Math.max(0, (totalRx - this.lastBytesReceived) / 1024 / timeDelta);
          txRate = Math.max(0, (totalTx - this.lastBytesSent) / 1024 / timeDelta);
        }
        this.lastBytesReceived = totalRx;
        this.lastBytesSent = totalTx;
        this.lastStatsTime = now;

        if (typeof selectedPair.currentRoundTripTime === 'number') {
          rtt = Math.round(selectedPair.currentRoundTripTime * 1000);
        } else if (typeof selectedPair.totalRoundTripTime === 'number' && selectedPair.responsesReceived > 0) {
          rtt = Math.round((selectedPair.totalRoundTripTime / selectedPair.responsesReceived) * 1000);
        }

        const localCand = candidates.get(selectedPair.localCandidateId);
        const remoteCand = candidates.get(selectedPair.remoteCandidateId);

        const localType = (localCand?.candidateType || '').toLowerCase();
        const remoteType = (remoteCand?.candidateType || '').toLowerCase();
        const isRelay = localType === 'relay' || remoteType === 'relay';

        this.hudInfo = {
          transportType: isRelay ? 'relay' : 'direct',
          localType: localType || 'host',
          remoteType: remoteType || 'srflx',
          localAddress: localCand ? `${localCand.address || localCand.ip || '?'}:${localCand.port || '?'}` : 'Unknown',
          remoteAddress: remoteCand ? `${remoteCand.address || remoteCand.ip || '?'}:${remoteCand.port || '?'}` : 'Unknown',
          protocol: (localCand?.protocol || 'udp').toUpperCase(),
          rttMs: rtt || (this.rttVal ? parseInt(this.rttVal.textContent || '0') : 0),
          rxRateKB: parseFloat(rxRate.toFixed(1)),
          txRateKB: parseFloat(txRate.toFixed(1)),
          totalRxMB: parseFloat((totalRx / (1024 * 1024)).toFixed(2)),
          totalTxMB: parseFloat((totalTx / (1024 * 1024)).toFixed(2)),
          packetsLost: selectedPair.packetsLost || 0,
          iceState: this.pc.iceConnectionState || 'connected',
          connState: this.pc.connectionState || 'connected',
        };
      } else {
        this.hudInfo.iceState = this.pc.iceConnectionState;
        this.hudInfo.connState = this.pc.connectionState;
        if (this.rttVal) {
          const parsed = parseInt(this.rttVal.textContent || '0');
          if (parsed > 0) this.hudInfo.rttMs = parsed;
        }
      }

      this.updateHUDUI();
    } catch {
      // Ignore poll error during ICE reconnection
    }
  }

  private openHUDModal() {
    const modal = document.getElementById('p2pHudModal');
    if (modal) {
      modal.style.display = 'flex';
      this.updateHUDUI();
    }
  }

  private closeHUDModal() {
    const modal = document.getElementById('p2pHudModal');
    if (modal) modal.style.display = 'none';
  }

  private createHUDModal() {
    if (document.getElementById('p2pHudModal')) return;
    const modal = document.createElement('div');
    modal.id = 'p2pHudModal';
    modal.style.cssText = `
      position: fixed;
      top: 0; left: 0; width: 100vw; height: 100vh;
      background: rgba(0,0,0,0.75);
      backdrop-filter: blur(8px);
      display: none; justify-content: center; align-items: center;
      z-index: 20000;
      font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, monospace;
    `;
    modal.innerHTML = `
      <div style="background: #161b22; border: 1px solid #30363d; border-radius: 14px; padding: 24px; max-width: 520px; width: 92%; color: #c9d1d9; box-shadow: 0 20px 50px rgba(0,0,0,0.7);">
        <div style="display: flex; justify-content: space-between; align-items: center; margin-bottom: 20px;">
          <div style="display: flex; align-items: center; gap: 8px;">
            <span style="font-size: 18px;">⚡</span>
            <h3 style="font-size: 16px; color: #f0f6fc; font-weight: 600; margin: 0;">WebRTC Connection Diagnostic HUD</h3>
          </div>
          <button id="btnCloseHud" style="background: none; border: none; color: #8b949e; font-size: 20px; cursor: pointer; padding: 0 4px;">✕</button>
        </div>

        <!-- Section 1: Route & Mode -->
        <div style="background: #0d1117; border: 1px solid #21262d; border-radius: 10px; padding: 14px; margin-bottom: 16px;">
          <div style="display: flex; justify-content: space-between; align-items: center; margin-bottom: 10px;">
            <span style="font-size: 12px; color: #8b949e; text-transform: uppercase; font-weight: 600;">传输连接模式 (Transport Mode)</span>
            <span id="hudBadge" style="display: inline-flex; align-items: center; gap: 4px; font-weight: 700; padding: 3px 10px; border-radius: 12px; font-size: 12px; background: #238636; color: #ffffff;">⚡ P2P Direct 直连</span>
          </div>
          <div style="display: grid; grid-template-columns: 1fr 1fr; gap: 10px; font-size: 12px;">
            <div>
              <div style="color: #8b949e; font-size: 11px;">本地节点候选 (Local)</div>
              <div id="hudLocalCand" style="color: #f0f6fc; font-family: monospace; font-size: 11px; margin-top: 2px;">--</div>
            </div>
            <div>
              <div style="color: #8b949e; font-size: 11px;">远端穿透目标 (Remote)</div>
              <div id="hudRemoteCand" style="color: #f0f6fc; font-family: monospace; font-size: 11px; margin-top: 2px;">--</div>
            </div>
          </div>
          <div style="display: flex; gap: 16px; margin-top: 10px; padding-top: 8px; border-top: 1px solid #21262d; font-size: 11px; color: #8b949e;">
            <span>协议: <strong id="hudProto" style="color: #58a6ff;">UDP</strong></span>
            <span>ICE 状态: <strong id="hudIceState" style="color: #3fb950;">connected</strong></span>
            <span>节点: <strong id="hudNodeId" style="color: #f0f6fc;">${this.peerId}</strong></span>
          </div>
        </div>

        <!-- Section 2: Realtime Metrics Cards -->
        <div style="display: grid; grid-template-columns: 1fr 1fr; gap: 10px; margin-bottom: 20px;">
          <div style="background: #0d1117; border: 1px solid #21262d; border-radius: 8px; padding: 12px;">
            <div style="font-size: 11px; color: #8b949e;">往返网络延迟 (RTT)</div>
            <div id="hudRtt" style="font-size: 20px; font-weight: 700; color: #58a6ff; font-family: monospace; margin-top: 4px;">-- ms</div>
          </div>
          <div style="background: #0d1117; border: 1px solid #21262d; border-radius: 8px; padding: 12px;">
            <div style="font-size: 11px; color: #8b949e;">瞬时下载速率 (Downlink)</div>
            <div id="hudRxRate" style="font-size: 20px; font-weight: 700; color: #3fb950; font-family: monospace; margin-top: 4px;">0 KB/s</div>
            <div id="hudTotalRx" style="font-size: 10px; color: #8b949e; margin-top: 2px;">累计: 0 MB</div>
          </div>
          <div style="background: #0d1117; border: 1px solid #21262d; border-radius: 8px; padding: 12px;">
            <div style="font-size: 11px; color: #8b949e;">瞬时上传速率 (Uplink)</div>
            <div id="hudTxRate" style="font-size: 20px; font-weight: 700; color: #d29922; font-family: monospace; margin-top: 4px;">0 KB/s</div>
            <div id="hudTotalTx" style="font-size: 10px; color: #8b949e; margin-top: 2px;">累计: 0 MB</div>
          </div>
          <div style="background: #0d1117; border: 1px solid #21262d; border-radius: 8px; padding: 12px;">
            <div style="font-size: 11px; color: #8b949e;">丢包统计 (Packet Loss)</div>
            <div id="hudPacketsLost" style="font-size: 20px; font-weight: 700; color: #f0f6fc; font-family: monospace; margin-top: 4px;">0 pkts</div>
          </div>
        </div>

        <!-- Section 3: Actions -->
        <div style="display: flex; gap: 10px;">
          <button id="btnHudIceRestart" style="flex: 1; padding: 10px 14px; background: #21262d; border: 1px solid #30363d; color: #58a6ff; border-radius: 8px; font-size: 13px; font-weight: 600; cursor: pointer; transition: all 0.2s;">🔄 测试 ICE Restart (切网自愈)</button>
          <button id="btnHudOpenSettings" style="padding: 10px 14px; background: #238636; border: none; color: #ffffff; border-radius: 8px; font-size: 13px; font-weight: 600; cursor: pointer;">⚙ 连接设置</button>
        </div>
        <div id="hudActionFeedback" style="font-size: 11px; color: #3fb950; margin-top: 8px; text-align: center; display: none;"></div>
      </div>
    `;

    document.body.appendChild(modal);

    document.getElementById('btnCloseHud')?.addEventListener('click', () => this.closeHUDModal());
    modal.addEventListener('click', (e) => {
      if (e.target === modal) this.closeHUDModal();
    });

    document.getElementById('btnHudIceRestart')?.addEventListener('click', async () => {
      const fb = document.getElementById('hudActionFeedback');
      if (fb) {
        fb.style.display = 'block';
        fb.style.color = '#58a6ff';
        fb.textContent = '🔄 正在触发 WebRTC 原地 ICE Restart 重协商...';
      }
      try {
        await this.triggerIceRestart();
        if (fb) {
          fb.style.color = '#3fb950';
          fb.textContent = '✅ ICE Restart 重协商成功，数据通道与会话无感保持！';
          setTimeout(() => { if (fb) fb.style.display = 'none'; }, 4000);
        }
      } catch (err: any) {
        if (fb) {
          fb.style.color = '#f85149';
          fb.textContent = `❌ ICE Restart 失败: ${err?.message || err}`;
        }
      }
    });

    document.getElementById('btnHudOpenSettings')?.addEventListener('click', () => {
      this.closeHUDModal();
      this.openSettingsModal();
    });
  }

  private updateHUDUI() {
    const isRelay = this.hudInfo.transportType === 'relay';

    // Update Floating Pill
    const pillBadge = document.getElementById('pillModeBadge');
    if (pillBadge) {
      if (isRelay) {
        pillBadge.textContent = '🛡️ TURN';
        pillBadge.style.background = '#d29922';
      } else {
        pillBadge.textContent = '⚡ P2P';
        pillBadge.style.background = '#238636';
      }
    }

    const pillNode = document.getElementById('pillNode');
    if (pillNode) pillNode.textContent = `P2P: ${this.peerId}`;

    const pillRtt = document.getElementById('pillRtt');
    if (pillRtt) {
      pillRtt.textContent = `${this.hudInfo.rttMs || '--'} ms`;
    }

    const pillSpeed = document.getElementById('pillSpeed');
    if (pillSpeed) {
      pillSpeed.textContent = `↓ ${this.hudInfo.rxRateKB} KB/s`;
    }

    // Update HUD Modal if open
    const hudBadge = document.getElementById('hudBadge');
    if (hudBadge) {
      if (isRelay) {
        hudBadge.textContent = '🛡️ TURN RELAY (加密中继)';
        hudBadge.style.background = '#d29922';
      } else {
        hudBadge.textContent = '⚡ P2P DIRECT (打洞直连)';
        hudBadge.style.background = '#238636';
      }
    }

    const hudLocalCand = document.getElementById('hudLocalCand');
    if (hudLocalCand) hudLocalCand.textContent = `${this.hudInfo.localType} (${this.hudInfo.localAddress})`;

    const hudRemoteCand = document.getElementById('hudRemoteCand');
    if (hudRemoteCand) hudRemoteCand.textContent = `${this.hudInfo.remoteType} (${this.hudInfo.remoteAddress})`;

    const hudProto = document.getElementById('hudProto');
    if (hudProto) hudProto.textContent = this.hudInfo.protocol;

    const hudIceState = document.getElementById('hudIceState');
    if (hudIceState) hudIceState.textContent = this.hudInfo.iceState;

    const hudNodeId = document.getElementById('hudNodeId');
    if (hudNodeId) hudNodeId.textContent = this.peerId;

    const hudRtt = document.getElementById('hudRtt');
    if (hudRtt) hudRtt.textContent = `${this.hudInfo.rttMs || '--'} ms`;

    const hudRxRate = document.getElementById('hudRxRate');
    if (hudRxRate) hudRxRate.textContent = `${this.hudInfo.rxRateKB} KB/s`;

    const hudTotalRx = document.getElementById('hudTotalRx');
    if (hudTotalRx) hudTotalRx.textContent = `累计: ${this.hudInfo.totalRxMB} MB`;

    const hudTxRate = document.getElementById('hudTxRate');
    if (hudTxRate) hudTxRate.textContent = `${this.hudInfo.txRateKB} KB/s`;

    const hudTotalTx = document.getElementById('hudTotalTx');
    if (hudTotalTx) hudTotalTx.textContent = `累计: ${this.hudInfo.totalTxMB} MB`;

    const hudPacketsLost = document.getElementById('hudPacketsLost');
    if (hudPacketsLost) hudPacketsLost.textContent = `${this.hudInfo.packetsLost} pkts`;
  }

  private createFloatingPill() {
    this.startHUDStatsMonitor();
    this.createHUDModal();
    this.createSettingsModal();

    if (document.getElementById('p2pPill')) {
      const pillNode = document.getElementById('pillNode');
      if (pillNode) pillNode.textContent = `P2P: ${this.peerId}`;
      return;
    }

    const pill = document.createElement('div');
    pill.id = 'p2pPill';
    pill.style.cssText = `
      position: fixed;
      bottom: 16px;
      right: 16px;
      background: rgba(22, 27, 34, 0.94);
      backdrop-filter: blur(10px);
      border: 1px solid #30363d;
      color: #c9d1d9;
      padding: 6px 14px;
      border-radius: 20px;
      font-size: 12px;
      font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, monospace;
      z-index: 10000;
      box-shadow: 0 4px 16px rgba(0,0,0,0.5);
      display: flex;
      align-items: center;
      gap: 8px;
      user-select: none;
      transition: all 0.2s ease;
      cursor: pointer;
    `;
    pill.innerHTML = `
      <span id="pillModeBadge" style="display:inline-flex; align-items:center; gap:3px; font-weight:700; padding:2px 7px; border-radius:10px; font-size:11px; background:#238636; color:#ffffff; letter-spacing:0.5px;">⚡ P2P</span>
      <span id="pillNode" style="font-weight:500;" title="Click to open WebRTC HUD">P2P: ${this.peerId}</span>
      <span id="pillRtt" style="color:#58a6ff; font-family:monospace;">-- ms</span>
      <span id="pillSpeed" style="color:#8b949e; font-size:11px; font-family:monospace;">↓ 0 KB/s</span>
      <span id="pillGear" style="cursor:pointer; opacity:0.75; font-size:13px; margin-left:2px;" title="Connection Settings">⚙</span>
    `;

    pill.addEventListener('click', (e) => {
      const target = e.target as HTMLElement;
      if (target && target.id === 'pillGear') {
        this.openSettingsModal();
      } else {
        this.openHUDModal();
      }
    });

    document.body.appendChild(pill);
  }

  private createSettingsModal() {
    if (document.getElementById('p2pSettingsModal')) return;
    const modal = document.createElement('div');
    modal.id = 'p2pSettingsModal';
    modal.style.cssText = `
      position: fixed;
      top: 0; left: 0; width: 100vw; height: 100vh;
      background: rgba(0,0,0,0.7);
      backdrop-filter: blur(6px);
      display: none; justify-content: center; align-items: center;
      z-index: 20000;
      font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, monospace;
    `;
    modal.innerHTML = `
      <div style="background: #161b22; border: 1px solid #30363d; border-radius: 12px; padding: 24px; max-width: 440px; width: 90%; color: #c9d1d9; box-shadow: 0 16px 40px rgba(0,0,0,0.6);">
        <div style="display: flex; justify-content: space-between; align-items: center; margin-bottom: 20px;">
          <h3 style="font-size: 16px; color: #f0f6fc; font-weight: 600;">⚙ P2P Gateway Settings</h3>
          <button id="btnCloseSettings" style="background: none; border: none; color: #8b949e; font-size: 18px; cursor: pointer;">✕</button>
        </div>
        <div style="margin-bottom: 16px;">
          <label style="display: block; font-size: 12px; color: #8b949e; text-transform: uppercase; margin-bottom: 4px; font-weight: 600;">目标节点 ID (Target Node ID)</label>
          <div style="font-size: 11px; color: #8b949e; margin-bottom: 8px;">支持二级/三级子域名（如 <code>bili.p2p.example.com</code>）、URL Hash（#nodeId）或手动指定。</div>
          <input id="inputNodeId" type="text" value="${this.peerId}" placeholder="e.g. myhome" style="width: 100%; box-sizing: border-box; background: #0d1117; border: 1px solid #30363d; color: #f0f6fc; padding: 8px 12px; border-radius: 6px; font-size: 14px; font-family: monospace;" />
          <div style="display: flex; flex-wrap: wrap; gap: 6px; margin-top: 8px;" id="nodeChips">
            <span class="node-chip" data-node="librespeed" style="cursor: pointer; font-size: 11px; padding: 3px 8px; border-radius: 12px; background: #21262d; border: 1px solid #30363d; color: #58a6ff;">librespeed</span>
            <span class="node-chip" data-node="vaultwarden" style="cursor: pointer; font-size: 11px; padding: 3px 8px; border-radius: 12px; background: #21262d; border: 1px solid #30363d; color: #58a6ff;">vaultwarden</span>
            <span class="node-chip" data-node="immich" style="cursor: pointer; font-size: 11px; padding: 3px 8px; border-radius: 12px; background: #21262d; border: 1px solid #30363d; color: #58a6ff;">immich</span>
            <span class="node-chip" data-node="jellyfin" style="cursor: pointer; font-size: 11px; padding: 3px 8px; border-radius: 12px; background: #21262d; border: 1px solid #30363d; color: #58a6ff;">jellyfin</span>
            <span class="node-chip" data-node="openlist" style="cursor: pointer; font-size: 11px; padding: 3px 8px; border-radius: 12px; background: #21262d; border: 1px solid #30363d; color: #58a6ff;">openlist</span>
            <span class="node-chip" data-node="portainer" style="cursor: pointer; font-size: 11px; padding: 3px 8px; border-radius: 12px; background: #21262d; border: 1px solid #30363d; color: #58a6ff;">portainer</span>
            <span class="node-chip" data-node="bili" style="cursor: pointer; font-size: 11px; padding: 3px 8px; border-radius: 12px; background: #21262d; border: 1px solid #30363d; color: #58a6ff;">bilibili</span>
          </div>
        </div>
        <div style="margin-bottom: 20px;">
          <label style="display: block; font-size: 12px; color: #8b949e; text-transform: uppercase; margin-bottom: 4px; font-weight: 600;">信令服务端地址 (Signaling Server URL)</label>
          <div style="font-size: 11px; color: #8b949e; margin-bottom: 8px;">设置 WebRTC 握手撮合信令服务器 WebSocket 地址。</div>
          <input id="inputSignalUrl" type="text" value="${this.signalUrl}" placeholder="wss://..." style="width: 100%; box-sizing: border-box; background: #0d1117; border: 1px solid #30363d; color: #f0f6fc; padding: 8px 12px; border-radius: 6px; font-size: 13px; font-family: monospace;" />
          <div style="display: flex; flex-wrap: wrap; gap: 6px; margin-top: 8px;" id="signalChips">
            <span class="signal-chip" data-signal="current" style="cursor: pointer; font-size: 11px; padding: 3px 8px; border-radius: 12px; background: #21262d; border: 1px solid #30363d; color: #58a6ff;">当前同源信令 (Same-Origin /ws)</span>
            <span class="signal-chip" data-signal="ws://127.0.0.1:8888/ws" style="cursor: pointer; font-size: 11px; padding: 3px 8px; border-radius: 12px; background: #21262d; border: 1px solid #30363d; color: #58a6ff;">本地信令 (localhost:8888)</span>
          </div>
        </div>
        <div style="margin-bottom: 20px;">
          <label style="display: block; font-size: 12px; color: #8b949e; text-transform: uppercase; margin-bottom: 4px; font-weight: 600;">访问口令 / 密钥 (Secret Token - 可选)</label>
          <div style="font-size: 11px; color: #8b949e; margin-bottom: 8px;">若目标节点开启了 <code>-secret</code> 保护，请输入匹配的口令；留空表示公开访问。</div>
          <input id="inputSecret" type="password" value="${this.secret}" placeholder="Optional Secret Token" style="width: 100%; box-sizing: border-box; background: #0d1117; border: 1px solid #30363d; color: #f0f6fc; padding: 8px 12px; border-radius: 6px; font-size: 13px; font-family: monospace;" />
        </div>
        <div style="margin-bottom: 20px;">
          <label style="display: block; font-size: 12px; color: #8b949e; text-transform: uppercase; margin-bottom: 4px; font-weight: 600;">TURN 中继穿透 (Symmetric NAT Relay - 可选)</label>
          <div style="font-size: 11px; color: #8b949e; margin-bottom: 8px;">在严格对称型 NAT（企业网/校园网/移动网络）打洞失败时无缝降级走中继。</div>
          <input id="inputTurnUrl" type="text" placeholder="turn:turn.domain.com:3478" style="width: 100%; box-sizing: border-box; background: #0d1117; border: 1px solid #30363d; color: #f0f6fc; padding: 6px 12px; border-radius: 6px; font-size: 12px; font-family: monospace; margin-bottom: 6px;" />
          <div style="display: flex; gap: 8px;">
            <input id="inputTurnUser" type="text" placeholder="Username" style="flex: 1; box-sizing: border-box; background: #0d1117; border: 1px solid #30363d; color: #f0f6fc; padding: 6px 10px; border-radius: 6px; font-size: 12px; font-family: monospace;" />
            <input id="inputTurnPass" type="password" placeholder="Password" style="flex: 1; box-sizing: border-box; background: #0d1117; border: 1px solid #30363d; color: #f0f6fc; padding: 6px 10px; border-radius: 6px; font-size: 12px; font-family: monospace;" />
          </div>
        </div>
        <div style="margin-bottom: 20px; border-top: 1px solid #30363d; padding-top: 16px;">
          <label style="display: block; font-size: 12px; color: #8b949e; text-transform: uppercase; margin-bottom: 4px; font-weight: 600;">🔗 限时安全分享链接 (Stateless Signed Share Link)</label>
          <div style="font-size: 11px; color: #8b949e; margin-bottom: 8px;">基于 HMAC-SHA256 纯数学时效签名，无需对方输入密钥，到期自动失效。</div>
          <div style="display: flex; gap: 8px; margin-bottom: 8px;">
            <select id="selShareDuration" style="flex: 1; background: #0d1117; border: 1px solid #30363d; color: #f0f6fc; padding: 6px 10px; border-radius: 6px; font-size: 12px;">
              <option value="3600">有效 1 小时 (1 Hour)</option>
              <option value="21600">有效 6 小时 (6 Hours)</option>
              <option value="86400" selected>有效 24 小时 (1 Day)</option>
              <option value="604800">有效 7 天 (7 Days)</option>
              <option value="2592000">有效 30 天 (30 Days)</option>
            </select>
            <button id="btnGenShareLink" type="button" style="background: #238636; color: #ffffff; border: none; padding: 6px 14px; border-radius: 6px; font-size: 12px; font-weight: 600; cursor: pointer;">生成链接</button>
          </div>
          <div id="shareLinkContainer" style="display: none; flex-direction: column; gap: 6px;">
            <div style="display: flex; gap: 6px;">
              <input id="inputShareLink" type="text" readonly style="flex: 1; background: #0d1117; border: 1px solid #238636; color: #7ee787; padding: 6px 10px; border-radius: 6px; font-size: 11px; font-family: monospace;" />
              <button id="btnCopyShareLink" type="button" style="background: #21262d; border: 1px solid #30363d; color: #58a6ff; padding: 6px 12px; border-radius: 6px; font-size: 12px; cursor: pointer;">复制</button>
            </div>
            <div id="shareCopyTip" style="font-size: 11px; color: #3fb950; display: none;">✓ 已复制到剪贴板！可以直接发送给他人访问。</div>
          </div>
        </div>
        <div style="display: flex; gap: 10px; justify-content: flex-end;">
          <button id="btnToggleDebugView" style="background: #21262d; border: 1px solid #30363d; color: #c9d1d9; padding: 8px 14px; border-radius: 6px; font-size: 13px; cursor: pointer;">Toggle Debug View</button>
          <button id="btnSaveReconnect" style="background: #58a6ff; color: #0d1117; font-weight: 600; border: none; padding: 8px 16px; border-radius: 6px; font-size: 13px; cursor: pointer;">Save & Connect</button>
        </div>
      </div>
    `;
    document.body.appendChild(modal);

    document.querySelectorAll('.node-chip').forEach((chip) => {
      chip.addEventListener('click', () => {
        const targetNode = chip.getAttribute('data-node');
        const inputNode = document.getElementById('inputNodeId') as HTMLInputElement;
        if (targetNode && inputNode) {
          inputNode.value = targetNode;
        }
      });
    });

    document.querySelectorAll('.signal-chip').forEach((chip) => {
      chip.addEventListener('click', () => {
        let targetSignal = chip.getAttribute('data-signal');
        if (targetSignal === 'current') {
          const proto = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
          targetSignal = `${proto}//${window.location.host}/ws`;
        }
        const inputSignal = document.getElementById('inputSignalUrl') as HTMLInputElement;
        if (targetSignal && inputSignal) {
          inputSignal.value = targetSignal;
        }
      });
    });

    document.getElementById('btnCloseSettings')?.addEventListener('click', () => {
      modal.style.display = 'none';
    });

    modal.addEventListener('click', (e) => {
      if (e.target === modal) modal.style.display = 'none';
    });

    document.getElementById('btnToggleDebugView')?.addEventListener('click', () => {
      if (this.appFrame) {
        const isHidden = this.appFrame.style.display === 'none';
        this.appFrame.style.display = isHidden ? 'block' : 'none';
      }
      modal.style.display = 'none';
    });

    document.getElementById('btnSaveReconnect')?.addEventListener('click', () => {
      const inputNode = document.getElementById('inputNodeId') as HTMLInputElement;
      const inputSignal = document.getElementById('inputSignalUrl') as HTMLInputElement;
      const inputSecret = document.getElementById('inputSecret') as HTMLInputElement;
      const inputTurnUrl = document.getElementById('inputTurnUrl') as HTMLInputElement;
      const inputTurnUser = document.getElementById('inputTurnUser') as HTMLInputElement;
      const inputTurnPass = document.getElementById('inputTurnPass') as HTMLInputElement;

      const newNodeId = inputNode?.value.trim() || DEFAULT_NODE_ID;
      const newSignalUrl = inputSignal?.value.trim() || DEFAULT_SIGNAL_URL;
      const newSecret = inputSecret ? inputSecret.value.trim() : '';

      this.peerId = newNodeId;
      this.signalUrl = newSignalUrl;
      this.secret = newSecret;
      try {
        sessionStorage.setItem('p2p_active_peer_id', newNodeId);
        localStorage.setItem('p2p_peer_id', newNodeId);
        localStorage.setItem('p2p_signal_url', newSignalUrl);
        if (newSecret) {
          sessionStorage.setItem('p2p_secret', newSecret);
          localStorage.setItem('p2p_secret_' + newNodeId, newSecret);
        } else {
          sessionStorage.removeItem('p2p_secret');
          localStorage.removeItem('p2p_secret_' + newNodeId);
        }

        if (inputTurnUrl && inputTurnUrl.value.trim()) {
          localStorage.setItem('p2p_turn_url', inputTurnUrl.value.trim());
          localStorage.setItem('p2p_turn_user', inputTurnUser?.value.trim() || '');
          localStorage.setItem('p2p_turn_pass', inputTurnPass?.value.trim() || '');
        } else {
          localStorage.removeItem('p2p_turn_url');
          localStorage.removeItem('p2p_turn_user');
          localStorage.removeItem('p2p_turn_pass');
        }
      } catch {}

      const sw = navigator.serviceWorker?.controller;
      if (sw) {
        sw.postMessage({ type: 'NODE_CHANGE', peerId: newNodeId, secret: newSecret });
      }

      modal.style.display = 'none';
      this.hideOfflinePrompt();
      this.hideAuthPrompt();
      this.reconnect();
    });

    document.getElementById('btnGenShareLink')?.addEventListener('click', async () => {
      const inputNode = document.getElementById('inputNodeId') as HTMLInputElement;
      const inputSecret = document.getElementById('inputSecret') as HTMLInputElement;
      const selDuration = document.getElementById('selShareDuration') as HTMLSelectElement;
      const container = document.getElementById('shareLinkContainer');
      const inputLink = document.getElementById('inputShareLink') as HTMLInputElement;
      const tip = document.getElementById('shareCopyTip');

      const targetNode = inputNode?.value.trim() || this.peerId;
      const secretKey = inputSecret?.value.trim() || this.secret;
      const durationSec = parseInt(selDuration?.value || '86400', 10);

      const base = window.location.origin + window.location.pathname;

      if (!secretKey) {
        // Node has no secret configured
        const directLink = `${base}#${targetNode}`;
        if (inputLink) inputLink.value = directLink;
        if (container) container.style.display = 'flex';
        return;
      }

      try {
        const exp = Math.floor(Date.now() / 1000) + durationSec;
        const payload = `${targetNode}:${exp}`;
        const sign = await computeHMACSHA256Hex(secretKey, payload);
        const signedLink = `${base}#${targetNode}?exp=${exp}&sign=${sign}`;

        if (inputLink) {
          inputLink.value = signedLink;
          inputLink.select();
        }
        if (container) {
          container.style.display = 'flex';
        }
        if (tip) {
          tip.style.display = 'none';
        }
      } catch (err: any) {
        alert('生成签名链接失败: ' + err.message);
      }
    });

    document.getElementById('btnCopyShareLink')?.addEventListener('click', async () => {
      const inputLink = document.getElementById('inputShareLink') as HTMLInputElement;
      const tip = document.getElementById('shareCopyTip');
      if (inputLink && inputLink.value) {
        try {
          await navigator.clipboard.writeText(inputLink.value);
          if (tip) {
            tip.style.display = 'block';
            setTimeout(() => {
              if (tip) tip.style.display = 'none';
            }, 3000);
          }
        } catch {
          inputLink.select();
          document.execCommand('copy');
          if (tip) {
            tip.style.display = 'block';
            setTimeout(() => {
              if (tip) tip.style.display = 'none';
            }, 3000);
          }
        }
      }
    });
  }

  public openSettingsModal() {
    const modal = document.getElementById('p2pSettingsModal');
    if (modal) {
      modal.style.display = 'flex';
      const inputNode = document.getElementById('inputNodeId') as HTMLInputElement;
      if (inputNode) inputNode.value = this.peerId;
      const inputSignal = document.getElementById('inputSignalUrl') as HTMLInputElement;
      if (inputSignal) inputSignal.value = this.signalUrl;
      const inputSecret = document.getElementById('inputSecret') as HTMLInputElement;
      if (inputSecret) inputSecret.value = this.secret;

      const inputTurnUrl = document.getElementById('inputTurnUrl') as HTMLInputElement;
      if (inputTurnUrl) inputTurnUrl.value = localStorage.getItem('p2p_turn_url') || '';
      const inputTurnUser = document.getElementById('inputTurnUser') as HTMLInputElement;
      if (inputTurnUser) inputTurnUser.value = localStorage.getItem('p2p_turn_user') || '';
      const inputTurnPass = document.getElementById('inputTurnPass') as HTMLInputElement;
      if (inputTurnPass) inputTurnPass.value = localStorage.getItem('p2p_turn_pass') || '';
    }
  }

  private showOfflinePrompt(peerId: string) {
    let overlay = document.getElementById('p2pOfflineOverlay');
    if (!overlay) {
      overlay = document.createElement('div');
      overlay.id = 'p2pOfflineOverlay';
      overlay.style.cssText = `
        position: fixed;
        top: 0; left: 0; width: 100vw; height: 100vh;
        background: rgba(13, 17, 23, 0.85);
        backdrop-filter: blur(10px);
        display: flex; justify-content: center; align-items: center;
        z-index: 15000;
        font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, monospace;
      `;
      document.body.appendChild(overlay);
    }

    overlay.innerHTML = `
      <div style="background: #161b22; border: 1px solid #30363d; border-radius: 12px; padding: 28px; max-width: 480px; width: 90%; color: #c9d1d9; box-shadow: 0 16px 40px rgba(0,0,0,0.6); text-align: center;">
        <div style="font-size: 36px; margin-bottom: 12px;">📡</div>
        <h2 style="font-size: 18px; color: #f0f6fc; margin-bottom: 8px;">Target Node [${peerId}] is Offline</h2>
        <p style="font-size: 13px; color: #8b949e; line-height: 1.6; margin-bottom: 20px;">
          The remote agent daemon has not registered with the signaling relay yet. Please verify that <code>agent.exe</code> is running on your server, or switch to another node.
        </p>
        <div style="background: #0d1117; border: 1px solid #30363d; border-radius: 6px; padding: 10px; font-size: 12px; font-family: monospace; color: #58a6ff; margin-bottom: 20px; text-align: left; word-break: break-all;">
          ./agent.exe -node-id ${peerId} -signal ${this.signalUrl}
        </div>
        <div style="display: flex; gap: 12px; justify-content: center;">
          <button id="btnRetryConn" style="background: #58a6ff; color: #0d1117; font-weight: 600; border: none; padding: 8px 18px; border-radius: 6px; cursor: pointer;">Retry</button>
          <button id="btnOpenSettings" style="background: #21262d; border: 1px solid #30363d; color: #c9d1d9; padding: 8px 18px; border-radius: 6px; cursor: pointer;">Switch Node</button>
        </div>
      </div>
    `;

    document.getElementById('btnRetryConn')?.addEventListener('click', () => {
      this.reconnect();
    });
    document.getElementById('btnOpenSettings')?.addEventListener('click', () => {
      this.openSettingsModal();
    });
  }

  private hideOfflinePrompt() {
    const overlay = document.getElementById('p2pOfflineOverlay');
    if (overlay) overlay.remove();
  }

  private showAuthPrompt(peerId: string, errorMsg?: string) {
    let overlay = document.getElementById('p2pAuthOverlay');
    if (!overlay) {
      overlay = document.createElement('div');
      overlay.id = 'p2pAuthOverlay';
      overlay.style.cssText = `
        position: fixed;
        top: 0; left: 0; width: 100vw; height: 100vh;
        background: rgba(13, 17, 23, 0.85);
        backdrop-filter: blur(10px);
        display: flex; justify-content: center; align-items: center;
        z-index: 25000;
        font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, monospace;
      `;
      document.body.appendChild(overlay);
    }

    overlay.innerHTML = `
      <div style="background: #161b22; border: 1px solid #30363d; border-radius: 12px; padding: 28px; max-width: 440px; width: 90%; color: #c9d1d9; box-shadow: 0 16px 40px rgba(0,0,0,0.6); text-align: center;">
        <div style="font-size: 36px; margin-bottom: 12px;">🔐</div>
        <h2 style="font-size: 18px; color: #f0f6fc; margin-bottom: 8px;">访问口令鉴权 (Auth Required)</h2>
        <p style="font-size: 13px; color: #8b949e; line-height: 1.6; margin-bottom: 16px;">
          目标节点 <code style="color: #58a6ff; font-weight: 600;">[${peerId}]</code> 已开启访问保护，请输入访问密钥：
        </p>
        ${
          errorMsg
            ? `<div style="background: rgba(248, 81, 73, 0.15); border: 1px solid rgba(248, 81, 73, 0.4); color: #f85149; border-radius: 6px; padding: 8px 12px; font-size: 12px; margin-bottom: 16px;">${errorMsg}</div>`
            : ''
        }
        <div style="margin-bottom: 16px; text-align: left;">
          <input id="inputAuthSecret" type="password" placeholder="请输入口令 / Secret Token..." value="${this.secret}" style="width: 100%; box-sizing: border-box; background: #0d1117; border: 1px solid #30363d; color: #f0f6fc; padding: 10px 12px; border-radius: 6px; font-size: 14px; font-family: monospace;" />
        </div>
        <div style="display: flex; align-items: center; gap: 8px; margin-bottom: 20px; font-size: 12px; color: #8b949e; cursor: pointer;">
          <input type="checkbox" id="chkRememberSecret" checked style="cursor: pointer;" />
          <label for="chkRememberSecret" style="cursor: pointer;">记住此节点的访问口令 (Remember)</label>
        </div>
        <div style="display: flex; gap: 12px; justify-content: center;">
          <button id="btnSubmitAuth" style="background: #238636; color: #ffffff; font-weight: 600; border: none; padding: 9px 22px; border-radius: 6px; cursor: pointer; font-size: 13px;">验证并连接</button>
          <button id="btnAuthSwitchNode" style="background: #21262d; border: 1px solid #30363d; color: #c9d1d9; padding: 9px 18px; border-radius: 6px; cursor: pointer; font-size: 13px;">切换节点</button>
        </div>
      </div>
    `;

    const input = document.getElementById('inputAuthSecret') as HTMLInputElement;
    if (input) {
      setTimeout(() => input.focus(), 50);
      input.addEventListener('keydown', (e) => {
        if (e.key === 'Enter') {
          doSubmit();
        }
      });
    }

    const doSubmit = () => {
      const val = input ? input.value.trim() : '';
      const remember = (document.getElementById('chkRememberSecret') as HTMLInputElement)?.checked;
      this.secret = val;
      if (remember) {
        try { localStorage.setItem('p2p_secret_' + peerId, val); } catch {}
      } else {
        try { localStorage.removeItem('p2p_secret_' + peerId); } catch {}
      }
      this.hideAuthPrompt();
      this.reconnect();
    };

    document.getElementById('btnSubmitAuth')?.addEventListener('click', doSubmit);
    document.getElementById('btnAuthSwitchNode')?.addEventListener('click', () => {
      this.hideAuthPrompt();
      this.openSettingsModal();
    });
  }

  private hideAuthPrompt() {
    const overlay = document.getElementById('p2pAuthOverlay');
    if (overlay) overlay.remove();
  }

  // Two-Stage ICE Restart self-healing pipeline for roaming / network switches
  private async triggerIceRestart() {
    if (this.isIceRestarting) return;
    if (!this.pc || this.pc.signalingState === 'closed') {
      this.reconnect();
      return;
    }

    this.isIceRestarting = true;
    this.log('Network interruption detected. Initiating WebRTC ICE Restart self-healing...');
    this.updateConnState('connecting');

    // 6-second fallback grace window: if ICE restart does not recover within 6s, fallback to full reconnect
    if (this.iceRestartTimer) clearTimeout(this.iceRestartTimer);
    this.iceRestartTimer = setTimeout(() => {
      if (this.isIceRestarting) {
        this.log('ICE Restart grace window timed out (6s). Falling back to full session reconnection...');
        this.isIceRestarting = false;
        this.reconnect();
      }
    }, 6000);

    try {
      // Stage 1: Ensure signaling channel WebSocket is alive
      await this.ensureSignalingConnected();

      // Stage 2: In-place ICE Restart Offer on existing RTCPeerConnection
      this.log('Generating in-place ICE Restart Offer...');
      const offer = await this.pc.createOffer({ iceRestart: true });
      await this.pc.setLocalDescription(offer);

      this.log('Transmitting ICE Restart Offer to Agent via Signaling...');
      this.ws?.send(
        JSON.stringify({
          type: 'offer',
          node_id: this.peerId,
          sdp: offer,
          secret: this.secret,
          client_token: this.clientToken,
          ice_restart: true,
          exp: this.shareExp || undefined,
          sign: this.shareSign || undefined,
        })
      );
    } catch (err: any) {
      this.log(`ICE Restart initiation failed: ${err.message}. Falling back to full reconnect.`);
      if (this.iceRestartTimer) clearTimeout(this.iceRestartTimer);
      this.isIceRestarting = false;
      this.reconnect();
    }
  }

  private ensureSignalingConnected(): Promise<void> {
    if (this.ws && this.ws.readyState === WebSocket.OPEN) {
      return Promise.resolve();
    }
    return new Promise<void>((resolve, reject) => {
      const timeout = setTimeout(() => {
        reject(new Error('Signaling reconnect timeout'));
      }, 4000);

      try {
        if (this.ws) {
          try { this.ws.close(); } catch {}
          this.ws = null;
        }
        this.log(`Re-establishing signaling WebSocket to ${this.signalUrl}...`);
        this.ws = new WebSocket(this.signalUrl);

        this.ws.onopen = () => {
          clearTimeout(timeout);
          this.log('Signaling WebSocket reconnected for ICE Restart. Re-joining room...');
          this.ws?.send(
            JSON.stringify({
              type: 'join',
              node_id: this.peerId,
              secret: this.secret,
            })
          );
          resolve();
        };

        this.ws.onmessage = async (event) => {
          try {
            const msg = JSON.parse(event.data);
            await this.handleSignalMessage(msg);
          } catch (err: any) {
            this.log(`Signaling parse error: ${err.message}`);
          }
        };

        this.ws.onerror = () => {
          clearTimeout(timeout);
          reject(new Error('Signaling WebSocket connection failed'));
        };

        this.ws.onclose = () => {
          this.log('Signaling WebSocket closed.');
        };
      } catch (e) {
        clearTimeout(timeout);
        reject(e);
      }
    });
  }

  private reconnect() {
    this.hideOfflinePrompt();
    this.hideAuthPrompt();
    this.isIceRestarting = false;
    if (this.iceRestartTimer) {
      clearTimeout(this.iceRestartTimer);
      this.iceRestartTimer = null;
    }

    this.log(`Reconnecting to node [${this.peerId}] via ${this.signalUrl}...`);

    const hashParams = new URLSearchParams();
    if (this.secret) {
      hashParams.set('secret', this.secret);
    } else if (this.shareSign && this.shareExp) {
      hashParams.set('exp', this.shareExp.toString());
      hashParams.set('sign', this.shareSign);
    }
    const qs = hashParams.toString();
    const targetHash = '#' + this.peerId + (qs ? '?' + qs : '');
    if (window.location.hash !== targetHash) {
      window.location.hash = targetHash;
    }

    if (this.peerVal) this.peerVal.textContent = this.peerId;
    if (this.signalVal) this.signalVal.textContent = this.signalUrl;

    try {
      sessionStorage.setItem('p2p_active_peer_id', this.peerId);
      localStorage.setItem('p2p_peer_id', this.peerId);
      if (this.secret) {
        sessionStorage.setItem('p2p_secret', this.secret);
        localStorage.setItem('p2p_secret_' + this.peerId, this.secret);
      }
    } catch {}

    const sw = navigator.serviceWorker?.controller;
    if (sw) {
      sw.postMessage({ type: 'NODE_CHANGE', peerId: this.peerId, secret: this.secret });
    }

    if (this.appFrame) {
      this.appFrame.remove();
      this.appFrame = null;
    }
    if (this.vaultContainer) {
      this.vaultContainer.remove();
      this.vaultContainer = null;
    }

    if (this.pingInterval) clearInterval(this.pingInterval);
    if (this.mux) {
      try { this.dc?.close(); } catch {}
      this.mux = null;
    }
    if (this.pc) {
      try { this.pc.close(); } catch {}
      this.pc = null;
    }
    if (this.ws) {
      try { this.ws.close(); } catch {}
      this.ws = null;
    }
    this.updateConnState('disconnected');
    this.startSignaling();
  }

  // Handle Fetch interception relayed from Service Worker
  private handleSWMessage(event: MessageEvent) {
    const msg = event.data;
    if (!msg || msg.type !== 'TUNNEL_FETCH_REQUEST') return;

    if (!this.mux) {
      if (event.ports && event.ports[0]) {
        event.ports[0].postMessage({
          type: 'RESP_ERROR',
          error: 'P2P DataChannel is not connected yet',
        });
      }
      return;
    }

    const port = event.ports[0];
    const stream = this.mux.openStream();
    this.updateStreamsCount(1);

    // Send HEADERS frame to Agent
    stream.sendHeaders({
      m: msg.method,
      p: msg.path,
      h: msg.headers,
    });

    // Handle upload body from Service Worker with serialized write queue
    let writeQueue: Promise<void> = Promise.resolve();
    port.onmessage = (e: MessageEvent) => {
      const reqMsg = e.data;
      switch (reqMsg.type) {
        case 'REQ_BODY_CHUNK':
          if (reqMsg.chunk) {
            const chunkData = new Uint8Array(reqMsg.chunk);
            writeQueue = writeQueue.then(async () => {
              try {
                await stream.write(chunkData);
              } catch {}
            });
          }
          break;
        case 'REQ_BODY_END':
          writeQueue = writeQueue.then(async () => {
            try {
              stream.close();
            } catch {}
          });
          break;
        case 'RESP_CANCEL':
          stream.reset();
          break;
      }
    };

    // Handle response headers from Go Agent
    stream.onHeaders((respMeta: any) => {
      // Sync non-HttpOnly cookies to document.cookie
      if (respMeta.c && Array.isArray(respMeta.c)) {
        for (const cookieStr of respMeta.c) {
          try {
            document.cookie = cookieStr;
          } catch {}
        }
      }

      const headers = respMeta.h || {};
      const contentType = (headers['content-type'] || '').toLowerCase();
      const contentDisposition = (headers['content-disposition'] || '').toLowerCase();
      const p2pMode = headers['x-p2p-mode'] || '';
      const p2pFileName = headers['x-p2p-file-name'] || '';
      const p2pFileSize = parseInt(headers['x-p2p-file-size'] || '0', 10) || 0;

      // Handle navigation requests originated from the appViewport iframe
      if (msg.destination === 'iframe') {
        const isExplicitInline = msg.path.includes('view=inline');
        const isP2PFileOrDir = p2pMode === 'file' || p2pMode === 'dir';

        const fileName = p2pFileName || this.extractFileName(msg.path);
        const isPdf = contentType.includes('application/pdf') || fileName.toLowerCase().endsWith('.pdf');
        const isVideo = contentType.startsWith('video/') || !!fileName.match(/\.(mp4|m4v|webm|mkv|avi|mov|wmv|flv)$/i);
        const isAudio = contentType.startsWith('audio/') || !!fileName.match(/\.(mp3|wav|flac|aac|m4a|ogg|opus)$/i);
        const isMedia = isVideo || isAudio;
        const isAttachment = contentDisposition.includes('attachment') ||
                             (p2pMode === 'file' && !contentType.includes('text/html') && !contentType.startsWith('image/') && !isMedia && !isPdf);

        if (!isExplicitInline && isP2PFileOrDir && (isPdf || isMedia || isAttachment)) {
          this.showFileVault({
            fileName,
            fileSize: p2pFileSize,
            type: isPdf ? 'pdf' : (isMedia ? 'media' : 'file'),
            path: msg.path,
            isDir: p2pMode === 'dir',
          });

          // Terminate iframe navigation: Send 204 No Content so the iframe cancels navigation
          // and remains safely on its current page (e.g. directory listing), completely eliminating
          // Edge's sandbox plugin blocking error and preventing background bandwidth consumption.
          port.postMessage({
            type: 'RESP_START',
            status: 204,
            statusText: 'No Content',
            headers: {},
          });
          port.postMessage({ type: 'RESP_END' });
          this.updateStreamsCount(-1);

          // Reset the backend stream immediately to stop pushing data and preserve P2P bandwidth
          stream.reset();
          return;
        } else if (this.appFrame && this.appFrame.style.display === 'none') {
          // Restore iframe if returning to normal HTML page, media streaming, or directory view
          this.closeVaultAndRestoreIframe();
        }
      }

      port.postMessage({
        type: 'RESP_START',
        status: respMeta.s || 200,
        statusText: respMeta.m || 'OK',
        headers: headers,
      });
    });

    // Stream response chunks into Service Worker
    stream.onData((chunk: Uint8Array, fin: boolean) => {
      if (chunk.byteLength > 0) {
        const buf = chunk.buffer.slice(chunk.byteOffset, chunk.byteOffset + chunk.byteLength);
        port.postMessage({
          type: 'RESP_CHUNK',
          chunk: buf,
        }, [buf]);
      }
      if (fin) {
        port.postMessage({ type: 'RESP_END' });
        this.updateStreamsCount(-1);
      }
    });
  }

  // Listen to messages from child iframes (e.g. WebSocket Monkey Patch & Cookie sync)
  private listenChildMessages() {
    window.addEventListener('message', (event) => {
      const msg = event.data;
      if (!msg) return;

      if (msg.type === 'TUNNEL_COOKIE_UPDATE') {
        // Sync document.cookie
        try {
          document.cookie = msg.cookie;
        } catch {}
      } else if (msg.type === 'TUNNEL_WS_CONNECT') {
        this.handleWSConnect(msg, event.ports[0]);
      }
    });
  }

  private handleWSConnect(msg: any, port: MessagePort) {
    if (!this.mux) {
      port.postMessage({ type: 'WS_ERROR', error: 'P2P tunnel not open' });
      return;
    }

    const stream = this.mux.openStream();
    this.updateStreamsCount(1);
    this.log(`Bridging WebSocket connection for [${msg.path}] over Stream ID ${stream.id}`);

    // Send WS_CONNECT headers
    stream.sendHeaders({
      p: msg.path,
      protocols: msg.protocols,
      h: { upgrade: 'websocket' },
    });

    // Wait for 101 confirmation
    stream.onHeaders((respMeta: any) => {
      if (respMeta.s === 101) {
        port.postMessage({ type: 'WS_OPENED', protocol: respMeta.h?.['sec-websocket-protocol'] });
      } else {
        port.postMessage({ type: 'WS_ERROR', error: respMeta.m || 'WS Upgrade failed' });
      }
    });

    // Relays incoming stream data to WS port with defragmentation buffer
    let wsBuffer = new Uint8Array(0);
    stream.onData((chunk: Uint8Array, fin: boolean) => {
      if (chunk.byteLength > 0) {
        const next = new Uint8Array(wsBuffer.byteLength + chunk.byteLength);
        next.set(wsBuffer, 0);
        next.set(chunk, wsBuffer.byteLength);
        wsBuffer = next;

        while (wsBuffer.byteLength >= 5) {
          const opcode = wsBuffer[0];
          const view = new DataView(wsBuffer.buffer, wsBuffer.byteOffset, wsBuffer.byteLength);
          const length = view.getUint32(1, false);

          if (wsBuffer.byteLength < 5 + length) {
            // Need more data from next chunk
            break;
          }

          const payload = wsBuffer.subarray(5, 5 + length);
          if (opcode === 1) {
            const text = new TextDecoder().decode(payload);
            port.postMessage({ type: 'WS_MSG', data: text });
          } else {
            const payloadBuf = wsBuffer.buffer.slice(wsBuffer.byteOffset + 5, wsBuffer.byteOffset + 5 + length);
            port.postMessage({ type: 'WS_MSG', data: payloadBuf }, [payloadBuf]);
          }
          wsBuffer = wsBuffer.subarray(5 + length);
        }
      }
      if (fin) {
        port.postMessage({ type: 'WS_CLOSED' });
        this.updateStreamsCount(-1);
      }
    });

    // Handle outgoing data from WS port
    port.onmessage = async (event: MessageEvent) => {
      const clientMsg = event.data;
      switch (clientMsg.type) {
        case 'WS_SEND':
          let payloadBytes: Uint8Array;
          if (typeof clientMsg.data === 'string') {
            payloadBytes = new TextEncoder().encode(clientMsg.data);
          } else {
            payloadBytes = new Uint8Array(clientMsg.data);
          }

          const packet = new Uint8Array(5 + payloadBytes.byteLength);
          packet[0] = clientMsg.opcode || 1;
          const view = new DataView(packet.buffer);
          view.setUint32(1, payloadBytes.byteLength, false);
          packet.set(payloadBytes, 5);

          await stream.write(packet);
          break;

        case 'WS_CLOSE':
          stream.close();
          this.updateStreamsCount(-1);
          break;
      }
    };
  }

  private updateStreamsCount(delta: number) {
    this.activeStreamsCount = Math.max(0, this.activeStreamsCount + delta);
    if (this.streamsVal) {
      this.streamsVal.textContent = `${this.activeStreamsCount} Active`;
    }
  }

  private async runEchoTest() {
    if (!this.mux) return;

    this.btnEcho.disabled = true;
    this.testOutput.textContent = 'Running echo test over P2P stream...';

    const testText = 'Hello P2PMux! Zero-client intranet penetration gateway is working! ' +
      'Data: ' + Array.from({ length: 50 }, (_, i) => `[Chunk#${i}]`).join(' ');

    const payload = new TextEncoder().encode(testText);
    const stream = this.mux.openStream();
    this.log(`Opened new P2P Stream (ID=${stream.id}). Sending ${payload.byteLength} bytes...`);

    const startTime = performance.now();
    let receivedChunks: Uint8Array[] = [];

    stream.onData((chunk, fin) => {
      if (chunk.byteLength > 0) {
        receivedChunks.push(chunk);
      }
      if (fin) {
        const elapsed = performance.now() - startTime;
        const totalLen = receivedChunks.reduce((acc, c) => acc + c.byteLength, 0);
        const merged = new Uint8Array(totalLen);
        let offset = 0;
        for (const c of receivedChunks) {
          merged.set(c, offset);
          offset += c.byteLength;
        }

        const decoded = new TextDecoder().decode(merged);
        const isMatched = decoded === testText;

        const report = `Stream ID ${stream.id} finished in ${elapsed.toFixed(1)}ms. ` +
          `Transferred: ${totalLen} bytes. Verification: ${isMatched ? 'MATCHED (100% OK)' : 'MISMATCH'}`;

        this.log(report);
        this.testOutput.textContent = report;
        this.btnEcho.disabled = false;
      }
    });

    try {
      await stream.write(payload);
      stream.close();
    } catch (err: any) {
      this.log(`Stream write error: ${err.message}`);
      this.btnEcho.disabled = false;
    }
  }

  // Show dedicated P2P File Vault for PDF and binary downloads
  private showFileVault(opts: { fileName: string; fileSize: number; type: string; path: string; isDir: boolean }) {
    if (this.activeVaultAbortController) {
      this.activeVaultAbortController.abort();
      this.activeVaultAbortController = null;
    }
    if (this.appFrame) {
      this.appFrame.style.display = 'none';
    }
    if (this.mediaContainer) {
      this.mediaContainer.remove();
      this.mediaContainer = null;
    }
    if (this.vaultContainer) {
      this.vaultContainer.remove();
      this.vaultContainer = null;
    }

    const container = document.createElement('div');
    container.id = 'p2pFileVault';
    container.style.cssText = `
      position: fixed;
      top: 0;
      left: 0;
      width: 100vw;
      height: 100vh;
      background: #0d1117;
      color: #f0f6fc;
      display: flex;
      flex-direction: column;
      align-items: center;
      justify-content: center;
      z-index: 100;
      padding: 20px;
      box-sizing: border-box;
      font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, monospace;
    `;

    const formattedSize = this.formatFileSize(opts.fileSize);
    const isPdf = opts.type === 'pdf' || opts.fileName.toLowerCase().endsWith('.pdf');
    const isVideo = !!opts.fileName.match(/\.(mp4|m4v|webm|mkv|avi|mov|wmv|flv)$/i);
    const isAudio = !!opts.fileName.match(/\.(mp3|wav|flac|aac|m4a|ogg|opus)$/i);
    const isMedia = opts.type === 'media' || isVideo || isAudio;
    const isArchive = !!opts.fileName.match(/\.(zip|tar|gz|7z|rar|iso|img|bin)$/i);

    let icon = '💾';
    if (isVideo) icon = '🎬';
    else if (isAudio) icon = '🎵';
    else if (isPdf) icon = '📄';
    else if (isArchive) icon = '📦';

    const viewUrl = opts.isDir ? opts.path : '/' + encodeURIComponent(opts.fileName || 'file');

    container.innerHTML = `
      <div style="background: #161b22; border: 1px solid #30363d; border-radius: 12px; max-width: 540px; width: 100%; padding: 32px; box-shadow: 0 16px 36px rgba(0,0,0,0.6); text-align: center;">
        <div style="font-size: 54px; margin-bottom: 16px; line-height: 1;">${icon}</div>
        <h2 style="font-size: 18px; font-weight: 600; color: #f0f6fc; margin-bottom: 8px; word-break: break-all; line-height: 1.4;">
          ${this.escapeHtml(opts.fileName)}
        </h2>
        <div style="display: flex; justify-content: center; align-items: center; gap: 12px; margin-bottom: 24px;">
          ${formattedSize ? `<span style="font-size: 13px; color: #8b949e; background: #21262d; padding: 2px 10px; border-radius: 12px; border: 1px solid #30363d;">${formattedSize}</span>` : ''}
          <span style="font-size: 13px; color: #3fb950; background: rgba(63, 185, 80, 0.15); padding: 2px 10px; border-radius: 12px; border: 1px solid #3fb950;">⚡ P2P Direct Tunnel</span>
        </div>

        <div style="display: flex; flex-direction: column; gap: 12px; margin-bottom: 20px;">
          ${isMedia ? `
          <button id="btnVaultPlay" style="background: #1f6feb; color: #ffffff; border: 1px solid rgba(240, 246, 252, 0.1); border-radius: 6px; padding: 12px 20px; font-size: 15px; font-weight: 600; cursor: pointer; display: flex; align-items: center; justify-content: center; gap: 8px; transition: 0.2s;">
            ▶ 立即在线播放 (Play Online)
          </button>
          ` : ''}

          <button id="btnVaultDownload" style="background: #238636; color: #ffffff; border: 1px solid rgba(240, 246, 252, 0.1); border-radius: 6px; padding: 12px 20px; font-size: 15px; font-weight: 600; cursor: pointer; display: flex; align-items: center; justify-content: center; gap: 8px; transition: 0.2s;">
            ⚡ 极速 P2P 下载 (Download)
          </button>

          <div id="vaultProgressBar" style="display: none; background: #21262d; border-radius: 6px; overflow: hidden; height: 8px; width: 100%; border: 1px solid #30363d; margin-top: -4px;">
            <div id="vaultProgressFill" style="background: #238636; width: 0%; height: 100%; transition: width 0.15s ease;"></div>
          </div>
          <div id="vaultStatusText" style="display: none; font-size: 12px; color: #8b949e; text-align: center; margin-top: -6px; word-break: break-all;"></div>

          <button id="btnVaultCancel" style="display: none; background: #21262d; color: #f85149; border: 1px solid rgba(248, 81, 73, 0.3); border-radius: 6px; padding: 8px 16px; font-size: 13px; font-weight: 600; cursor: pointer; align-items: center; justify-content: center; gap: 6px; transition: 0.2s;">
            ⏹ 终止下载 (Cancel)
          </button>

          ${isPdf ? `
          <button id="btnVaultPreview" style="background: #21262d; border: 1px solid #30363d; color: #58a6ff; border-radius: 6px; padding: 10px 20px; font-size: 14px; font-weight: 500; cursor: pointer; display: flex; align-items: center; justify-content: center; gap: 8px; transition: 0.2s;">
            📑 在新标签页打开原生预览 (Open in New Tab)
          </button>
          ` : ''}

          ${opts.isDir ? `
          <button id="btnVaultBack" style="background: transparent; border: 1px solid #30363d; color: #8b949e; border-radius: 6px; padding: 8px 16px; font-size: 13px; cursor: pointer; transition: 0.2s;">
            ⬅ 返回文件目录 (Back to Directory)
          </button>
          ` : ''}
        </div>

        ${isPdf ? `
        <p style="font-size: 12px; color: #8b949e; line-height: 1.5; margin: 0; text-align: left; background: #0d1117; padding: 10px 14px; border-radius: 6px; border: 1px solid #21262d;">
          💡 <strong>预览提示</strong>：由于浏览器安全沙箱限制，内嵌沙箱环境已禁用 PDF 插件。桌面端请点击<strong>【在新标签页打开原生预览】</strong>；移动端推荐点击<strong>【极速 P2P 下载】</strong>使用系统阅读器浏览。
        </p>
        ` : ''}

        ${isMedia ? `
        <p style="font-size: 12px; color: #8b949e; line-height: 1.5; margin: 0; text-align: left; background: #0d1117; padding: 10px 14px; border-radius: 6px; border: 1px solid #21262d;">
          💡 <strong>媒体提示</strong>：点击<strong>【立即在线播放】</strong>使用浏览器原生播放器（支持全屏、倍速与画中画）；点击<strong>【极速 P2P 下载】</strong>将原片高速直存至本地设备。
        </p>
        ` : ''}
      </div>
    `;

    document.body.appendChild(container);
    this.vaultContainer = container;

    const btnDownload = document.getElementById('btnVaultDownload') as HTMLButtonElement | null;
    const btnPlay = document.getElementById('btnVaultPlay') as HTMLButtonElement | null;
    const btnPreview = document.getElementById('btnVaultPreview') as HTMLButtonElement | null;
    const btnCancel = document.getElementById('btnVaultCancel') as HTMLButtonElement | null;
    const progressBar = document.getElementById('vaultProgressBar') as HTMLDivElement | null;
    const progressFill = document.getElementById('vaultProgressFill') as HTMLDivElement | null;
    const statusText = document.getElementById('vaultStatusText') as HTMLDivElement | null;

    if (isMedia) {
      btnPlay?.addEventListener('click', () => {
        this.closeVaultAndRestoreIframe();
        const inlineUrl = viewUrl + (viewUrl.includes('?') ? '&' : '?') + 'view=inline';
        if (this.appFrame) {
          this.appFrame.src = inlineUrl;
        }
      });
    }

    btnCancel?.addEventListener('click', () => {
      if (this.activeVaultAbortController) {
        this.activeVaultAbortController.abort();
        this.activeVaultAbortController = null;
      }
    });

    btnDownload?.addEventListener('click', async () => {
      if (this.activeVaultAbortController) {
        return; // Prevent duplicate download clicks
      }

      const resetUI = () => {
        if (btnDownload) {
          btnDownload.disabled = false;
          btnDownload.style.opacity = '1';
          btnDownload.style.cursor = 'pointer';
          btnDownload.innerText = '⚡ 极速 P2P 下载 (Download)';
        }
        if (btnPlay) btnPlay.style.display = 'flex';
        if (btnPreview) btnPreview.style.display = 'flex';
        if (btnCancel) btnCancel.style.display = 'none';
        if (progressBar) progressBar.style.display = 'none';
        if (progressFill) progressFill.style.width = '0%';
        if (statusText) statusText.style.display = 'none';
      };

      // 1. Dual-mode write detection for ultra-large files (> 500MB) via File System Access API
      let fileStream: any = null;
      const LARGE_FILE_THRESHOLD = 500 * 1024 * 1024; // 500MB
      const hasSavePicker = typeof window !== 'undefined' && typeof (window as any).showSaveFilePicker === 'function';

      if (opts.fileSize > LARGE_FILE_THRESHOLD && hasSavePicker) {
        try {
          const handle = await (window as any).showSaveFilePicker({
            suggestedName: opts.fileName,
          });
          fileStream = await handle.createWritable();
        } catch (pickerErr: any) {
          if (pickerErr.name === 'AbortError') {
            // User cancelled the file picker dialog
            return;
          }
          this.log(`File System Access API prompt failed: ${pickerErr.message}, falling back to Blob.`);
        }
      }

      // 2. Lock UI & prepare AbortController
      btnDownload.disabled = true;
      btnDownload.style.opacity = '0.7';
      btnDownload.style.cursor = 'not-allowed';
      btnDownload.innerText = '⚡ 正在建立 P2P 传输...';
      if (btnPlay) btnPlay.style.display = 'none';
      if (btnPreview) btnPreview.style.display = 'none';
      if (btnCancel) btnCancel.style.display = 'flex';
      if (progressBar) progressBar.style.display = 'block';
      if (statusText) {
        statusText.style.display = 'block';
        statusText.innerText = '正在通过 WebRTC DataChannel 发起请求...';
      }

      const abortController = new AbortController();
      this.activeVaultAbortController = abortController;

      try {
        const response = await fetch(viewUrl, { signal: abortController.signal });
        if (!response.ok) {
          throw new Error(`HTTP ${response.status} ${response.statusText}`);
        }

        const contentLengthHeader = response.headers.get('content-length');
        const totalBytes = contentLengthHeader ? parseInt(contentLengthHeader, 10) : (opts.fileSize || 0);
        const contentType = response.headers.get('content-type') || 'application/octet-stream';

        const reader = response.body?.getReader();
        if (!reader) {
          const blob = await response.blob();
          this.triggerBlobDownload(blob, opts.fileName);
          btnDownload.innerText = '✅ 下载完成';
          if (btnCancel) btnCancel.style.display = 'none';
          setTimeout(resetUI, 2500);
          return;
        }

        const chunks: Uint8Array[] = [];
        let receivedBytes = 0;
        const startTime = Date.now();
        let lastUiUpdate = 0;

        while (true) {
          const { done, value } = await reader.read();
          if (done) break;
          if (value) {
            receivedBytes += value.length;
            if (fileStream) {
              await fileStream.write(value);
            } else {
              chunks.push(value);
            }

            const now = Date.now();
            if (now - lastUiUpdate > 100 || (totalBytes > 0 && receivedBytes >= totalBytes)) {
              lastUiUpdate = now;
              const elapsedSec = Math.max((now - startTime) / 1000, 0.01);
              const speedBps = receivedBytes / elapsedSec;
              const speedStr = this.formatSpeed(speedBps);
              const receivedStr = this.formatFileSize(receivedBytes);
              const totalStr = totalBytes > 0 ? this.formatFileSize(totalBytes) : '';

              if (totalBytes > 0) {
                const pct = Math.min(100, Math.round((receivedBytes / totalBytes) * 100));
                btnDownload.innerText = `📥 正在接收: ${pct}% (${speedStr})`;
                if (progressFill) progressFill.style.width = `${pct}%`;
                if (statusText) statusText.innerText = `已传输 ${receivedStr} / ${totalStr} · 速度 ${speedStr}`;
              } else {
                btnDownload.innerText = `📥 已接收 ${receivedStr} (${speedStr})`;
                if (statusText) statusText.innerText = `已传输 ${receivedStr} · 速度 ${speedStr}`;
              }
            }
          }
        }

        if (fileStream) {
          await fileStream.close();
          fileStream = null;
        } else {
          const finalBlob = new Blob(chunks, { type: contentType });
          this.triggerBlobDownload(finalBlob, opts.fileName);
        }

        btnDownload.innerText = '✅ 下载完成';
        if (btnCancel) btnCancel.style.display = 'none';
        if (progressFill) progressFill.style.width = '100%';
        if (statusText) statusText.innerText = `🎉 传输完毕，共 ${this.formatFileSize(receivedBytes)}`;
        setTimeout(resetUI, 2500);
      } catch (err: any) {
        if (fileStream) {
          try { await fileStream.abort(); } catch {}
          fileStream = null;
        }
        if (btnCancel) btnCancel.style.display = 'none';
        if (err.name === 'AbortError') {
          btnDownload.innerText = '⚠️ 下载已取消';
          if (statusText) statusText.innerText = '用户中断了传输';
        } else {
          this.log(`Vault download error: ${err.message}`);
          btnDownload.innerText = '❌ 下载失败';
          if (statusText) statusText.innerText = `传输失败: ${err.message || '网络连接异常'}`;
        }
        setTimeout(resetUI, 1500);
      } finally {
        this.activeVaultAbortController = null;
      }
    });

    if (isPdf) {
      document.getElementById('btnVaultPreview')?.addEventListener('click', () => {
        window.open(viewUrl, '_blank');
      });
    }

    if (opts.isDir) {
      document.getElementById('btnVaultBack')?.addEventListener('click', () => {
        this.closeVaultAndRestoreIframe();
      });
    }
  }

  private closeVaultAndRestoreIframe() {
    if (this.activeVaultAbortController) {
      this.activeVaultAbortController.abort();
      this.activeVaultAbortController = null;
    }
    if (this.vaultContainer) {
      this.vaultContainer.remove();
      this.vaultContainer = null;
    }
    if (this.appFrame) {
      this.appFrame.style.display = 'block';
    }
  }

  private formatSpeed(bytesPerSec: number): string {
    return `${this.formatFileSize(bytesPerSec)}/s`;
  }

  private triggerBlobDownload(blob: Blob, fileName: string) {
    const blobUrl = URL.createObjectURL(blob);
    const a = document.createElement('a');
    a.href = blobUrl;
    a.download = fileName;
    document.body.appendChild(a);
    a.click();
    a.remove();
    setTimeout(() => {
      try {
        URL.revokeObjectURL(blobUrl);
      } catch {}
    }, 60000);
  }

  private formatFileSize(bytes: number): string {
    if (!bytes || bytes <= 0) return '';
    const units = ['B', 'KB', 'MB', 'GB', 'TB'];
    let val = bytes;
    let idx = 0;
    while (val >= 1024 && idx < units.length - 1) {
      val /= 1024;
      idx++;
    }
    return `${val.toFixed(val >= 10 || idx === 0 ? 0 : 1)} ${units[idx]}`;
  }

  private escapeHtml(str: string): string {
    return str
      .replace(/&/g, '&amp;')
      .replace(/</g, '&lt;')
      .replace(/>/g, '&gt;')
      .replace(/"/g, '&quot;')
      .replace(/'/g, '&#039;');
  }

  private extractFileName(urlPath: string): string {
    try {
      const clean = urlPath.split('?')[0].split('#')[0];
      const segments = clean.split('/').filter(Boolean);
      if (segments.length > 0) {
        return decodeURIComponent(segments[segments.length - 1]);
      }
    } catch {}
    return 'file';
  }
}

// Initialize on page load
window.addEventListener('DOMContentLoaded', () => {
  new PortalApp();
});
