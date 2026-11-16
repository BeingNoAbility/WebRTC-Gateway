var C=(u,e,t)=>()=>{if(t)throw t[0];try{return u&&(e=u(u=0)),e}catch(i){throw t=[i],i}};var M=(u,e)=>()=>{try{return e||u((e={exports:{}}).exports,e),e.exports}catch(t){throw e=0,t}};function b(u,e,t,i){let n=i?i.byteLength:0,o=10+n,s=new Uint8Array(o),r=new DataView(s.buffer,s.byteOffset,s.byteLength);return r.setUint32(0,u,!1),r.setUint8(4,e),r.setUint8(5,t),r.setUint32(6,n,!1),i&&n>0&&s.set(i,10),s}function E(u){if(u.byteLength<10)throw new Error(`Buffer too short for frame header: ${u.byteLength} < 10`);let e=new DataView(u.buffer,u.byteOffset,u.byteLength),t=e.getUint32(0,!1),i=e.getUint8(4),n=e.getUint8(5),o=e.getUint32(6,!1);if(u.byteLength!==10+o)throw new Error(`Buffer size mismatch: expected ${10+o}, got ${u.byteLength}`);let s=u.slice(10,10+o);return{streamId:t,type:i,flags:n,length:o,payload:s}}var f,x,k=C(()=>{"use strict";f={DATA:1,HEADERS:2,RST_STREAM:3,WINDOW_UPDATE:4,PING:5,PONG:6,WS_CONNECT:16,WS_DATA:17,WS_CLOSE:18},x={NONE:0,FIN:1,ACK:2}});var v,w,T=C(()=>{"use strict";k();v=class{id;mux;readableController;_readableStream;headersCallbacks=[];dataCallbacks=[];isClosed=!1;readClosed=!1;writeClosed=!1;constructor(e,t){this.id=e,this.mux=t,this._readableStream=new ReadableStream({start:i=>{this.readableController=i},cancel:()=>{this.reset()}})}get readableStream(){return this._readableStream}onHeaders(e){this.headersCallbacks.push(e)}onData(e){this.dataCallbacks.push(e)}sendHeaders(e){let t=typeof e=="string"?e:JSON.stringify(e),i=new TextEncoder().encode(t),n=b(this.id,f.HEADERS,x.NONE,i);this.mux.sendRaw(n)}async write(e){if(this.isClosed)throw new Error(`Stream ${this.id} is closed`);let t=0;for(;t<e.byteLength;){let i=Math.min(32768,e.byteLength-t),n=e.subarray(t,t+i),o=b(this.id,f.DATA,x.NONE,n);await this.mux.sendRawAsync(o),t+=i}}close(){if(this.writeClosed||this.isClosed)return;this.writeClosed=!0;let e=b(this.id,f.DATA,x.FIN,null);this.mux.sendRaw(e),this.checkFullClose()}reset(){if(this.isClosed)return;this.isClosed=!0,this.readClosed=!0,this.writeClosed=!0;let e=b(this.id,f.RST_STREAM,x.NONE,null);if(this.mux.sendRaw(e),this.readableController)try{this.readableController.error(new Error("Stream reset by client"))}catch{}this.mux.removeStream(this.id)}checkFullClose(){this.readClosed&&this.writeClosed&&(this.isClosed=!0,this.mux.removeStream(this.id))}_pushHeaders(e){let t=null;try{let i=new TextDecoder().decode(e);t=JSON.parse(i)}catch{t=e}for(let i of this.headersCallbacks)i(t)}_pushData(e,t){if(e.byteLength>0&&this.readableController)try{this.readableController.enqueue(e)}catch{}for(let i of this.dataCallbacks)i(e,t);if(t){if(this.readClosed=!0,this.readableController)try{this.readableController.close()}catch{}this.checkFullClose()}}_pushReset(){if(this.isClosed=!0,this.readClosed=!0,this.writeClosed=!0,this.readableController)try{this.readableController.error(new Error("Stream reset by peer"))}catch{}this.mux.removeStream(this.id)}},w=class{dc;isClient;nextStreamId;streams=new Map;streamListeners=[];pongResolvers=[];pingTime=0;highWaterMark=256*1024;lowWaterMark=64*1024;sendQueue=Promise.resolve();constructor(e,t=!0){this.dc=e,this.isClient=t,this.nextStreamId=t?1:2,this.dc.binaryType="arraybuffer",this.dc.bufferedAmountLowThreshold=this.lowWaterMark,this.dc.onmessage=i=>{i.data instanceof ArrayBuffer?this.handleMessage(new Uint8Array(i.data)):i.data instanceof Blob&&i.data.arrayBuffer().then(n=>{this.handleMessage(new Uint8Array(n))})}}openStream(){let e=this.nextStreamId;this.nextStreamId+=2;let t=new v(e,this);return this.streams.set(e,t),t}onStream(e){this.streamListeners.push(e)}removeStream(e){this.streams.delete(e)}ping(){return new Promise((e,t)=>{this.pingTime=performance.now();let i=setTimeout(()=>{t(new Error("Ping timeout"))},5e3);this.pongResolvers.push(o=>{clearTimeout(i),e(o)});let n=b(0,f.PING,x.NONE,null);this.sendRaw(n)})}sendRaw(e){if(this.dc.readyState!=="open")throw new Error(`DataChannel not open (readyState: ${this.dc.readyState})`);this.dc.send(e)}async sendRawAsync(e){if(this.dc.readyState!=="open")throw new Error(`DataChannel not open (readyState: ${this.dc.readyState})`);let t=async()=>{if(this.dc.readyState!=="open")throw new Error(`DataChannel not open (readyState: ${this.dc.readyState})`);for(;this.dc.bufferedAmount>this.highWaterMark;)await new Promise(i=>{let n=null,o=()=>{n&&clearTimeout(n),this.dc.removeEventListener("bufferedamountlow",o),i()};this.dc.addEventListener("bufferedamountlow",o),n=setTimeout(()=>{this.dc.removeEventListener("bufferedamountlow",o),i()},50)});this.dc.send(e)};this.sendQueue=this.sendQueue.then(t,t),await this.sendQueue}handleMessage(e){try{let t=E(e);if(t.streamId===0){this.handleControlFrame(t);return}let i=this.streams.get(t.streamId);if(!i)if(t.type===f.HEADERS||t.type===f.DATA||t.type===f.WS_CONNECT){i=new v(t.streamId,this),this.streams.set(t.streamId,i);for(let n of this.streamListeners)n(i)}else return;switch(t.type){case f.HEADERS:i._pushHeaders(t.payload);break;case f.DATA:case f.WS_DATA:let n=(t.flags&x.FIN)!==0;i._pushData(t.payload,n);break;case f.RST_STREAM:i._pushReset();break}}catch(t){console.error("[P2PMux] Failed to decode incoming frame:",t)}}handleControlFrame(e){switch(e.type){case f.PING:let t=b(0,f.PONG,x.NONE,e.payload);this.sendRaw(t);break;case f.PONG:let i=performance.now()-this.pingTime;for(;this.pongResolvers.length>0;){let n=this.pongResolvers.shift();n&&n(i)}break}}}});var _=M(()=>{T();var R="myhome",L=typeof window<"u"&&window.location&&window.location.host?`${window.location.protocol==="https:"?"wss:":"ws:"}//${window.location.host}/ws`:"ws://127.0.0.1:8888/ws";async function B(u,e){let t=new TextEncoder,i=t.encode(u),n=t.encode(e),o=await crypto.subtle.importKey("raw",i,{name:"HMAC",hash:"SHA-256"},!1,["sign"]),s=await crypto.subtle.sign("HMAC",o,n);return Array.from(new Uint8Array(s)).map(d=>d.toString(16).padStart(2,"0")).join("")}var S=class{peerId;signalUrl;secret="";shareExp=0;shareSign="";isIceRestarting=!1;iceRestartTimer=null;clientToken="";initialSubpath="/";ws=null;pc=null;hudStatsTimer=null;lastStatsTime=0;lastBytesReceived=0;lastBytesSent=0;hudInfo={transportType:"unknown",localType:"",remoteType:"",localAddress:"",remoteAddress:"",protocol:"UDP",rttMs:0,rxRateKB:0,txRateKB:0,totalRxMB:0,totalTxMB:0,packetsLost:0,iceState:"disconnected",connState:"new"};dc=null;mux=null;pingInterval=null;swRegistration=null;connBadge=document.getElementById("connBadge");peerVal=document.getElementById("peerVal");signalVal=document.getElementById("signalVal");rttVal=document.getElementById("rttVal");streamsVal=document.getElementById("streamsVal");logBox=document.getElementById("logBox");btnPing=document.getElementById("btnPing");btnEcho=document.getElementById("btnEcho");btnClearLog=document.getElementById("btnClearLog");testOutput=document.getElementById("testOutput");appFrame=null;vaultContainer=null;mediaContainer=null;activeStreamsCount=0;constructor(){let e=this.resolveInitialConfig();this.peerId=e.peerId,this.signalUrl=e.signalUrl,this.secret=e.secret,this.shareExp=e.exp||0,this.shareSign=e.sign||"",this.initialSubpath=e.path||"/";let t="";try{t=sessionStorage.getItem("p2p_client_token")||"",t||(t="cl_"+(typeof crypto<"u"&&crypto.randomUUID?crypto.randomUUID():Math.random().toString(36).substring(2)+Date.now().toString(36)),sessionStorage.setItem("p2p_client_token",t))}catch{t="cl_"+Math.random().toString(36).substring(2)}this.clientToken=t,this.peerVal&&(this.peerVal.textContent=this.peerId),this.signalVal&&(this.signalVal.textContent=this.signalUrl),typeof window<"u"&&window.addEventListener("message",i=>{if(i.data&&i.data.type==="P2P_ROUTE_SYNC"){let n=i.data.path;typeof n=="string"&&n.startsWith("/")&&!n.startsWith("//")&&!n.toLowerCase().includes("javascript:")&&this.syncRouteToHash(n)}}),this.bindEvents(),this.initServiceWorker(),this.startSignaling(),this.listenChildMessages(),this.createSettingsModal()}syncRouteToHash(e){if(this.peerId)try{let t=new URLSearchParams;this.secret?t.set("secret",this.secret):this.shareSign&&this.shareExp&&(t.set("exp",this.shareExp.toString()),t.set("sign",this.shareSign)),e&&e!=="/"&&t.set("path",e);let i=t.toString(),n="#"+this.peerId+(i?"?"+i:"");window.location.hash!==n&&window.history.replaceState(null,"",n)}catch{}}extractExplicitP2PSubdomain(){try{let e=window.location.hostname.toLowerCase().trim();if(!e)return null;let t=new Set(["p2p","www","webrtc","webrtc-p2p-signal","signal","signaling","gateway","portal","api","admin","proxy","vpn","tunnel"]),i=e.indexOf(".p2p.");if(i>0){let o=e.substring(0,i).split("."),s=o[o.length-1];if(s&&!t.has(s))return s}}catch{}return null}extractNodeIdFromHostname(){try{let e=window.location.hostname.toLowerCase().trim();if(!e||/^(\d{1,3}\.){3}\d{1,3}$/.test(e)||e==="localhost")return null;let t=[".pages.dev",".workers.dev",".eu.org",".github.io",".vercel.app",".netlify.app",".onrender.com",".fly.dev"];for(let o of t)if(e.endsWith(o)&&!e.slice(0,-o.length).includes("."))return null;let i=new Set(["p2p","www","webrtc","webrtc-p2p-signal","signal","signaling","gateway","portal","api","admin","proxy","vpn","tunnel","webrtc-gateway","p2p-gateway","app","web","client","relay","service","hub","test","demo","node","nodes","server","direct","static","assets","cdn"]),n=e.split(".");if(n.length>=3||n.length===2&&n[1]==="localhost"){let o=n[0];if(o&&!i.has(o))return o}}catch{}return null}resolveInitialConfig(){let e=new URLSearchParams(window.location.search),t=e.get("peer")||e.get("node"),i="",n="/",o=0,s="",r=e.get("path");r&&r.startsWith("/")&&!r.startsWith("//")&&!r.toLowerCase().includes("javascript:")&&(n=r);let d=e.get("secret")||e.get("token");if(d){i=d;try{e.delete("secret"),e.delete("token");let a=e.toString()?"?"+e.toString():"",c=window.location.pathname+a+window.location.hash;window.history.replaceState(null,"",c)}catch{}}if(window.location.hash){let a=window.location.hash.replace(/^#\/?/,"").trim();if(a){let c=a.split(/[?&]/);t||(t=c[0]);for(let m=1;m<c.length;m++){let[g,h]=c[m].split("=");if((g==="secret"||g==="token")&&h)i=decodeURIComponent(h);else if(g==="exp"&&h)o=parseInt(decodeURIComponent(h),10)||0;else if(g==="sign"&&h)s=decodeURIComponent(h);else if(g==="path"&&h){let y=decodeURIComponent(h);y.startsWith("/")&&!y.startsWith("//")&&!y.toLowerCase().includes("javascript:")&&(n=y)}}}}if(!t){let a=this.extractExplicitP2PSubdomain();a&&(t=a)}if(t||(t=sessionStorage.getItem("p2p_active_peer_id")||localStorage.getItem("p2p_peer_id")||""),!t){let a=this.extractNodeIdFromHostname();a&&(t=a)}t||(t=R),!i&&t&&(i=sessionStorage.getItem("p2p_secret")||localStorage.getItem("p2p_secret_"+t)||"");let p=window.location.hostname.includes("pages.dev")||window.location.hostname.includes("github.io")||window.location.hostname.includes("vercel.app"),l=e.get("signal");if(!l){let a=localStorage.getItem("p2p_signal_url");a?l=a:!p&&window.location.host?l=`${window.location.protocol==="https:"?"wss:":"ws:"}//${window.location.host}/ws`:l=L}try{sessionStorage.setItem("p2p_active_peer_id",t),localStorage.setItem("p2p_peer_id",t),localStorage.setItem("p2p_signal_url",l),i&&(sessionStorage.setItem("p2p_secret",i),localStorage.setItem("p2p_secret_"+t,i))}catch{}return{peerId:t,signalUrl:l,secret:i,path:n,exp:o,sign:s}}log(e){let t=new Date().toLocaleTimeString();this.logBox&&(this.logBox.textContent+=`[${t}] ${e}
`,this.logBox.scrollTop=this.logBox.scrollHeight),console.log(`[Portal] ${e}`)}bindEvents(){this.btnClearLog&&(this.btnClearLog.onclick=()=>{this.logBox&&(this.logBox.textContent="")}),this.btnPing&&(this.btnPing.onclick=async()=>{if(this.mux)try{let e=await this.mux.ping();this.rttVal.textContent=`${e.toFixed(1)} ms`,this.log(`Manual Ping successful: RTT = ${e.toFixed(2)} ms`)}catch(e){this.log(`Ping failed: ${e.message}`)}}),this.btnEcho&&(this.btnEcho.onclick=async()=>{await this.runEchoTest()}),document.getElementById("btnHeaderSettings")?.addEventListener("click",()=>{this.openSettingsModal()}),document.getElementById("cardPeer")?.addEventListener("click",()=>{this.openSettingsModal()}),document.getElementById("cardSignal")?.addEventListener("click",()=>{this.openSettingsModal()}),window.addEventListener("hashchange",()=>{let e=window.location.hash.replace(/^#\/?/,"").trim();if(e){let t=e.split(/[?&]/),i=t[0],n="",o=0,s="";for(let r=1;r<t.length;r++){let[d,p]=t[r].split("=");(d==="secret"||d==="token")&&p?n=decodeURIComponent(p):d==="exp"&&p?o=parseInt(decodeURIComponent(p),10)||0:d==="sign"&&p&&(s=decodeURIComponent(p))}if(i&&(i!==this.peerId||n&&n!==this.secret||s&&s!==this.shareSign)){if(this.log(`URL hash changed to #${e}, switching peer...`),this.peerId=i,this.shareExp=o,this.shareSign=s,n){this.secret=n;try{sessionStorage.setItem("p2p_secret",n),localStorage.setItem("p2p_secret_"+i,n)}catch{}}else s||(this.secret=sessionStorage.getItem("p2p_secret")||localStorage.getItem("p2p_secret_"+i)||"");try{sessionStorage.setItem("p2p_active_peer_id",i),localStorage.setItem("p2p_peer_id",i)}catch{}let r=navigator.serviceWorker?.controller;r&&r.postMessage({type:"NODE_CHANGE",peerId:i,secret:this.secret}),this.reconnect()}}})}async initServiceWorker(){if("serviceWorker"in navigator)try{this.log("Registering Service Worker (sw.js)..."),this.swRegistration=await navigator.serviceWorker.register("/sw.js",{scope:"/"}),await navigator.serviceWorker.ready,this.log("Service Worker is active and controlling scope /");let e=()=>{let t=navigator.serviceWorker.controller;if(!t)return;let i=new MessageChannel;i.port1.onmessage=n=>{this.handleSWMessage(n)},t.postMessage({type:"PORTAL_ATTACH",peerId:this.peerId,secret:this.secret},[i.port2]),this.log("Attached dedicated MessagePort tunnel to Service Worker")};e(),navigator.serviceWorker.addEventListener("controllerchange",e),navigator.serviceWorker.addEventListener("message",t=>{this.handleSWMessage(t)})}catch(e){this.log(`Failed to register Service Worker: ${e.message}`)}else this.log("Warning: Service Worker not supported or disabled (Insecure Context)."),this.showInsecureContextBanner()}startSignaling(){this.log(`Connecting to signaling server at ${this.signalUrl}...`),this.signalVal.textContent="Connecting...",this.ws=new WebSocket(this.signalUrl),this.ws.onopen=()=>{this.log("Signaling WebSocket connected. Joining peer room..."),this.signalVal.textContent="Connected (Joining...)",this.ws?.send(JSON.stringify({type:"join",node_id:this.peerId,secret:this.secret}))},this.ws.onmessage=async e=>{try{let t=JSON.parse(e.data);await this.handleSignalMessage(t)}catch(t){this.log(`Signaling parse error: ${t.message}`)}},this.ws.onerror=()=>{this.log("Signaling WebSocket error."),this.signalVal.textContent="Error"},this.ws.onclose=()=>{this.log("Signaling WebSocket closed."),this.signalVal.textContent="Disconnected"}}async handleSignalMessage(e){switch(e.type){case"peer_status":if(e.node_id&&e.node_id!==this.peerId)return;if(e.status==="online"){if(this.hideOfflinePrompt(),e.requires_secret&&!this.secret&&!this.shareSign){this.log(`Node [${this.peerId}] requires an access token (secret).`),this.signalVal&&(this.signalVal.textContent="Auth Required"),this.showAuthPrompt(this.peerId);return}this.hideAuthPrompt(),this.log(`Target peer [${this.peerId}] is ONLINE. Initializing WebRTC Offer...`),this.signalVal&&(this.signalVal.textContent="Peer Online"),await this.initWebRTC()}else this.log(`Target peer [${this.peerId}] is OFFLINE.`),this.signalVal&&(this.signalVal.textContent="Peer Offline"),this.showOfflinePrompt(this.peerId);break;case"answer":this.log("Received WebRTC Answer from Agent. Setting remote description..."),this.pc&&e.sdp&&(await this.pc.setRemoteDescription(new RTCSessionDescription(e.sdp)),this.log("Remote description applied successfully."));break;case"candidate":this.pc&&e.candidate&&await this.pc.addIceCandidate(new RTCIceCandidate(e.candidate));break;case"error":if(this.log(`Signaling error message: ${e.error}`),e.error&&e.error.toLowerCase().includes("unauthorized")){this.log(`Access rejected by node: ${e.error}`),this.signalVal&&(this.signalVal.textContent="Auth Failed (401)"),this.updateConnState("disconnected");let t=this.shareSign?"\u5206\u4EAB\u94FE\u63A5\u5DF2\u5931\u6548\u6216\u7B7E\u540D\u4E0D\u5339\u914D\uFF0C\u8BF7\u8F93\u5165\u8BBF\u95EE\u53E3\u4EE4":"\u8BBF\u95EE\u53E3\u4EE4\u4E0D\u6B63\u786E\u6216\u7F3A\u5931\uFF0C\u8BF7\u91CD\u65B0\u8F93\u5165";this.showAuthPrompt(this.peerId,t)}break}}async initWebRTC(){let e=[{urls:"stun:stun.qq.com:18302"},{urls:"stun:stun.miwifi.com:3478"},{urls:"stun:stun.bilibili.com:3478"},{urls:"stun:stun.cloudflare.com:3478"},{urls:"stun:stun.l.google.com:19302"}];try{let n=localStorage.getItem("p2p_stun_servers");n&&n.split(",").forEach(d=>{d=d.trim(),d&&e.unshift({urls:d.startsWith("stun:")?d:"stun:"+d})});let o=localStorage.getItem("p2p_turn_url"),s=localStorage.getItem("p2p_turn_user"),r=localStorage.getItem("p2p_turn_pass");if(o){let d={urls:o.startsWith("turn:")||o.startsWith("turns:")?o:"turn:"+o};s&&(d.username=s,d.credential=r||""),e.push(d),this.log(`Configured TURN relay: ${d.urls}`)}}catch{}let t={iceServers:e};this.pc=new RTCPeerConnection(t),this.updateConnState("connecting"),this.pc.onicecandidate=n=>{n.candidate&&this.ws?.readyState===WebSocket.OPEN&&this.ws.send(JSON.stringify({type:"candidate",node_id:this.peerId,candidate:n.candidate}))},this.pc.oniceconnectionstatechange=()=>{let n=this.pc?.iceConnectionState;this.log(`ICE Connection State: ${n}`),n==="connected"||n==="completed"?(this.isIceRestarting&&(this.log("ICE Restart succeeded! PeerConnection fully recovered with 0 session loss."),this.isIceRestarting=!1,this.iceRestartTimer&&clearTimeout(this.iceRestartTimer)),this.updateConnState("connected")):n==="disconnected"?(this.log("ICE connection disconnected (transient or network roaming). Scheduling ICE Restart..."),this.updateConnState("connecting"),setTimeout(()=>{(this.pc?.iceConnectionState==="disconnected"||this.pc?.iceConnectionState==="failed")&&this.triggerIceRestart()},1200)):n==="failed"&&(this.log("ICE connection failed! Triggering immediate ICE Restart..."),this.triggerIceRestart())},this.pc.onconnectionstatechange=()=>{let n=this.pc?.connectionState;this.log(`PeerConnection State: ${n}`),n==="connected"?(this.isIceRestarting&&(this.isIceRestarting=!1,this.iceRestartTimer&&clearTimeout(this.iceRestartTimer)),this.updateConnState("connected")):n==="failed"&&this.triggerIceRestart()},this.dc=this.pc.createDataChannel("p2pmux",{ordered:!0}),this.setupDataChannel(this.dc);let i=await this.pc.createOffer();await this.pc.setLocalDescription(i),this.log("Generated WebRTC Offer. Sending to Agent via Signaling..."),this.ws?.send(JSON.stringify({type:"offer",node_id:this.peerId,sdp:i,secret:this.secret,client_token:this.clientToken,exp:this.shareExp||void 0,sign:this.shareSign||void 0}))}setupDataChannel(e){e.onopen=()=>{this.log("DataChannel [p2pmux] OPENED! Direct P2P tunnel established."),this.updateConnState("connected"),this.mux=new w(e,!0),this.btnPing&&(this.btnPing.disabled=!1),this.btnEcho&&(this.btnEcho.disabled=!1),this.startPingLoop(),this.mountAppViewport()},e.onclose=()=>{this.log("DataChannel closed."),this.updateConnState("disconnected"),this.pingInterval&&clearInterval(this.pingInterval),this.btnPing&&(this.btnPing.disabled=!0),this.btnEcho&&(this.btnEcho.disabled=!0)},e.onerror=t=>{this.log(`DataChannel error: ${t}`)}}updateConnState(e){this.connBadge&&(this.connBadge.className=`badge ${e}`,this.connBadge.textContent=e.toUpperCase())}startPingLoop(){this.pingInterval&&clearInterval(this.pingInterval),this.pingInterval=setInterval(async()=>{if(this.mux)try{let e=await this.mux.ping();this.rttVal&&(this.rttVal.textContent=`${e.toFixed(1)} ms`)}catch{}},2500)}mountAppViewport(){if(!("serviceWorker"in navigator)||typeof window<"u"&&!window.isSecureContext){this.log("Service Worker is inactive (Insecure Context). App viewport mounting blocked to prevent blank recursion."),this.showInsecureContextBanner();return}let e=new URLSearchParams(window.location.search);if(!(e.get("mode")!=="debug"))return;let i=this.initialSubpath||e.get("path")||"/";if(this.appFrame){this.appFrame.src=i,this.createFloatingPill();return}this.log("Mounting App Shell Viewport iframe...");let n=document.createElement("iframe");n.id="appViewport",n.style.cssText=`
      position: fixed;
      top: 0;
      left: 0;
      width: 100vw;
      height: 100vh;
      border: none;
      z-index: 100;
      background: #ffffff;
    `,n.setAttribute("sandbox","allow-scripts allow-forms allow-same-origin allow-downloads allow-modals allow-popups allow-popups-to-escape-sandbox allow-presentation"),n.setAttribute("allow","fullscreen; clipboard-read; clipboard-write; autoplay; camera; microphone; display-capture; web-share; publickey-credentials-get; publickey-credentials-create; identity-credentials-get; cross-origin-isolated; storage-access; encrypted-media; presentation; screen-wake-lock; picture-in-picture"),n.src=i,document.body.appendChild(n),this.appFrame=n,n.addEventListener("load",()=>{try{n.contentDocument?.title&&(document.title=n.contentDocument.title)}catch{}try{n.contentWindow?.postMessage({type:"P2P_PORTAL_CONTEXT",nodeId:this.peerId,portalOrigin:window.location.origin,secret:this.secret},"*")}catch{}}),this.createFloatingPill()}showInsecureContextBanner(){if(document.getElementById("insecureBanner"))return;let e=document.createElement("div");e.id="insecureBanner",e.style.cssText=`
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
    `;let t=window.location.host,i=window.location.port||"8888";e.innerHTML=`
      <div style="display: flex; align-items: flex-start; gap: 14px;">
        <span style="font-size: 26px; line-height: 1;">\u26A0\uFE0F</span>
        <div style="flex: 1;">
          <h4 style="font-size: 15px; font-weight: 600; color: #e3b341; margin-bottom: 6px;">
            \u5F53\u524D\u5904\u4E8E\u975E\u5B89\u5168\u4E0A\u4E0B\u6587\uFF08Insecure Context\uFF09\uFF0C\u5DF2\u6682\u505C\u52A0\u8F7D\u76EE\u6807\u7F51\u9875
          </h4>
          <p style="color: #c9d1d9; margin-bottom: 10px;">
            \u6839\u636E W3C Web \u6807\u51C6\u4E0E\u73B0\u4EE3\u6D4F\u89C8\u5668\u5B89\u5168\u89C4\u8303\uFF1A\u5728\u975E HTTPS \u4E14\u975E localhost \u7684\u5C40\u57DF\u7F51\u7EAF HTTP IP\uFF08\u5982 <code>${window.location.origin}</code>\uFF09\u4E0B\uFF0C<strong>Service Worker \u5C06\u88AB\u5F3A\u5236\u7981\u7528</strong>\u3002\u672C\u7F51\u5173\u4F9D\u8D56 Service Worker \u865A\u62DF\u5316\u62E6\u622A\u548C\u8F6C\u53D1\u8BF7\u6C42\uFF0C\u8BF7\u9009\u62E9\u4EE5\u4E0B\u4EFB\u4E00\u65B9\u5F0F\u4F7F\u7528\uFF1A
          </p>
          <div style="background: #0d1117; border: 1px solid #30363d; border-radius: 6px; padding: 12px; margin-bottom: 10px;">
            <div style="margin-bottom: 8px;">
              <span style="color: #58a6ff; font-weight: 600;">\u65B9\u6848 1\uFF08\u672C\u673A\u8BBF\u95EE\uFF0C\u6700\u5FEB\uFF09</span>\uFF1A\u82E5\u6D4F\u89C8\u5668\u4E0E\u4FE1\u4EE4\u670D\u52A1\u8FD0\u884C\u5728\u540C\u4E00\u53F0\u8BBE\u5907\uFF0C\u8BF7\u76F4\u63A5\u5728\u5730\u5740\u680F\u6253\u5F00 
              <a href="http://localhost:${i}/#${this.peerId}" style="color: #388bfd; text-decoration: underline;">http://localhost:${i}/#${this.peerId}</a> \u6216 <code>http://127.0.0.1:${i}/#${this.peerId}</code>\uFF08localhost \u5C5E\u4E8E\u6807\u51C6\u5B89\u5168\u4E0A\u4E0B\u6587\uFF09\u3002
            </div>
            <div style="margin-bottom: 8px;">
              <span style="color: #3fb950; font-weight: 600;">\u65B9\u6848 2\uFF08\u751F\u4EA7\u63A8\u8350\uFF0C\u96F6\u914D\u7F6E\uFF09</span>\uFF1A\u90E8\u7F72\u5728\u652F\u6301 HTTPS \u7684\u57DF\u540D\u6216\u514D\u8D39\u90E8\u7F72\u81F3 Cloudflare Workers\uFF08\u5982 <code>https://p2p.yourdomain.com/#${this.peerId}</code>\uFF09\u3002
            </div>
            <div>
              <span style="color: #d29922; font-weight: 600;">\u65B9\u6848 3\uFF08\u5C40\u57DF\u7F51\u8DE8\u8BBE\u5907 HTTP \u514D\u8BC1\u4E66\u6D4B\u8BD5\uFF09</span>\uFF1A\u5728 Chrome/Edge \u6D4F\u89C8\u5668\u5730\u5740\u680F\u6253\u5F00\uFF1A<br/>
              <code>chrome://flags/#unsafely-treat-insecure-origin-as-secure</code>\uFF08Edge \u4E3A <code>edge://flags/...</code>\uFF09<br/>
              \u5C06 <code>http://${t}</code> \u586B\u5165\u767D\u540D\u5355\u8F93\u5165\u6846\uFF0C\u5E76\u5728\u53F3\u4FA7\u4E0B\u62C9\u6846\u9009\u62E9 <strong>Enabled</strong>\uFF0C\u70B9\u51FB <strong>Relaunch</strong> \u91CD\u542F\u6D4F\u89C8\u5668\u5373\u53EF\uFF01
            </div>
          </div>
          <div style="display: flex; justify-content: flex-end;">
            <button id="btnCloseInsecureBanner" style="background: #21262d; border: 1px solid #30363d; color: #c9d1d9; padding: 6px 14px; border-radius: 6px; font-size: 12px; cursor: pointer;">
              \u4FDD\u7559\u5728\u63A7\u5236\u53F0\u8C03\u8BD5\u89C6\u56FE
            </button>
          </div>
        </div>
      </div>
    `,document.body.appendChild(e),document.getElementById("btnCloseInsecureBanner")?.addEventListener("click",()=>{e.style.display="none"})}startHUDStatsMonitor(){this.hudStatsTimer||(this.hudStatsTimer=setInterval(async()=>{await this.updateWebRTCStats()},1e3))}async updateWebRTCStats(){if(this.pc)try{let e=await this.pc.getStats(),t=null,i=new Map;e.forEach(a=>{a.type==="candidate-pair"&&(a.selected||a.nominated&&a.state==="succeeded")&&(t=a),(a.type==="local-candidate"||a.type==="remote-candidate")&&i.set(a.id,a)}),t||e.forEach(a=>{a.type==="candidate-pair"&&a.state==="succeeded"&&(t=a)});let n=performance.now(),o=this.lastStatsTime>0?(n-this.lastStatsTime)/1e3:1,s=0,r=0,d=0,p=0,l=0;if(t){d=t.bytesReceived||0,p=t.bytesSent||0,this.lastStatsTime>0&&o>0&&(s=Math.max(0,(d-this.lastBytesReceived)/1024/o),r=Math.max(0,(p-this.lastBytesSent)/1024/o)),this.lastBytesReceived=d,this.lastBytesSent=p,this.lastStatsTime=n,typeof t.currentRoundTripTime=="number"?l=Math.round(t.currentRoundTripTime*1e3):typeof t.totalRoundTripTime=="number"&&t.responsesReceived>0&&(l=Math.round(t.totalRoundTripTime/t.responsesReceived*1e3));let a=i.get(t.localCandidateId),c=i.get(t.remoteCandidateId),m=(a?.candidateType||"").toLowerCase(),g=(c?.candidateType||"").toLowerCase(),h=m==="relay"||g==="relay";this.hudInfo={transportType:h?"relay":"direct",localType:m||"host",remoteType:g||"srflx",localAddress:a?`${a.address||a.ip||"?"}:${a.port||"?"}`:"Unknown",remoteAddress:c?`${c.address||c.ip||"?"}:${c.port||"?"}`:"Unknown",protocol:(a?.protocol||"udp").toUpperCase(),rttMs:l||(this.rttVal?parseInt(this.rttVal.textContent||"0"):0),rxRateKB:parseFloat(s.toFixed(1)),txRateKB:parseFloat(r.toFixed(1)),totalRxMB:parseFloat((d/(1024*1024)).toFixed(2)),totalTxMB:parseFloat((p/(1024*1024)).toFixed(2)),packetsLost:t.packetsLost||0,iceState:this.pc.iceConnectionState||"connected",connState:this.pc.connectionState||"connected"}}else if(this.hudInfo.iceState=this.pc.iceConnectionState,this.hudInfo.connState=this.pc.connectionState,this.rttVal){let a=parseInt(this.rttVal.textContent||"0");a>0&&(this.hudInfo.rttMs=a)}this.updateHUDUI()}catch{}}openHUDModal(){let e=document.getElementById("p2pHudModal");e&&(e.style.display="flex",this.updateHUDUI())}closeHUDModal(){let e=document.getElementById("p2pHudModal");e&&(e.style.display="none")}createHUDModal(){if(document.getElementById("p2pHudModal"))return;let e=document.createElement("div");e.id="p2pHudModal",e.style.cssText=`
      position: fixed;
      top: 0; left: 0; width: 100vw; height: 100vh;
      background: rgba(0,0,0,0.75);
      backdrop-filter: blur(8px);
      display: none; justify-content: center; align-items: center;
      z-index: 20000;
      font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, monospace;
    `,e.innerHTML=`
      <div style="background: #161b22; border: 1px solid #30363d; border-radius: 14px; padding: 24px; max-width: 520px; width: 92%; color: #c9d1d9; box-shadow: 0 20px 50px rgba(0,0,0,0.7);">
        <div style="display: flex; justify-content: space-between; align-items: center; margin-bottom: 20px;">
          <div style="display: flex; align-items: center; gap: 8px;">
            <span style="font-size: 18px;">\u26A1</span>
            <h3 style="font-size: 16px; color: #f0f6fc; font-weight: 600; margin: 0;">WebRTC Connection Diagnostic HUD</h3>
          </div>
          <button id="btnCloseHud" style="background: none; border: none; color: #8b949e; font-size: 20px; cursor: pointer; padding: 0 4px;">\u2715</button>
        </div>

        <!-- Section 1: Route & Mode -->
        <div style="background: #0d1117; border: 1px solid #21262d; border-radius: 10px; padding: 14px; margin-bottom: 16px;">
          <div style="display: flex; justify-content: space-between; align-items: center; margin-bottom: 10px;">
            <span style="font-size: 12px; color: #8b949e; text-transform: uppercase; font-weight: 600;">\u4F20\u8F93\u8FDE\u63A5\u6A21\u5F0F (Transport Mode)</span>
            <span id="hudBadge" style="display: inline-flex; align-items: center; gap: 4px; font-weight: 700; padding: 3px 10px; border-radius: 12px; font-size: 12px; background: #238636; color: #ffffff;">\u26A1 P2P Direct \u76F4\u8FDE</span>
          </div>
          <div style="display: grid; grid-template-columns: 1fr 1fr; gap: 10px; font-size: 12px;">
            <div>
              <div style="color: #8b949e; font-size: 11px;">\u672C\u5730\u8282\u70B9\u5019\u9009 (Local)</div>
              <div id="hudLocalCand" style="color: #f0f6fc; font-family: monospace; font-size: 11px; margin-top: 2px;">--</div>
            </div>
            <div>
              <div style="color: #8b949e; font-size: 11px;">\u8FDC\u7AEF\u7A7F\u900F\u76EE\u6807 (Remote)</div>
              <div id="hudRemoteCand" style="color: #f0f6fc; font-family: monospace; font-size: 11px; margin-top: 2px;">--</div>
            </div>
          </div>
          <div style="display: flex; gap: 16px; margin-top: 10px; padding-top: 8px; border-top: 1px solid #21262d; font-size: 11px; color: #8b949e;">
            <span>\u534F\u8BAE: <strong id="hudProto" style="color: #58a6ff;">UDP</strong></span>
            <span>ICE \u72B6\u6001: <strong id="hudIceState" style="color: #3fb950;">connected</strong></span>
            <span>\u8282\u70B9: <strong id="hudNodeId" style="color: #f0f6fc;">${this.peerId}</strong></span>
          </div>
        </div>

        <!-- Section 2: Realtime Metrics Cards -->
        <div style="display: grid; grid-template-columns: 1fr 1fr; gap: 10px; margin-bottom: 20px;">
          <div style="background: #0d1117; border: 1px solid #21262d; border-radius: 8px; padding: 12px;">
            <div style="font-size: 11px; color: #8b949e;">\u5F80\u8FD4\u7F51\u7EDC\u5EF6\u8FDF (RTT)</div>
            <div id="hudRtt" style="font-size: 20px; font-weight: 700; color: #58a6ff; font-family: monospace; margin-top: 4px;">-- ms</div>
          </div>
          <div style="background: #0d1117; border: 1px solid #21262d; border-radius: 8px; padding: 12px;">
            <div style="font-size: 11px; color: #8b949e;">\u77AC\u65F6\u4E0B\u8F7D\u901F\u7387 (Downlink)</div>
            <div id="hudRxRate" style="font-size: 20px; font-weight: 700; color: #3fb950; font-family: monospace; margin-top: 4px;">0 KB/s</div>
            <div id="hudTotalRx" style="font-size: 10px; color: #8b949e; margin-top: 2px;">\u7D2F\u8BA1: 0 MB</div>
          </div>
          <div style="background: #0d1117; border: 1px solid #21262d; border-radius: 8px; padding: 12px;">
            <div style="font-size: 11px; color: #8b949e;">\u77AC\u65F6\u4E0A\u4F20\u901F\u7387 (Uplink)</div>
            <div id="hudTxRate" style="font-size: 20px; font-weight: 700; color: #d29922; font-family: monospace; margin-top: 4px;">0 KB/s</div>
            <div id="hudTotalTx" style="font-size: 10px; color: #8b949e; margin-top: 2px;">\u7D2F\u8BA1: 0 MB</div>
          </div>
          <div style="background: #0d1117; border: 1px solid #21262d; border-radius: 8px; padding: 12px;">
            <div style="font-size: 11px; color: #8b949e;">\u4E22\u5305\u7EDF\u8BA1 (Packet Loss)</div>
            <div id="hudPacketsLost" style="font-size: 20px; font-weight: 700; color: #f0f6fc; font-family: monospace; margin-top: 4px;">0 pkts</div>
          </div>
        </div>

        <!-- Section 3: Actions -->
        <div style="display: flex; gap: 10px;">
          <button id="btnHudIceRestart" style="flex: 1; padding: 10px 14px; background: #21262d; border: 1px solid #30363d; color: #58a6ff; border-radius: 8px; font-size: 13px; font-weight: 600; cursor: pointer; transition: all 0.2s;">\u{1F504} \u6D4B\u8BD5 ICE Restart (\u5207\u7F51\u81EA\u6108)</button>
          <button id="btnHudOpenSettings" style="padding: 10px 14px; background: #238636; border: none; color: #ffffff; border-radius: 8px; font-size: 13px; font-weight: 600; cursor: pointer;">\u2699 \u8FDE\u63A5\u8BBE\u7F6E</button>
        </div>
        <div id="hudActionFeedback" style="font-size: 11px; color: #3fb950; margin-top: 8px; text-align: center; display: none;"></div>
      </div>
    `,document.body.appendChild(e),document.getElementById("btnCloseHud")?.addEventListener("click",()=>this.closeHUDModal()),e.addEventListener("click",t=>{t.target===e&&this.closeHUDModal()}),document.getElementById("btnHudIceRestart")?.addEventListener("click",async()=>{let t=document.getElementById("hudActionFeedback");t&&(t.style.display="block",t.style.color="#58a6ff",t.textContent="\u{1F504} \u6B63\u5728\u89E6\u53D1 WebRTC \u539F\u5730 ICE Restart \u91CD\u534F\u5546...");try{await this.triggerIceRestart(),t&&(t.style.color="#3fb950",t.textContent="\u2705 ICE Restart \u91CD\u534F\u5546\u6210\u529F\uFF0C\u6570\u636E\u901A\u9053\u4E0E\u4F1A\u8BDD\u65E0\u611F\u4FDD\u6301\uFF01",setTimeout(()=>{t&&(t.style.display="none")},4e3))}catch(i){t&&(t.style.color="#f85149",t.textContent=`\u274C ICE Restart \u5931\u8D25: ${i?.message||i}`)}}),document.getElementById("btnHudOpenSettings")?.addEventListener("click",()=>{this.closeHUDModal(),this.openSettingsModal()})}updateHUDUI(){let e=this.hudInfo.transportType==="relay",t=document.getElementById("pillModeBadge");t&&(e?(t.textContent="\u{1F6E1}\uFE0F TURN",t.style.background="#d29922"):(t.textContent="\u26A1 P2P",t.style.background="#238636"));let i=document.getElementById("pillNode");i&&(i.textContent=`P2P: ${this.peerId}`);let n=document.getElementById("pillRtt");n&&(n.textContent=`${this.hudInfo.rttMs||"--"} ms`);let o=document.getElementById("pillSpeed");o&&(o.textContent=`\u2193 ${this.hudInfo.rxRateKB} KB/s`);let s=document.getElementById("hudBadge");s&&(e?(s.textContent="\u{1F6E1}\uFE0F TURN RELAY (\u52A0\u5BC6\u4E2D\u7EE7)",s.style.background="#d29922"):(s.textContent="\u26A1 P2P DIRECT (\u6253\u6D1E\u76F4\u8FDE)",s.style.background="#238636"));let r=document.getElementById("hudLocalCand");r&&(r.textContent=`${this.hudInfo.localType} (${this.hudInfo.localAddress})`);let d=document.getElementById("hudRemoteCand");d&&(d.textContent=`${this.hudInfo.remoteType} (${this.hudInfo.remoteAddress})`);let p=document.getElementById("hudProto");p&&(p.textContent=this.hudInfo.protocol);let l=document.getElementById("hudIceState");l&&(l.textContent=this.hudInfo.iceState);let a=document.getElementById("hudNodeId");a&&(a.textContent=this.peerId);let c=document.getElementById("hudRtt");c&&(c.textContent=`${this.hudInfo.rttMs||"--"} ms`);let m=document.getElementById("hudRxRate");m&&(m.textContent=`${this.hudInfo.rxRateKB} KB/s`);let g=document.getElementById("hudTotalRx");g&&(g.textContent=`\u7D2F\u8BA1: ${this.hudInfo.totalRxMB} MB`);let h=document.getElementById("hudTxRate");h&&(h.textContent=`${this.hudInfo.txRateKB} KB/s`);let y=document.getElementById("hudTotalTx");y&&(y.textContent=`\u7D2F\u8BA1: ${this.hudInfo.totalTxMB} MB`);let I=document.getElementById("hudPacketsLost");I&&(I.textContent=`${this.hudInfo.packetsLost} pkts`)}createFloatingPill(){if(this.startHUDStatsMonitor(),this.createHUDModal(),this.createSettingsModal(),document.getElementById("p2pPill")){let t=document.getElementById("pillNode");t&&(t.textContent=`P2P: ${this.peerId}`);return}let e=document.createElement("div");e.id="p2pPill",e.style.cssText=`
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
    `,e.innerHTML=`
      <span id="pillModeBadge" style="display:inline-flex; align-items:center; gap:3px; font-weight:700; padding:2px 7px; border-radius:10px; font-size:11px; background:#238636; color:#ffffff; letter-spacing:0.5px;">\u26A1 P2P</span>
      <span id="pillNode" style="font-weight:500;" title="Click to open WebRTC HUD">P2P: ${this.peerId}</span>
      <span id="pillRtt" style="color:#58a6ff; font-family:monospace;">-- ms</span>
      <span id="pillSpeed" style="color:#8b949e; font-size:11px; font-family:monospace;">\u2193 0 KB/s</span>
      <span id="pillGear" style="cursor:pointer; opacity:0.75; font-size:13px; margin-left:2px;" title="Connection Settings">\u2699</span>
    `,e.addEventListener("click",t=>{let i=t.target;i&&i.id==="pillGear"?this.openSettingsModal():this.openHUDModal()}),document.body.appendChild(e)}createSettingsModal(){if(document.getElementById("p2pSettingsModal"))return;let e=document.createElement("div");e.id="p2pSettingsModal",e.style.cssText=`
      position: fixed;
      top: 0; left: 0; width: 100vw; height: 100vh;
      background: rgba(0,0,0,0.7);
      backdrop-filter: blur(6px);
      display: none; justify-content: center; align-items: center;
      z-index: 20000;
      font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, monospace;
    `,e.innerHTML=`
      <div style="background: #161b22; border: 1px solid #30363d; border-radius: 12px; padding: 24px; max-width: 440px; width: 90%; color: #c9d1d9; box-shadow: 0 16px 40px rgba(0,0,0,0.6);">
        <div style="display: flex; justify-content: space-between; align-items: center; margin-bottom: 20px;">
          <h3 style="font-size: 16px; color: #f0f6fc; font-weight: 600;">\u2699 P2P Gateway Settings</h3>
          <button id="btnCloseSettings" style="background: none; border: none; color: #8b949e; font-size: 18px; cursor: pointer;">\u2715</button>
        </div>
        <div style="margin-bottom: 16px;">
          <label style="display: block; font-size: 12px; color: #8b949e; text-transform: uppercase; margin-bottom: 4px; font-weight: 600;">\u76EE\u6807\u8282\u70B9 ID (Target Node ID)</label>
          <div style="font-size: 11px; color: #8b949e; margin-bottom: 8px;">\u652F\u6301\u4E8C\u7EA7/\u4E09\u7EA7\u5B50\u57DF\u540D\uFF08\u5982 <code>bili.p2p.example.com</code>\uFF09\u3001URL Hash\uFF08#nodeId\uFF09\u6216\u624B\u52A8\u6307\u5B9A\u3002</div>
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
          <label style="display: block; font-size: 12px; color: #8b949e; text-transform: uppercase; margin-bottom: 4px; font-weight: 600;">\u4FE1\u4EE4\u670D\u52A1\u7AEF\u5730\u5740 (Signaling Server URL)</label>
          <div style="font-size: 11px; color: #8b949e; margin-bottom: 8px;">\u8BBE\u7F6E WebRTC \u63E1\u624B\u64AE\u5408\u4FE1\u4EE4\u670D\u52A1\u5668 WebSocket \u5730\u5740\u3002</div>
          <input id="inputSignalUrl" type="text" value="${this.signalUrl}" placeholder="wss://..." style="width: 100%; box-sizing: border-box; background: #0d1117; border: 1px solid #30363d; color: #f0f6fc; padding: 8px 12px; border-radius: 6px; font-size: 13px; font-family: monospace;" />
          <div style="display: flex; flex-wrap: wrap; gap: 6px; margin-top: 8px;" id="signalChips">
            <span class="signal-chip" data-signal="current" style="cursor: pointer; font-size: 11px; padding: 3px 8px; border-radius: 12px; background: #21262d; border: 1px solid #30363d; color: #58a6ff;">\u5F53\u524D\u540C\u6E90\u4FE1\u4EE4 (Same-Origin /ws)</span>
            <span class="signal-chip" data-signal="ws://127.0.0.1:8888/ws" style="cursor: pointer; font-size: 11px; padding: 3px 8px; border-radius: 12px; background: #21262d; border: 1px solid #30363d; color: #58a6ff;">\u672C\u5730\u4FE1\u4EE4 (localhost:8888)</span>
          </div>
        </div>
        <div style="margin-bottom: 20px;">
          <label style="display: block; font-size: 12px; color: #8b949e; text-transform: uppercase; margin-bottom: 4px; font-weight: 600;">\u8BBF\u95EE\u53E3\u4EE4 / \u5BC6\u94A5 (Secret Token - \u53EF\u9009)</label>
          <div style="font-size: 11px; color: #8b949e; margin-bottom: 8px;">\u82E5\u76EE\u6807\u8282\u70B9\u5F00\u542F\u4E86 <code>-secret</code> \u4FDD\u62A4\uFF0C\u8BF7\u8F93\u5165\u5339\u914D\u7684\u53E3\u4EE4\uFF1B\u7559\u7A7A\u8868\u793A\u516C\u5F00\u8BBF\u95EE\u3002</div>
          <input id="inputSecret" type="password" value="${this.secret}" placeholder="Optional Secret Token" style="width: 100%; box-sizing: border-box; background: #0d1117; border: 1px solid #30363d; color: #f0f6fc; padding: 8px 12px; border-radius: 6px; font-size: 13px; font-family: monospace;" />
        </div>
        <div style="margin-bottom: 20px;">
          <label style="display: block; font-size: 12px; color: #8b949e; text-transform: uppercase; margin-bottom: 4px; font-weight: 600;">TURN \u4E2D\u7EE7\u7A7F\u900F (Symmetric NAT Relay - \u53EF\u9009)</label>
          <div style="font-size: 11px; color: #8b949e; margin-bottom: 8px;">\u5728\u4E25\u683C\u5BF9\u79F0\u578B NAT\uFF08\u4F01\u4E1A\u7F51/\u6821\u56ED\u7F51/\u79FB\u52A8\u7F51\u7EDC\uFF09\u6253\u6D1E\u5931\u8D25\u65F6\u65E0\u7F1D\u964D\u7EA7\u8D70\u4E2D\u7EE7\u3002</div>
          <input id="inputTurnUrl" type="text" placeholder="turn:turn.domain.com:3478" style="width: 100%; box-sizing: border-box; background: #0d1117; border: 1px solid #30363d; color: #f0f6fc; padding: 6px 12px; border-radius: 6px; font-size: 12px; font-family: monospace; margin-bottom: 6px;" />
          <div style="display: flex; gap: 8px;">
            <input id="inputTurnUser" type="text" placeholder="Username" style="flex: 1; box-sizing: border-box; background: #0d1117; border: 1px solid #30363d; color: #f0f6fc; padding: 6px 10px; border-radius: 6px; font-size: 12px; font-family: monospace;" />
            <input id="inputTurnPass" type="password" placeholder="Password" style="flex: 1; box-sizing: border-box; background: #0d1117; border: 1px solid #30363d; color: #f0f6fc; padding: 6px 10px; border-radius: 6px; font-size: 12px; font-family: monospace;" />
          </div>
        </div>
        <div style="margin-bottom: 20px; border-top: 1px solid #30363d; padding-top: 16px;">
          <label style="display: block; font-size: 12px; color: #8b949e; text-transform: uppercase; margin-bottom: 4px; font-weight: 600;">\u{1F517} \u9650\u65F6\u5B89\u5168\u5206\u4EAB\u94FE\u63A5 (Stateless Signed Share Link)</label>
          <div style="font-size: 11px; color: #8b949e; margin-bottom: 8px;">\u57FA\u4E8E HMAC-SHA256 \u7EAF\u6570\u5B66\u65F6\u6548\u7B7E\u540D\uFF0C\u65E0\u9700\u5BF9\u65B9\u8F93\u5165\u5BC6\u94A5\uFF0C\u5230\u671F\u81EA\u52A8\u5931\u6548\u3002</div>
          <div style="display: flex; gap: 8px; margin-bottom: 8px;">
            <select id="selShareDuration" style="flex: 1; background: #0d1117; border: 1px solid #30363d; color: #f0f6fc; padding: 6px 10px; border-radius: 6px; font-size: 12px;">
              <option value="3600">\u6709\u6548 1 \u5C0F\u65F6 (1 Hour)</option>
              <option value="21600">\u6709\u6548 6 \u5C0F\u65F6 (6 Hours)</option>
              <option value="86400" selected>\u6709\u6548 24 \u5C0F\u65F6 (1 Day)</option>
              <option value="604800">\u6709\u6548 7 \u5929 (7 Days)</option>
              <option value="2592000">\u6709\u6548 30 \u5929 (30 Days)</option>
            </select>
            <button id="btnGenShareLink" type="button" style="background: #238636; color: #ffffff; border: none; padding: 6px 14px; border-radius: 6px; font-size: 12px; font-weight: 600; cursor: pointer;">\u751F\u6210\u94FE\u63A5</button>
          </div>
          <div id="shareLinkContainer" style="display: none; flex-direction: column; gap: 6px;">
            <div style="display: flex; gap: 6px;">
              <input id="inputShareLink" type="text" readonly style="flex: 1; background: #0d1117; border: 1px solid #238636; color: #7ee787; padding: 6px 10px; border-radius: 6px; font-size: 11px; font-family: monospace;" />
              <button id="btnCopyShareLink" type="button" style="background: #21262d; border: 1px solid #30363d; color: #58a6ff; padding: 6px 12px; border-radius: 6px; font-size: 12px; cursor: pointer;">\u590D\u5236</button>
            </div>
            <div id="shareCopyTip" style="font-size: 11px; color: #3fb950; display: none;">\u2713 \u5DF2\u590D\u5236\u5230\u526A\u8D34\u677F\uFF01\u53EF\u4EE5\u76F4\u63A5\u53D1\u9001\u7ED9\u4ED6\u4EBA\u8BBF\u95EE\u3002</div>
          </div>
        </div>
        <div style="display: flex; gap: 10px; justify-content: flex-end;">
          <button id="btnToggleDebugView" style="background: #21262d; border: 1px solid #30363d; color: #c9d1d9; padding: 8px 14px; border-radius: 6px; font-size: 13px; cursor: pointer;">Toggle Debug View</button>
          <button id="btnSaveReconnect" style="background: #58a6ff; color: #0d1117; font-weight: 600; border: none; padding: 8px 16px; border-radius: 6px; font-size: 13px; cursor: pointer;">Save & Connect</button>
        </div>
      </div>
    `,document.body.appendChild(e),document.querySelectorAll(".node-chip").forEach(t=>{t.addEventListener("click",()=>{let i=t.getAttribute("data-node"),n=document.getElementById("inputNodeId");i&&n&&(n.value=i)})}),document.querySelectorAll(".signal-chip").forEach(t=>{t.addEventListener("click",()=>{let i=t.getAttribute("data-signal");i==="current"&&(i=`${window.location.protocol==="https:"?"wss:":"ws:"}//${window.location.host}/ws`);let n=document.getElementById("inputSignalUrl");i&&n&&(n.value=i)})}),document.getElementById("btnCloseSettings")?.addEventListener("click",()=>{e.style.display="none"}),e.addEventListener("click",t=>{t.target===e&&(e.style.display="none")}),document.getElementById("btnToggleDebugView")?.addEventListener("click",()=>{if(this.appFrame){let t=this.appFrame.style.display==="none";this.appFrame.style.display=t?"block":"none"}e.style.display="none"}),document.getElementById("btnSaveReconnect")?.addEventListener("click",()=>{let t=document.getElementById("inputNodeId"),i=document.getElementById("inputSignalUrl"),n=document.getElementById("inputSecret"),o=document.getElementById("inputTurnUrl"),s=document.getElementById("inputTurnUser"),r=document.getElementById("inputTurnPass"),d=t?.value.trim()||R,p=i?.value.trim()||L,l=n?n.value.trim():"";this.peerId=d,this.signalUrl=p,this.secret=l;try{sessionStorage.setItem("p2p_active_peer_id",d),localStorage.setItem("p2p_peer_id",d),localStorage.setItem("p2p_signal_url",p),l?(sessionStorage.setItem("p2p_secret",l),localStorage.setItem("p2p_secret_"+d,l)):(sessionStorage.removeItem("p2p_secret"),localStorage.removeItem("p2p_secret_"+d)),o&&o.value.trim()?(localStorage.setItem("p2p_turn_url",o.value.trim()),localStorage.setItem("p2p_turn_user",s?.value.trim()||""),localStorage.setItem("p2p_turn_pass",r?.value.trim()||"")):(localStorage.removeItem("p2p_turn_url"),localStorage.removeItem("p2p_turn_user"),localStorage.removeItem("p2p_turn_pass"))}catch{}let a=navigator.serviceWorker?.controller;a&&a.postMessage({type:"NODE_CHANGE",peerId:d,secret:l}),e.style.display="none",this.hideOfflinePrompt(),this.hideAuthPrompt(),this.reconnect()}),document.getElementById("btnGenShareLink")?.addEventListener("click",async()=>{let t=document.getElementById("inputNodeId"),i=document.getElementById("inputSecret"),n=document.getElementById("selShareDuration"),o=document.getElementById("shareLinkContainer"),s=document.getElementById("inputShareLink"),r=document.getElementById("shareCopyTip"),d=t?.value.trim()||this.peerId,p=i?.value.trim()||this.secret,l=parseInt(n?.value||"86400",10),a=window.location.origin+window.location.pathname;if(!p){let c=`${a}#${d}`;s&&(s.value=c),o&&(o.style.display="flex");return}try{let c=Math.floor(Date.now()/1e3)+l,m=`${d}:${c}`,g=await B(p,m),h=`${a}#${d}?exp=${c}&sign=${g}`;s&&(s.value=h,s.select()),o&&(o.style.display="flex"),r&&(r.style.display="none")}catch(c){alert("\u751F\u6210\u7B7E\u540D\u94FE\u63A5\u5931\u8D25: "+c.message)}}),document.getElementById("btnCopyShareLink")?.addEventListener("click",async()=>{let t=document.getElementById("inputShareLink"),i=document.getElementById("shareCopyTip");if(t&&t.value)try{await navigator.clipboard.writeText(t.value),i&&(i.style.display="block",setTimeout(()=>{i&&(i.style.display="none")},3e3))}catch{t.select(),document.execCommand("copy"),i&&(i.style.display="block",setTimeout(()=>{i&&(i.style.display="none")},3e3))}})}openSettingsModal(){let e=document.getElementById("p2pSettingsModal");if(e){e.style.display="flex";let t=document.getElementById("inputNodeId");t&&(t.value=this.peerId);let i=document.getElementById("inputSignalUrl");i&&(i.value=this.signalUrl);let n=document.getElementById("inputSecret");n&&(n.value=this.secret);let o=document.getElementById("inputTurnUrl");o&&(o.value=localStorage.getItem("p2p_turn_url")||"");let s=document.getElementById("inputTurnUser");s&&(s.value=localStorage.getItem("p2p_turn_user")||"");let r=document.getElementById("inputTurnPass");r&&(r.value=localStorage.getItem("p2p_turn_pass")||"")}}showOfflinePrompt(e){let t=document.getElementById("p2pOfflineOverlay");t||(t=document.createElement("div"),t.id="p2pOfflineOverlay",t.style.cssText=`
        position: fixed;
        top: 0; left: 0; width: 100vw; height: 100vh;
        background: rgba(13, 17, 23, 0.85);
        backdrop-filter: blur(10px);
        display: flex; justify-content: center; align-items: center;
        z-index: 15000;
        font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, monospace;
      `,document.body.appendChild(t)),t.innerHTML=`
      <div style="background: #161b22; border: 1px solid #30363d; border-radius: 12px; padding: 28px; max-width: 480px; width: 90%; color: #c9d1d9; box-shadow: 0 16px 40px rgba(0,0,0,0.6); text-align: center;">
        <div style="font-size: 36px; margin-bottom: 12px;">\u{1F4E1}</div>
        <h2 style="font-size: 18px; color: #f0f6fc; margin-bottom: 8px;">Target Node [${e}] is Offline</h2>
        <p style="font-size: 13px; color: #8b949e; line-height: 1.6; margin-bottom: 20px;">
          The remote agent daemon has not registered with the signaling relay yet. Please verify that <code>agent.exe</code> is running on your server, or switch to another node.
        </p>
        <div style="background: #0d1117; border: 1px solid #30363d; border-radius: 6px; padding: 10px; font-size: 12px; font-family: monospace; color: #58a6ff; margin-bottom: 20px; text-align: left; word-break: break-all;">
          ./agent.exe -node-id ${e} -signal ${this.signalUrl}
        </div>
        <div style="display: flex; gap: 12px; justify-content: center;">
          <button id="btnRetryConn" style="background: #58a6ff; color: #0d1117; font-weight: 600; border: none; padding: 8px 18px; border-radius: 6px; cursor: pointer;">Retry</button>
          <button id="btnOpenSettings" style="background: #21262d; border: 1px solid #30363d; color: #c9d1d9; padding: 8px 18px; border-radius: 6px; cursor: pointer;">Switch Node</button>
        </div>
      </div>
    `,document.getElementById("btnRetryConn")?.addEventListener("click",()=>{this.reconnect()}),document.getElementById("btnOpenSettings")?.addEventListener("click",()=>{this.openSettingsModal()})}hideOfflinePrompt(){let e=document.getElementById("p2pOfflineOverlay");e&&e.remove()}showAuthPrompt(e,t){let i=document.getElementById("p2pAuthOverlay");i||(i=document.createElement("div"),i.id="p2pAuthOverlay",i.style.cssText=`
        position: fixed;
        top: 0; left: 0; width: 100vw; height: 100vh;
        background: rgba(13, 17, 23, 0.85);
        backdrop-filter: blur(10px);
        display: flex; justify-content: center; align-items: center;
        z-index: 25000;
        font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, monospace;
      `,document.body.appendChild(i)),i.innerHTML=`
      <div style="background: #161b22; border: 1px solid #30363d; border-radius: 12px; padding: 28px; max-width: 440px; width: 90%; color: #c9d1d9; box-shadow: 0 16px 40px rgba(0,0,0,0.6); text-align: center;">
        <div style="font-size: 36px; margin-bottom: 12px;">\u{1F510}</div>
        <h2 style="font-size: 18px; color: #f0f6fc; margin-bottom: 8px;">\u8BBF\u95EE\u53E3\u4EE4\u9274\u6743 (Auth Required)</h2>
        <p style="font-size: 13px; color: #8b949e; line-height: 1.6; margin-bottom: 16px;">
          \u76EE\u6807\u8282\u70B9 <code style="color: #58a6ff; font-weight: 600;">[${e}]</code> \u5DF2\u5F00\u542F\u8BBF\u95EE\u4FDD\u62A4\uFF0C\u8BF7\u8F93\u5165\u8BBF\u95EE\u5BC6\u94A5\uFF1A
        </p>
        ${t?`<div style="background: rgba(248, 81, 73, 0.15); border: 1px solid rgba(248, 81, 73, 0.4); color: #f85149; border-radius: 6px; padding: 8px 12px; font-size: 12px; margin-bottom: 16px;">${t}</div>`:""}
        <div style="margin-bottom: 16px; text-align: left;">
          <input id="inputAuthSecret" type="password" placeholder="\u8BF7\u8F93\u5165\u53E3\u4EE4 / Secret Token..." value="${this.secret}" style="width: 100%; box-sizing: border-box; background: #0d1117; border: 1px solid #30363d; color: #f0f6fc; padding: 10px 12px; border-radius: 6px; font-size: 14px; font-family: monospace;" />
        </div>
        <div style="display: flex; align-items: center; gap: 8px; margin-bottom: 20px; font-size: 12px; color: #8b949e; cursor: pointer;">
          <input type="checkbox" id="chkRememberSecret" checked style="cursor: pointer;" />
          <label for="chkRememberSecret" style="cursor: pointer;">\u8BB0\u4F4F\u6B64\u8282\u70B9\u7684\u8BBF\u95EE\u53E3\u4EE4 (Remember)</label>
        </div>
        <div style="display: flex; gap: 12px; justify-content: center;">
          <button id="btnSubmitAuth" style="background: #238636; color: #ffffff; font-weight: 600; border: none; padding: 9px 22px; border-radius: 6px; cursor: pointer; font-size: 13px;">\u9A8C\u8BC1\u5E76\u8FDE\u63A5</button>
          <button id="btnAuthSwitchNode" style="background: #21262d; border: 1px solid #30363d; color: #c9d1d9; padding: 9px 18px; border-radius: 6px; cursor: pointer; font-size: 13px;">\u5207\u6362\u8282\u70B9</button>
        </div>
      </div>
    `;let n=document.getElementById("inputAuthSecret");n&&(setTimeout(()=>n.focus(),50),n.addEventListener("keydown",s=>{s.key==="Enter"&&o()}));let o=()=>{let s=n?n.value.trim():"",r=document.getElementById("chkRememberSecret")?.checked;if(this.secret=s,r)try{localStorage.setItem("p2p_secret_"+e,s)}catch{}else try{localStorage.removeItem("p2p_secret_"+e)}catch{}this.hideAuthPrompt(),this.reconnect()};document.getElementById("btnSubmitAuth")?.addEventListener("click",o),document.getElementById("btnAuthSwitchNode")?.addEventListener("click",()=>{this.hideAuthPrompt(),this.openSettingsModal()})}hideAuthPrompt(){let e=document.getElementById("p2pAuthOverlay");e&&e.remove()}async triggerIceRestart(){if(!this.isIceRestarting){if(!this.pc||this.pc.signalingState==="closed"){this.reconnect();return}this.isIceRestarting=!0,this.log("Network interruption detected. Initiating WebRTC ICE Restart self-healing..."),this.updateConnState("connecting"),this.iceRestartTimer&&clearTimeout(this.iceRestartTimer),this.iceRestartTimer=setTimeout(()=>{this.isIceRestarting&&(this.log("ICE Restart grace window timed out (6s). Falling back to full session reconnection..."),this.isIceRestarting=!1,this.reconnect())},6e3);try{await this.ensureSignalingConnected(),this.log("Generating in-place ICE Restart Offer...");let e=await this.pc.createOffer({iceRestart:!0});await this.pc.setLocalDescription(e),this.log("Transmitting ICE Restart Offer to Agent via Signaling..."),this.ws?.send(JSON.stringify({type:"offer",node_id:this.peerId,sdp:e,secret:this.secret,client_token:this.clientToken,ice_restart:!0,exp:this.shareExp||void 0,sign:this.shareSign||void 0}))}catch(e){this.log(`ICE Restart initiation failed: ${e.message}. Falling back to full reconnect.`),this.iceRestartTimer&&clearTimeout(this.iceRestartTimer),this.isIceRestarting=!1,this.reconnect()}}}ensureSignalingConnected(){return this.ws&&this.ws.readyState===WebSocket.OPEN?Promise.resolve():new Promise((e,t)=>{let i=setTimeout(()=>{t(new Error("Signaling reconnect timeout"))},4e3);try{if(this.ws){try{this.ws.close()}catch{}this.ws=null}this.log(`Re-establishing signaling WebSocket to ${this.signalUrl}...`),this.ws=new WebSocket(this.signalUrl),this.ws.onopen=()=>{clearTimeout(i),this.log("Signaling WebSocket reconnected for ICE Restart. Re-joining room..."),this.ws?.send(JSON.stringify({type:"join",node_id:this.peerId,secret:this.secret})),e()},this.ws.onmessage=async n=>{try{let o=JSON.parse(n.data);await this.handleSignalMessage(o)}catch(o){this.log(`Signaling parse error: ${o.message}`)}},this.ws.onerror=()=>{clearTimeout(i),t(new Error("Signaling WebSocket connection failed"))},this.ws.onclose=()=>{this.log("Signaling WebSocket closed.")}}catch(n){clearTimeout(i),t(n)}})}reconnect(){this.hideOfflinePrompt(),this.hideAuthPrompt(),this.isIceRestarting=!1,this.iceRestartTimer&&(clearTimeout(this.iceRestartTimer),this.iceRestartTimer=null),this.log(`Reconnecting to node [${this.peerId}] via ${this.signalUrl}...`);let e=new URLSearchParams;this.secret?e.set("secret",this.secret):this.shareSign&&this.shareExp&&(e.set("exp",this.shareExp.toString()),e.set("sign",this.shareSign));let t=e.toString(),i="#"+this.peerId+(t?"?"+t:"");window.location.hash!==i&&(window.location.hash=i),this.peerVal&&(this.peerVal.textContent=this.peerId),this.signalVal&&(this.signalVal.textContent=this.signalUrl);try{sessionStorage.setItem("p2p_active_peer_id",this.peerId),localStorage.setItem("p2p_peer_id",this.peerId),this.secret&&(sessionStorage.setItem("p2p_secret",this.secret),localStorage.setItem("p2p_secret_"+this.peerId,this.secret))}catch{}let n=navigator.serviceWorker?.controller;if(n&&n.postMessage({type:"NODE_CHANGE",peerId:this.peerId,secret:this.secret}),this.appFrame&&(this.appFrame.remove(),this.appFrame=null),this.vaultContainer&&(this.vaultContainer.remove(),this.vaultContainer=null),this.mediaContainer&&(this.mediaContainer.remove(),this.mediaContainer=null),this.pingInterval&&clearInterval(this.pingInterval),this.mux){try{this.dc?.close()}catch{}this.mux=null}if(this.pc){try{this.pc.close()}catch{}this.pc=null}if(this.ws){try{this.ws.close()}catch{}this.ws=null}this.updateConnState("disconnected"),this.startSignaling()}handleSWMessage(e){let t=e.data;if(!t||t.type!=="TUNNEL_FETCH_REQUEST")return;if(!this.mux){e.ports&&e.ports[0]&&e.ports[0].postMessage({type:"RESP_ERROR",error:"P2P DataChannel is not connected yet"});return}let i=e.ports[0],n=this.mux.openStream();this.updateStreamsCount(1),n.sendHeaders({m:t.method,p:t.path,h:t.headers});let o=Promise.resolve();i.onmessage=s=>{let r=s.data;switch(r.type){case"REQ_BODY_CHUNK":if(r.chunk){let d=new Uint8Array(r.chunk);o=o.then(async()=>{try{await n.write(d)}catch{}})}break;case"REQ_BODY_END":o=o.then(async()=>{try{n.close()}catch{}});break;case"RESP_CANCEL":n.reset();break}},n.onHeaders(s=>{if(s.c&&Array.isArray(s.c))for(let m of s.c)try{document.cookie=m}catch{}let r=s.h||{},d=(r["content-type"]||"").toLowerCase(),p=(r["content-disposition"]||"").toLowerCase(),l=r["x-p2p-mode"]||"",a=r["x-p2p-file-name"]||"",c=parseInt(r["x-p2p-file-size"]||"0",10)||0;if(t.destination==="iframe"){let m=d.includes("application/pdf")||a&&a.toLowerCase().endsWith(".pdf"),g=d.startsWith("video/")||a&&a.match(/\.(mp4|webm|mkv|mov|avi|ts|m4v)$/i),h=d.startsWith("audio/")||a&&a.match(/\.(mp3|wav|flac|aac|m4a|ogg)$/i),y=p.includes("attachment")||l==="file"&&!d.includes("text/html")&&!d.startsWith("image/");m?this.showFileVault({fileName:a||this.extractFileName(t.path),fileSize:c,type:"pdf",path:t.path,isDir:l==="dir"}):g?this.showNativeMediaPlayer({src:t.path,fileName:a||this.extractFileName(t.path),isDir:l==="dir",type:"video"}):h?this.showNativeMediaPlayer({src:t.path,fileName:a||this.extractFileName(t.path),isDir:l==="dir",type:"audio"}):y?this.showFileVault({fileName:a||this.extractFileName(t.path),fileSize:c,type:"file",path:t.path,isDir:l==="dir"}):this.appFrame&&this.appFrame.style.display==="none"&&(this.mediaContainer&&(this.mediaContainer.remove(),this.mediaContainer=null),this.vaultContainer&&(this.vaultContainer.remove(),this.vaultContainer=null),this.appFrame.style.display="block")}i.postMessage({type:"RESP_START",status:s.s||200,statusText:s.m||"OK",headers:r})}),n.onData((s,r)=>{if(s.byteLength>0){let d=s.buffer.slice(s.byteOffset,s.byteOffset+s.byteLength);i.postMessage({type:"RESP_CHUNK",chunk:d},[d])}r&&(i.postMessage({type:"RESP_END"}),this.updateStreamsCount(-1))})}listenChildMessages(){window.addEventListener("message",e=>{let t=e.data;if(t)if(t.type==="TUNNEL_COOKIE_UPDATE")try{document.cookie=t.cookie}catch{}else t.type==="TUNNEL_WS_CONNECT"&&this.handleWSConnect(t,e.ports[0])})}handleWSConnect(e,t){if(!this.mux){t.postMessage({type:"WS_ERROR",error:"P2P tunnel not open"});return}let i=this.mux.openStream();this.updateStreamsCount(1),this.log(`Bridging WebSocket connection for [${e.path}] over Stream ID ${i.id}`),i.sendHeaders({p:e.path,protocols:e.protocols,h:{upgrade:"websocket"}}),i.onHeaders(o=>{o.s===101?t.postMessage({type:"WS_OPENED",protocol:o.h?.["sec-websocket-protocol"]}):t.postMessage({type:"WS_ERROR",error:o.m||"WS Upgrade failed"})});let n=new Uint8Array(0);i.onData((o,s)=>{if(o.byteLength>0){let r=new Uint8Array(n.byteLength+o.byteLength);for(r.set(n,0),r.set(o,n.byteLength),n=r;n.byteLength>=5;){let d=n[0],l=new DataView(n.buffer,n.byteOffset,n.byteLength).getUint32(1,!1);if(n.byteLength<5+l)break;let a=n.subarray(5,5+l);if(d===1){let c=new TextDecoder().decode(a);t.postMessage({type:"WS_MSG",data:c})}else{let c=n.buffer.slice(n.byteOffset+5,n.byteOffset+5+l);t.postMessage({type:"WS_MSG",data:c},[c])}n=n.subarray(5+l)}}s&&(t.postMessage({type:"WS_CLOSED"}),this.updateStreamsCount(-1))}),t.onmessage=async o=>{let s=o.data;switch(s.type){case"WS_SEND":let r;typeof s.data=="string"?r=new TextEncoder().encode(s.data):r=new Uint8Array(s.data);let d=new Uint8Array(5+r.byteLength);d[0]=s.opcode||1,new DataView(d.buffer).setUint32(1,r.byteLength,!1),d.set(r,5),await i.write(d);break;case"WS_CLOSE":i.close(),this.updateStreamsCount(-1);break}}}updateStreamsCount(e){this.activeStreamsCount=Math.max(0,this.activeStreamsCount+e),this.streamsVal&&(this.streamsVal.textContent=`${this.activeStreamsCount} Active`)}async runEchoTest(){if(!this.mux)return;this.btnEcho.disabled=!0,this.testOutput.textContent="Running echo test over P2P stream...";let e="Hello P2PMux! Zero-client intranet penetration gateway is working! Data: "+Array.from({length:50},(s,r)=>`[Chunk#${r}]`).join(" "),t=new TextEncoder().encode(e),i=this.mux.openStream();this.log(`Opened new P2P Stream (ID=${i.id}). Sending ${t.byteLength} bytes...`);let n=performance.now(),o=[];i.onData((s,r)=>{if(s.byteLength>0&&o.push(s),r){let d=performance.now()-n,p=o.reduce((h,y)=>h+y.byteLength,0),l=new Uint8Array(p),a=0;for(let h of o)l.set(h,a),a+=h.byteLength;let m=new TextDecoder().decode(l)===e,g=`Stream ID ${i.id} finished in ${d.toFixed(1)}ms. Transferred: ${p} bytes. Verification: ${m?"MATCHED (100% OK)":"MISMATCH"}`;this.log(g),this.testOutput.textContent=g,this.btnEcho.disabled=!1}});try{await i.write(t),i.close()}catch(s){this.log(`Stream write error: ${s.message}`),this.btnEcho.disabled=!1}}showFileVault(e){this.appFrame&&(this.appFrame.style.display="none"),this.mediaContainer&&(this.mediaContainer.remove(),this.mediaContainer=null),this.vaultContainer&&(this.vaultContainer.remove(),this.vaultContainer=null);let t=document.createElement("div");t.id="p2pFileVault",t.style.cssText=`
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
    `;let i=this.formatFileSize(e.fileSize),n=e.type==="pdf"||e.fileName.toLowerCase().endsWith(".pdf"),o=n?"\u{1F4C4}":e.fileName.match(/\.(zip|tar|gz|7z|rar|iso)$/i)?"\u{1F4E6}":"\u{1F4BE}",s=e.isDir?e.path:"/"+encodeURIComponent(e.fileName||"file");t.innerHTML=`
      <div style="background: #161b22; border: 1px solid #30363d; border-radius: 12px; max-width: 540px; width: 100%; padding: 32px; box-shadow: 0 16px 36px rgba(0,0,0,0.6); text-align: center;">
        <div style="font-size: 54px; margin-bottom: 16px; line-height: 1;">${o}</div>
        <h2 style="font-size: 18px; font-weight: 600; color: #f0f6fc; margin-bottom: 8px; word-break: break-all; line-height: 1.4;">
          ${this.escapeHtml(e.fileName)}
        </h2>
        <div style="display: flex; justify-content: center; align-items: center; gap: 12px; margin-bottom: 24px;">
          ${i?`<span style="font-size: 13px; color: #8b949e; background: #21262d; padding: 2px 10px; border-radius: 12px; border: 1px solid #30363d;">${i}</span>`:""}
          <span style="font-size: 13px; color: #3fb950; background: rgba(63, 185, 80, 0.15); padding: 2px 10px; border-radius: 12px; border: 1px solid #3fb950;">\u26A1 P2P Direct Tunnel</span>
        </div>

        <div style="display: flex; flex-direction: column; gap: 12px; margin-bottom: 20px;">
          <button id="btnVaultDownload" style="background: #238636; color: #ffffff; border: 1px solid rgba(240, 246, 252, 0.1); border-radius: 6px; padding: 12px 20px; font-size: 15px; font-weight: 600; cursor: pointer; display: flex; align-items: center; justify-content: center; gap: 8px; transition: 0.2s;">
            \u26A1 \u6781\u901F P2P \u4E0B\u8F7D (Download)
          </button>

          ${n?`
          <button id="btnVaultPreview" style="background: #21262d; border: 1px solid #30363d; color: #58a6ff; border-radius: 6px; padding: 10px 20px; font-size: 14px; font-weight: 500; cursor: pointer; display: flex; align-items: center; justify-content: center; gap: 8px; transition: 0.2s;">
            \u{1F4D1} \u5728\u65B0\u6807\u7B7E\u9875\u6253\u5F00\u539F\u751F\u9884\u89C8 (Open in New Tab)
          </button>
          `:""}

          ${e.isDir?`
          <button id="btnVaultBack" style="background: transparent; border: 1px solid #30363d; color: #8b949e; border-radius: 6px; padding: 8px 16px; font-size: 13px; cursor: pointer; transition: 0.2s;">
            \u2B05 \u8FD4\u56DE\u6587\u4EF6\u76EE\u5F55 (Back to Directory)
          </button>
          `:""}
        </div>

        ${n?`
        <p style="font-size: 12px; color: #8b949e; line-height: 1.5; margin: 0; text-align: left; background: #0d1117; padding: 10px 14px; border-radius: 6px; border: 1px solid #21262d;">
          \u{1F4A1} <strong>\u9884\u89C8\u63D0\u793A</strong>\uFF1A\u7531\u4E8E\u6D4F\u89C8\u5668\u5B89\u5168\u6C99\u7BB1\u9650\u5236\uFF0C\u5185\u5D4C\u6C99\u7BB1\u73AF\u5883\u5DF2\u7981\u7528 PDF \u63D2\u4EF6\u3002\u684C\u9762\u7AEF\u8BF7\u70B9\u51FB<strong>\u3010\u5728\u65B0\u6807\u7B7E\u9875\u6253\u5F00\u539F\u751F\u9884\u89C8\u3011</strong>\uFF1B\u79FB\u52A8\u7AEF\u63A8\u8350\u70B9\u51FB<strong>\u3010\u6781\u901F P2P \u4E0B\u8F7D\u3011</strong>\u4F7F\u7528\u7CFB\u7EDF\u9605\u8BFB\u5668\u6D4F\u89C8\u3002
        </p>
        `:""}
      </div>
    `,document.body.appendChild(t),this.vaultContainer=t,document.getElementById("btnVaultDownload")?.addEventListener("click",()=>{let r=document.createElement("a");r.href=s,r.download=e.fileName,document.body.appendChild(r),r.click(),r.remove()}),n&&document.getElementById("btnVaultPreview")?.addEventListener("click",()=>{window.open(s,"_blank")}),e.isDir&&document.getElementById("btnVaultBack")?.addEventListener("click",()=>{this.closeVaultAndRestoreIframe()})}showNativeMediaPlayer(e){this.appFrame&&(this.appFrame.style.display="none"),this.vaultContainer&&(this.vaultContainer.remove(),this.vaultContainer=null),this.mediaContainer&&(this.mediaContainer.remove(),this.mediaContainer=null);let t=document.createElement("div");t.id="p2pMediaPlayerContainer",t.style.cssText=`
      position: fixed;
      top: 0;
      left: 0;
      width: 100vw;
      height: 100vh;
      background: #000000;
      display: flex;
      flex-direction: column;
      align-items: center;
      justify-content: center;
      z-index: 100;
    `;let i=e.isDir?e.src:"/"+encodeURIComponent(e.fileName||"media");if(e.isDir){let n=document.createElement("div");n.style.cssText=`
        position: absolute;
        top: 16px;
        left: 20px;
        display: flex;
        align-items: center;
        gap: 12px;
        z-index: 102;
      `,n.innerHTML=`
        <button id="btnMediaBack" style="background: rgba(33, 38, 45, 0.85); backdrop-filter: blur(8px); border: 1px solid #30363d; color: #f0f6fc; border-radius: 6px; padding: 6px 14px; font-size: 13px; font-weight: 500; cursor: pointer; display: flex; align-items: center; gap: 6px;">
          \u2B05 \u8FD4\u56DE\u76EE\u5F55
        </button>
        <span style="color: #c9d1d9; font-size: 13px; font-weight: 500; max-width: 50vw; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; text-shadow: 0 2px 4px rgba(0,0,0,0.8);">
          ${this.escapeHtml(e.fileName)}
        </span>
      `,t.appendChild(n)}if(e.type==="video"){let n=document.createElement("video");n.id="p2pNativeVideo",n.controls=!0,n.autoplay=!0,n.playsInline=!0,n.style.cssText="width: 100vw; height: 100vh; object-fit: contain; background: #000;",n.src=i,t.appendChild(n)}else{let n=document.createElement("div");n.style.cssText=`
        background: #161b22;
        border: 1px solid #30363d;
        border-radius: 12px;
        padding: 32px;
        max-width: 500px;
        width: 90%;
        text-align: center;
        box-shadow: 0 16px 36px rgba(0,0,0,0.6);
      `,n.innerHTML=`
        <div style="font-size: 48px; margin-bottom: 16px;">\u{1F3B5}</div>
        <h3 style="font-size: 16px; color: #f0f6fc; margin-bottom: 20px; word-break: break-all;">${this.escapeHtml(e.fileName)}</h3>
        <audio controls autoplay style="width: 100%;" src="${i}"></audio>
      `,t.appendChild(n)}document.body.appendChild(t),this.mediaContainer=t,e.isDir&&document.getElementById("btnMediaBack")?.addEventListener("click",()=>{this.closeMediaAndRestoreIframe()})}closeMediaAndRestoreIframe(){if(this.mediaContainer){let e=this.mediaContainer.querySelector("video");e&&(e.pause(),e.src="",e.load());let t=this.mediaContainer.querySelector("audio");t&&(t.pause(),t.src="",t.load()),this.mediaContainer.remove(),this.mediaContainer=null}this.appFrame&&(this.appFrame.style.display="block",this.appFrame.src="/")}closeVaultAndRestoreIframe(){this.vaultContainer&&(this.vaultContainer.remove(),this.vaultContainer=null),this.appFrame&&(this.appFrame.style.display="block",this.appFrame.src="/")}formatFileSize(e){if(!e||e<=0)return"";let t=["B","KB","MB","GB","TB"],i=e,n=0;for(;i>=1024&&n<t.length-1;)i/=1024,n++;return`${i.toFixed(i>=10||n===0?0:1)} ${t[n]}`}escapeHtml(e){return e.replace(/&/g,"&amp;").replace(/</g,"&lt;").replace(/>/g,"&gt;").replace(/"/g,"&quot;").replace(/'/g,"&#039;")}extractFileName(e){try{let i=e.split("?")[0].split("#")[0].split("/").filter(Boolean);if(i.length>0)return decodeURIComponent(i[i.length-1])}catch{}return"file"}};window.addEventListener("DOMContentLoaded",()=>{new S})});export default _();
