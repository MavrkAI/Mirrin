(() => {
  'use strict';
  const nativeFetch = window.fetch.bind(window);
  const standalone = () => matchMedia('(display-mode: standalone)').matches || navigator.standalone === true;
  // A page cached before the rename names the twin under the old meta name.
  const twin = () => document.querySelector('meta[name="mirrin-name"],meta[name="antbot-name"]')?.content || 'your twin'; // rename:keep
  let installPrompt;
  addEventListener('beforeinstallprompt', e => { e.preventDefault(); installPrompt = e; });
  function say(text) {
    let el = document.getElementById('pwa-status');
    if (!el) { el = document.createElement('p'); el.id='pwa-status'; el.setAttribute('role','status'); el.style.cssText='position:fixed;top:1rem;left:1rem;right:1rem;z-index:1000;padding:1rem;background:#111318;color:#eceef3;border:1px solid #7aa2ff;border-radius:.7rem'; el.onclick=()=>el.remove();document.body.prepend(el); }
    el.textContent = text;
  }
  function saved(key, value) { try { if (value !== undefined) localStorage.setItem(key,value); return localStorage.getItem(key); } catch { return null; } }
  function ticket(value) { try { if(value) sessionStorage.setItem('antbot-install-ticket',value); return sessionStorage.getItem('antbot-install-ticket'); } catch { return null; } }
  function manifest(t) {
    const iosSafari = (/iPad|iPhone|iPod/.test(navigator.userAgent) || (navigator.platform === 'MacIntel' && navigator.maxTouchPoints > 1)) && /Safari/.test(navigator.userAgent) && !/CriOS|FxiOS|EdgiOS|OPiOS/.test(navigator.userAgent);
    if (!iosSafari) return;
    if (!/^it_[A-Za-z0-9_-]{24}$/.test(t || '')) return;
    const link = document.querySelector('link[rel="manifest"]');
    if (link) link.href = '/manifest.webmanifest?t=' + encodeURIComponent(t);
  }
  // Passkeys (step-up). WebAuthn takes and gives bytes; the twin speaks
  // base64url JSON.
  const unb64 = s => Uint8Array.from(atob(String(s).replace(/-/g,'+').replace(/_/g,'/') + '==='.slice((String(s).length + 3) % 4)), c => c.charCodeAt(0));
  const b64 = buf => btoa(String.fromCharCode(...new Uint8Array(buf))).replace(/\+/g,'-').replace(/\//g,'_').replace(/=+$/,'');
  function requestOptions(o) {
    const p = {...o, challenge: unb64(o.challenge)};
    if (o.allowCredentials) p.allowCredentials = o.allowCredentials.map(c => ({...c, id: unb64(c.id)}));
    return p;
  }
  function creationOptions(o) {
    const p = {...o, challenge: unb64(o.challenge), user: {...o.user, id: unb64(o.user.id)}};
    if (o.excludeCredentials) p.excludeCredentials = o.excludeCredentials.map(c => ({...c, id: unb64(c.id)}));
    return p;
  }
  function credentialJSON(c) {
    const r = c.response, out = {id: c.id, rawId: b64(c.rawId), type: c.type, clientExtensionResults: c.getClientExtensionResults ? c.getClientExtensionResults() : {}, response: {clientDataJSON: b64(r.clientDataJSON)}};
    if (c.authenticatorAttachment) out.authenticatorAttachment = c.authenticatorAttachment;
    if (r.attestationObject) {
      out.response.attestationObject = b64(r.attestationObject);
      if (r.getTransports) out.response.transports = r.getTransports();
    } else {
      out.response.authenticatorData = b64(r.authenticatorData);
      out.response.signature = b64(r.signature);
      if (r.userHandle) out.response.userHandle = b64(r.userHandle);
    }
    return out;
  }
  async function stepUp(response, retry) {
    let body;
    try { body = await response.json(); } catch { return false; }
    if (!body || !body.stepup || !body.session || !retry) return false;
    if (!window.PublicKeyCredential || !navigator.credentials) { say('This browser can’t use Face ID or passkeys. Open '+twin()+' from its Home Screen app, or approve on your computer.'); return false; }
    let cred;
    try { cred = await navigator.credentials.get({publicKey: requestOptions(body.stepup)}); }
    catch { say('Face ID didn’t finish, so nothing was approved. Try again.'); return false; }
    if (!cred) return false;
    const headers = new Headers(retry.headers);
    headers.set('AntBot-Stepup', body.session);
    headers.set('Content-Type', 'application/json');
    return nativeFetch(new Request(retry.url, {method: retry.method, headers, body: JSON.stringify(credentialJSON(cred)), credentials: 'same-origin'}));
  }
  async function problemText(r, fallback) {
    try { const j = await r.json(); return [j.message, j.hint || j.fix].filter(Boolean).join(' ') || fallback; } catch { return fallback; }
  }
  // enrolPasskey sets up Face ID (or a passkey) for approving from this
  // device. The twin allows it just after pairing, with a passkey this
  // device already has, or once the owner says so.
  async function enrolPasskey() {
    if (!window.PublicKeyCredential || !navigator.credentials) { say('This browser can’t use Face ID or passkeys. Open '+twin()+' from its Home Screen app and try there.'); return false; }
    const begin = await window.fetch('/stepup/register/begin', {method: 'POST', headers: {Accept: 'application/json'}});
    if (!begin.ok) { say(await problemText(begin, 'Couldn’t set up Face ID. Try again.')); return false; }
    const {publicKey, session} = await begin.json();
    let cred;
    try { cred = await navigator.credentials.create({publicKey: creationOptions(publicKey)}); }
    catch { say('Face ID setup didn’t finish. Try again.'); return false; }
    const finish = await nativeFetch('/stepup/register/finish', {method: 'POST', headers: {'Content-Type': 'application/json', Accept: 'application/json', 'AntBot-Stepup': session}, body: JSON.stringify(credentialJSON(cred))});
    if (!finish.ok) { say(await problemText(finish, 'Couldn’t set up Face ID. Try again.')); return false; }
    say('Face ID is set up. You can approve from this device now.');
    return true;
  }
  window.mirrinPWA = {
    standalone,
    async install() {
      if (installPrompt) { await installPrompt.prompt(); const result=await installPrompt.userChoice; installPrompt=null; return result; }
      say('To install '+twin()+', open the … menu, then choose Add to Home Screen.');
    },
    // stepUp(response, retry) answers a 428: it asks for Face ID or a passkey
    // and sends retry again with the signed check, returning that response.
    // A replacement may return true instead (retry as it was) or false (never
    // retries).
    stepUp,
    enrolPasskey,
    async notifications() {
      if (!('serviceWorker' in navigator) || !('PushManager' in window)) { say('Open the Home Screen app to enable notifications.'); return false; }
      if (await Notification.requestPermission() !== 'granted') { say('Allow notifications in this device’s settings, then try again.'); return false; }
      const reg = await navigator.serviceWorker.ready;
      const response = await nativeFetch('/push/vapid');
      if (!response.ok) { say('Notifications aren’t ready. Try again when your twin is online.'); return false; }
      const {key} = await response.json();
      const bytes = Uint8Array.from(atob(key.replace(/-/g,'+').replace(/_/g,'/')), c=>c.charCodeAt(0));
      const sub = await reg.pushManager.getSubscription() || await reg.pushManager.subscribe({userVisibleOnly:true,applicationServerKey:bytes});
      const result = await nativeFetch('/push/subscribe',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(sub)});
      if (!result.ok) say('Couldn’t enable notifications. Check your connection and try again.');
      return result.ok;
    }
  };
  window.fetch = async (input, init) => {
    const request = new Request(input,init);
    const retry = request.clone();
    const response = await nativeFetch(request);
    const u = new URL(request.url);
    if (u.origin !== location.origin) return response;
    if (u.pathname === '/pair/claim' && response.ok) {
      const result=await response.clone().json();
      if(result.ticket) {ticket(result.ticket);manifest(result.ticket);}
    }
    if (u.pathname === '/screen' && response.ok) saved('antbot-last-seen', String(Date.now()));
    if (response.status === 428 && typeof window.mirrinPWA.stepUp === 'function') {
      const out = await window.mirrinPWA.stepUp(response.clone(), retry.clone());
      if (out && typeof out === 'object' && 'status' in out) return out;
      if (out === true) return nativeFetch(retry);
    }
    return response;
  };
  async function start() {
    if ('serviceWorker' in navigator && !document.title.endsWith(' is offline')) navigator.serviceWorker.register('/sw.js',{scope:'/',updateViaCache:'none'}).catch(()=>say('The app couldn’t update. Check your connection and reopen it.'));
    manifest(ticket());
    if (location.pathname === '/ui') {
      const controls=document.createElement('nav'); controls.setAttribute('aria-label','App settings');
      function button(label,action) {const b=document.createElement('button');b.type='button';b.textContent=label;b.onclick=()=>Promise.resolve(action()).catch(()=>say('Couldn’t finish. Check your connection and try again.'));controls.append(b);}
      if (!standalone()) button('Install '+twin(),()=>window.mirrinPWA.install());
      if ('PushManager' in window && Notification.permission !== 'denied') button('Enable notifications',()=>window.mirrinPWA.notifications());
      if (window.PublicKeyCredential) nativeFetch('/stepup/status',{headers:{Accept:'application/json'}}).then(r=>r.ok?r.json():null).then(s=>{if(s && s.possible && !s.passkey) button('Set up Face ID for approvals',()=>window.mirrinPWA.enrolPasskey());}).catch(()=>{});
      const menu=document.createElement('details');menu.style.position='relative';const label=document.createElement('summary');label.textContent='⋯';label.setAttribute('aria-label','App settings');label.title='Install, notifications, Face ID';menu.append(label,controls);controls.style.cssText='position:absolute;right:0;top:100%;z-index:100;background:#111318;border:1px solid #293040;border-radius:.7rem;padding:.5rem;min-width:12rem;display:grid';(document.querySelector('.meta') || document.body).append(menu);
    }
    if (location.pathname === '/start') {
      const t = new URLSearchParams(location.hash.slice(1)).get('t');
      if (!standalone()) return; // Safari prefetch and ordinary tabs never spend a ticket.
      try {
        const current=await nativeFetch('/pwa/config.json',{cache:'no-store'});
        if (current.ok) {location.replace('/ui');return;}
        if (current.status !== 401) throw new Error('Unavailable');
        const r=await nativeFetch('/pair/ticket',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({t})});
        if(r.ok) {history.replaceState(null,'','/start');location.replace('/ui');return;}
        if (r.status === 409) {say('This app’s pairing needs a fresh start. Remove this Home Screen app, then run mirrin pair --screen on your computer and add it again.');return;}
        say('This pairing ticket has expired or was used. On your computer, run mirrin pair --screen and add the app again.');
      } catch {say('Can’t reach your twin. Check your connection, then reopen this app.');}
      return;
    }
    if (document.title.endsWith(' is offline')) {
      const seen=Number(saved('antbot-last-seen'));
      say('Can’t reach '+twin()+' — last seen '+(seen ? new Date(seen).toLocaleString() : 'not yet on this device')+'.');
    }
    function approval() {
      const id=new URLSearchParams(location.hash.slice(1)).get('approval');
      if(!/^\d+$/.test(id || '')) return;
      const card=document.getElementById('ap'+id);
      if(card) {card.scrollIntoView({block:'center'});card.tabIndex=-1;card.focus();history.replaceState(null,'',location.pathname+location.search);}
    }
    new MutationObserver(approval).observe(document.body,{subtree:true,childList:true});
    addEventListener('hashchange',approval);approval();
  }
  if(document.readyState === 'loading') document.addEventListener('DOMContentLoaded',start); else start();
})();
