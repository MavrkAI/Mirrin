const {readFileSync}=require('node:fs');
const vm=require('node:vm');
const assert=require('node:assert/strict');
const source=readFileSync(process.argv[2],'utf8');
function client(path='/ui',standalone=false, navigator={}) {
 const calls=[],events={},storage=new Map(),link={},status={};
 let respond=()=>new Response('{}',{status:200});
 const location={origin:'https://twin.test',pathname:path,hash:'#t=it_abcdefghijklmnopqrstuvwx',replace(path){this.replaced=path;}};
 const document={readyState:'loading',addEventListener(k,f){events[k]=f;},querySelector(q){return q.includes('manifest')?link:{content:'Ava'};},getElementById(){return status;}};
 const window={fetch:async request=>{calls.push({url:request.url||request,method:request.method||'GET',body:request.body ? await request.clone().text() : '',headers:request.headers ? Object.fromEntries(request.headers) : {}});return respond(request);}};
 class BrowserRequest extends Request {constructor(input,init){super(typeof input==='string'?new URL(input,location.origin):input,init);}}
 vm.runInNewContext(source,{window,document,location,history:{replaceState(){}},navigator,matchMedia:()=>({matches:standalone}),addEventListener(k,f){events[k]=f;},Request:BrowserRequest,Headers,atob,btoa,URL,URLSearchParams,Uint8Array,localStorage:{setItem(k,v){storage.set(k,v);},getItem(k){return storage.get(k);}},sessionStorage:{setItem(k,v){storage.set(k,v);},getItem(k){return storage.get(k);}}});
 return {window,calls,events,storage,link,status,location,setRespond(f){respond=f;}};
}
(async()=>{
 const c=client();let attempts=0,stepups=0;
 c.setRespond(()=>new Response('{}',{status:++attempts===1?428:200}));
 c.window.mirrinPWA.stepUp=async()=>{stepups++;return true;};
 const response=await c.window.fetch('/approvals/12/approve',{method:'POST',headers:{'Content-Type':'application/json'},body:'{"check":12}'});
 assert.equal(response.status,200);assert.equal(stepups,1);assert.equal(c.calls.length,2);assert.equal(c.calls[0].body,c.calls[1].body);assert.equal(c.calls[1].method,'POST');
 c.setRespond(()=>new Response('{}',{status:428}));c.window.mirrinPWA.stepUp=async()=>false;
 const before=c.calls.length;assert.equal((await c.window.fetch('/approvals/12/approve',{method:'POST'})).status,428);assert.equal(c.calls.length,before+1);
 c.window.mirrinPWA.stepUp=async()=>{throw Error('foreign step-up');};await c.window.fetch('https://elsewhere.test/x');
 c.setRespond(()=>Response.json({ticket:'it_abcdefghijklmnopqrstuvwx'}));await c.window.fetch('/pair/claim',{method:'POST'});assert.equal(c.link.href,undefined);
 for (const nav of [{userAgent:'iPhone Version/26 Safari'}, {userAgent:'Macintosh Version/26 Safari',platform:'MacIntel',maxTouchPoints:5}]) {
 const ios=client('/pair',false,nav);ios.setRespond(()=>Response.json({ticket:'it_abcdefghijklmnopqrstuvwx'}));await ios.window.fetch('/pair/claim',{method:'POST'});assert.equal(ios.link.href,'/manifest.webmanifest?t=it_abcdefghijklmnopqrstuvwx');
 }
 for (const userAgent of ['Android Chrome Safari','Windows Chrome Safari','Macintosh Version/26 Safari','iPhone CriOS Safari']) {const other=client('/pair',false,{userAgent});other.setRespond(()=>Response.json({ticket:'it_abcdefghijklmnopqrstuvwx'}));await other.window.fetch('/pair/claim',{method:'POST'});assert.equal(other.link.href,undefined);}
 const tab=client('/start',false);await tab.events.DOMContentLoaded();assert.equal(tab.calls.length,0,'Safari prefetch redeemed');
 const paired=client('/start',true);await paired.events.DOMContentLoaded();assert.equal(paired.calls.length,1);assert.equal(paired.calls[0].url,'/pwa/config.json');assert.equal(paired.location.replaced,'/ui');
 const fresh=client('/start',true);fresh.setRespond(request=>new Response('{}',{status:request==='/pwa/config.json'?401:200}));await fresh.events.DOMContentLoaded();assert.equal(fresh.calls.length,2);assert.equal(fresh.calls[1].url,'/pair/ticket');assert.equal(fresh.location.replaced,'/ui');
 for (const code of [409,410]) {
 const stale=client('/start',true);stale.setRespond(request=>new Response('{}',{status:request==='/pwa/config.json'?401:code}));await stale.events.DOMContentLoaded();
 assert.match(stale.status.textContent,code===409 ? /Remove this Home Screen app/ : /expired or was used/);
 }
 // The built-in step-up: a 428 asks the passkey for the check it carries and
 // sends the same request again with the session and the signed check.
 let asked;
 const bytes=a=>new Uint8Array(a).buffer;
 const phone=client('/ui',false,{credentials:{get:async({publicKey})=>{asked=publicKey;return {id:'AQID',rawId:bytes([1,2,3]),type:'public-key',authenticatorAttachment:'platform',response:{clientDataJSON:bytes([123,125]),authenticatorData:bytes([9]),signature:bytes([8]),userHandle:bytes([7])},getClientExtensionResults(){return {};}};}}});
 phone.window.PublicKeyCredential=function(){};
 let turn=0;phone.setRespond(()=>++turn===1?Response.json({stepup:{challenge:'AQID',rpId:'twin.test',allowCredentials:[{type:'public-key',id:'BAUG'}],userVerification:'required'},session:'su_abc'},{status:428}):Response.json({reply:'Booked.'}));
 const decided=await phone.window.fetch('/approvals/12/approve',{method:'POST',headers:{Accept:'application/json'}});
 assert.equal(decided.status,200);assert.equal((await decided.json()).reply,'Booked.');
 assert.deepEqual([...asked.challenge],[1,2,3]);assert.deepEqual([...asked.allowCredentials[0].id],[4,5,6]);assert.equal(asked.userVerification,'required');
 assert.equal(phone.calls.length,2);const again=phone.calls[1];
 assert.equal(again.method,'POST');assert.equal(again.url,'https://twin.test/approvals/12/approve');assert.equal(again.headers['mirrin-stepup'],'su_abc');assert.equal(again.headers.accept,'application/json');
 assert.deepEqual(JSON.parse(again.body),{id:'AQID',rawId:'AQID',type:'public-key',authenticatorAttachment:'platform',clientExtensionResults:{},response:{clientDataJSON:'e30',authenticatorData:'CQ',signature:'CA',userHandle:'Bw'}});
 // Face ID cancelled: nothing is sent again.

 const cancelled=client('/ui',false,{credentials:{get:async()=>{throw Error('NotAllowedError');}}});cancelled.window.PublicKeyCredential=function(){};
 cancelled.setRespond(()=>Response.json({stepup:{challenge:'AQID'},session:'su_abc'},{status:428}));
 assert.equal((await cancelled.window.fetch('/approvals/12/approve',{method:'POST'})).status,428);assert.equal(cancelled.calls.length,1);
})().catch(e=>{console.error(e);process.exitCode=1;});
