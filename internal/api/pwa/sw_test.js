// Socket-free lifecycle checks for the actual embedded worker.
const {readFileSync} = require('node:fs');
const vm = require('node:vm');
const {webcrypto} = require('node:crypto');
const assert = require('node:assert/strict');
const source = readFileSync(process.argv[2], 'utf8');
const bodies = JSON.parse(readFileSync(process.argv[3], 'utf8'));
const all = new Map();
const caches = {
  async keys() { return [...all.keys()]; },
  async delete(k) { return all.delete(k); },
  async open(k) {
    if (!all.has(k)) all.set(k, new Map());
    const data = all.get(k);
    return {async put(p,r) {data.set(p,r.clone());}, async match(p) {return data.get(p)?.clone();}};
  }
};
let offline = false, shown, badge, closed = false, opened;
const posted = [];
function worker(code) {
  const handlers = {};
  const self = {
    location: {origin:'https://twin.test'},
    addEventListener(k,fn) { handlers[k]=fn; },
    async skipWaiting() {},
    clients:{async claim(){},async openWindow(url){opened=url;}},
    navigator:{async setAppBadge(n){badge=n;}},
    registration:{async getNotifications(){return [{close(){closed=true;}}];},async showNotification(title,options){shown={title,...options};}}
  };
  vm.runInNewContext(code,{self,caches,URL,crypto:webcrypto,Uint8Array,fetch:async (path,init)=>{
    if(offline) throw Error('offline');
    if(init&&init.method==='POST'){posted.push({path,body:init.body});return new Response(null,{status:204});}
    const body=bodies[path];return new Response(body ? Buffer.from(body,'base64') : 'missing',{status:body ? 200 : 404});
  }});
  return handlers;
}
async function fire(h,type,data={}) {let waiting=Promise.resolve();h[type]({...data,waitUntil(p){waiting=p;}});await waiting;}
(async()=>{
 const first=worker(source);
 await fire(first,'install');await fire(first,'activate');
 const names=await caches.keys();assert.equal(names.length,1);
 const entries=[...all.get(names[0]).keys()];assert.deepEqual(entries.sort(),Object.keys(bodies).sort());
 for(const path of ['/events','/screen','/screen/shot','/message','/message/stream','/approvals','/approvals/12/approve','/push/test','/pair/ticket']) {
   first.fetch({request:{method:'GET',url:'https://twin.test'+path,mode:'navigate'},respondWith(){throw Error('intercepted private request '+path);}});
 }
 offline=true;
 let response;first.fetch({request:{method:'GET',url:'https://twin.test/ui',mode:'navigate'},respondWith(p){response=p;}});
 assert.match(await (await response).text(),/Can't reach/);
 offline=false;
 // The older shell goes; another app's cache stays.
 all.set('someone-else',new Map());
 const second=worker(source.replace(/mirrin-shell-([a-f0-9]+)/g,'mirrin-shell-next-$1'));
 await fire(second,'install');await fire(second,'activate');
 assert.ok(all.has('someone-else'));all.delete('someone-else');
 assert.equal((await caches.keys()).length,1);assert.ok((await caches.keys())[0].includes('-next-'));
 // A corrupted cache must fail activation before old known-good caches are removed.
 const next=(await caches.keys())[0];all.get(next).set('/offline',new Response('tampered'));
 all.set('mirrin-shell-old-good',new Map());
 await assert.rejects(fire(second,'activate'),/Shell changed/);
 assert.ok(all.has('mirrin-shell-old-good'));
 await fire(first,'push',{data:{json:()=>({v:1,k:'approval',t:'Ava needs you',b:'Review it',tag:'approval-12',badge:3,u:'/approve/12'})}});
 assert.equal(badge,3);assert.equal(shown.tag,'approval-12');assert.equal(shown.actions,undefined);
 await fire(first,'push',{data:{json:()=>({v:1,k:'resolved',t:'Ava: handled',b:'This request no longer needs you.',u:'/ui',tag:'approval-12',badge:2})}});
 assert.equal(badge,2);assert.equal(shown.title,'Ava: handled');assert.equal(shown.tag,'approval-12');assert.equal(shown.silent,true);assert.equal(shown.renotify,false);assert.equal(shown.data.url,'/ui');
 assert.equal(posted.length,0,'an approval push reported itself');
 // A test notification tells the twin it arrived ("Add your phone" lights its last step).
 await fire(first,'push',{data:{json:()=>({v:1,k:'test',t:'Ava',b:'Notifications are ready.',u:'/ui',tag:'test'})}});
 assert.equal(shown.tag,'test');assert.deepEqual(posted,[{path:'/push/received',body:JSON.stringify({tag:'test'})}]);
 await fire(first,'notificationclick',{notification:{data:{url:'/approve/12'},close(){}}});assert.equal(opened,'/approve/12');
 await fire(first,'notificationclick',{notification:{data:{url:'https://evil.test'},close(){}}});assert.equal(opened,'/ui');
 // A hand-over opens the screen with the page up; no other fragment gets through.
 await fire(first,'notificationclick',{notification:{data:{url:'/ui#browser'},close(){}}});assert.equal(opened,'/ui#browser');
 await fire(first,'notificationclick',{notification:{data:{url:'/ui#x'},close(){}}});assert.equal(opened,'/ui');
 await fire(first,'notificationclick',{notification:{data:{url:'https://evil.test/ui#browser'},close(){}}});assert.equal(opened,'/ui');
})().catch(e=>{console.error(e);process.exitCode=1;});
