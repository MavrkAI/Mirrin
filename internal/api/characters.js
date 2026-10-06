// The characters, drawn the same way wherever they appear: the presence
// screen (ui.html) and the welcome page (welcome.html). Code only: each
// drawing is a function of its id prefix, so two of them on one page never
// share a gradient id. The pages' styles pose and animate them.
//
// window.Characters.draw(kind, prefix) is the svg markup of 'penguin',
// 'nyra' or 'maverick'; window.Characters.kindFor(personaID) is the kind a
// persona is drawn as.
(() => {
'use strict';
// The penguin butler (Pickoo and anyone without their own), lit like
// clay: a light from the upper left, shading below, a soft shadow on the
// ice. Its body takes the page's --pb1..3 and the ice --ice1..2; the bow tie
// is its only part in the state's colour (--c1). The hooks every pose uses
// are .pb .turn .front .head .eyes .eye .open .lid .lower .dots .bd, and
// its flippers (.fl) wave hello. p prefixes its ids: 'pg' on the screen,
// 'pw' in the floating widget.
function penguinSVG(p){
  return `<svg viewBox="0 0 200 220" aria-hidden="true" focusable="false"><defs>
<radialGradient id="${p}Body" cx="36%" cy="26%" r="78%"><stop offset="0" style="stop-color:var(--pb1)"/><stop offset=".42" style="stop-color:var(--pb2)"/><stop offset="1" style="stop-color:var(--pb3)"/></radialGradient>
<radialGradient id="${p}Belly" gradientUnits="userSpaceOnUse" cx="84" cy="84" r="118"><stop offset="0" stop-color="#ffffff"/><stop offset=".55" stop-color="#f4f0e8"/><stop offset="1" stop-color="#cfc6b6"/></radialGradient>
<radialGradient id="${p}Beak" cx="38%" cy="30%" r="80%"><stop offset="0" stop-color="#ffd98a"/><stop offset=".6" stop-color="#ffa24a"/><stop offset="1" stop-color="#e06f1e"/></radialGradient>
<radialGradient id="${p}Eye" cx="40%" cy="35%" r="70%"><stop offset="0" stop-color="#262a55"/><stop offset=".75" stop-color="#05060f"/></radialGradient>
<radialGradient id="${p}Cheek" cx="50%" cy="50%" r="50%"><stop offset="0" stop-color="#ff8fb6" stop-opacity=".85"/><stop offset="1" stop-color="#ff8fb6" stop-opacity="0"/></radialGradient>
<radialGradient id="${p}Shine" cx="50%" cy="50%" r="50%"><stop offset="0" stop-color="#fff" stop-opacity=".32"/><stop offset="1" stop-color="#fff" stop-opacity="0"/></radialGradient>
<linearGradient id="${p}Ice" x1="0" y1="0" x2="0" y2="1"><stop offset="0" style="stop-color:var(--ice1)"/><stop offset="1" style="stop-color:var(--ice2)"/></linearGradient>
<radialGradient id="${p}Shadow" cx="50%" cy="50%" r="50%"><stop offset="0" stop-color="#000" stop-opacity=".5"/><stop offset="1" stop-color="#000" stop-opacity="0"/></radialGradient>
<radialGradient id="${p}Bow" cx="40%" cy="35%" r="75%"><stop class="bowc" offset="0" style="stop-color:color-mix(in srgb,var(--c1) 60%,#fff)"/><stop class="bowc" offset=".6" style="stop-color:var(--c1)"/><stop class="bowc" offset="1" style="stop-color:color-mix(in srgb,var(--c1) 55%,#000)"/></radialGradient>
</defs>
<ellipse cx="100" cy="211" rx="80" ry="12.5" fill="url(#${p}Ice)"/><ellipse cx="100" cy="207" rx="50" ry="6.5" fill="url(#${p}Shadow)"/>
<g class="pb"><g class="turn">
<ellipse cx="80" cy="199" rx="16" ry="7" fill="url(#${p}Beak)"/><ellipse cx="120" cy="199" rx="16" ry="7" fill="url(#${p}Beak)"/>
<g class="fl l"><ellipse class="bd" cx="36" cy="132" rx="14" ry="36" transform="rotate(22 36 132)" fill="url(#${p}Body)"/></g>
<g class="fl r"><ellipse class="bd" cx="164" cy="132" rx="14" ry="36" transform="rotate(-22 164 132)" fill="url(#${p}Body)"/></g>
<ellipse class="bd" cx="100" cy="120" rx="64" ry="80" fill="url(#${p}Body)"/>
<g class="front"><ellipse cx="100" cy="140" rx="47" ry="57" fill="url(#${p}Belly)"/><g><path d="M100 152 L79 141 Q73 152 79 163 Z" fill="url(#${p}Bow)"/><path d="M100 152 L121 141 Q127 152 121 163 Z" fill="url(#${p}Bow)"/><circle cx="100" cy="152" r="6.5" fill="url(#${p}Bow)"/></g></g>
<ellipse cx="76" cy="58" rx="30" ry="18" transform="rotate(-24 76 58)" fill="url(#${p}Shine)"/>
<g class="head"><ellipse cx="80" cy="94" rx="27" ry="25" fill="url(#${p}Belly)"/><ellipse cx="120" cy="94" rx="27" ry="25" fill="url(#${p}Belly)"/>
<g class="eyes"><g class="eye"><g class="open"><ellipse cx="81" cy="96" rx="11" ry="13" fill="url(#${p}Eye)"/><circle cx="77.5" cy="91" r="4" fill="#fff"/><circle cx="84.5" cy="100" r="1.8" fill="#fff" opacity=".8"/></g><path class="lid" d="M71 97 Q81 105 91 97" fill="none" stroke="#1a1d3a" stroke-width="3.4" stroke-linecap="round"/></g>
<g class="eye"><g class="open"><ellipse cx="119" cy="96" rx="11" ry="13" fill="url(#${p}Eye)"/><circle cx="115.5" cy="91" r="4" fill="#fff"/><circle cx="122.5" cy="100" r="1.8" fill="#fff" opacity=".8"/></g><path class="lid" d="M109 97 Q119 105 129 97" fill="none" stroke="#1a1d3a" stroke-width="3.4" stroke-linecap="round"/></g></g>
<ellipse cx="69" cy="113" rx="8.5" ry="5" fill="url(#${p}Cheek)"/><ellipse cx="131" cy="113" rx="8.5" ry="5" fill="url(#${p}Cheek)"/>
<path class="lower" d="M92 115 Q100 128 108 115 Q100 119 92 115Z" fill="#d8641b"/>
<path d="M88 112 Q100 103 112 112 Q100 122 88 112Z" fill="url(#${p}Beak)"/></g>
</g></g>
<g class="dots"><circle cx="160" cy="44" r="5" fill="#fff"/><circle cx="174" cy="32" r="6" fill="#fff"/><circle cx="190" cy="18" r="7" fill="#fff"/></g>
</svg>`.replace(/\n/g, '');
}
// Nyra: a warm, composed woman with long dark waves swept over her left
// shoulder, in a navy blazer over a cream high-neck top, drawn in the same
// box with the same hooks as the penguin (.pb .turn .front .head .eyes .eye
// .open .lid .smile .lower .dots .bd), so every pose, the blink and the
// pointer apply. Her back hair stays with the body while her face and fringe
// slide over it, so she turns her head; she has no flippers (.fl), so she
// nods hello instead of waving. The sparkle clip in her hair is her only
// part in the state's colour. p prefixes her ids: 'ny' on the screen, 'nw'
// in the floating widget.
function nyraSVG(p){
  // her markup is kept small (about 9 KB): numbers without a leading 0, a
  // colour's opacity in the colour (#rrggbbaa), path data with no separator
  // before a minus, and fill="none" on the svg, so a line needs no fill and
  // every shape names its own
  const u = n => `url(#${p}${n})`;
  const num = v => String(v).replace(/^(-?)0\./, '$1.'); // 0.5 → .5, never 10.5 → 1.5
  const a = (c, o) => c + Math.round(o * 255).toString(16).padStart(2, '0');
  const D = d => d.replace(/[ ,]-/g, '-');
  // a gradient's stops are [offset, colour]; the first one's offset is 0
  const g = (t, id, at, ...s) => `<${t}Gradient id="${p}${id}"${at ? ' ' + at : ''}>${s.map(([o, c]) => `<stop${o ? ` offset="${num(o)}"` : ''} stop-color="${c}"/>`).join('')}</${t}Gradient>`;
  const fade = (id, c, o) => g('radial', id, '', [0, a(c, o)], [1, a(c, 0)]);
  const sheen = (id, at) => g('linear', id, at, [0, '#d09a7a00'], [.5, '#d09a7acc'], [1, '#d09a7a00']);
  const P = (d, f, x = '') => `<path d="${D(d)}" fill="${f}"${x}/>`;
  const L = (d, c, w, o = 1) => `<path d="${D(d)}" stroke="${o < 1 ? a(c, o) : c}" stroke-width="${num(w)}"/>`;
  // the right side is drawn and the left is its mirror. Paths are "M x,y" then
  // relative "dx,dy" pairs, so mirroring flips the first x and negates every dx.
  const mx = (d, s, i = 0) => s > 0 ? d : d.replace(/-?[\d.]+/g, n => i++ % 2 ? n : i > 1 ? +(-n).toFixed(1) : 200 - n);
  const both = d => d + mx(d, -1);
  const V = 'x2="0" y2="1"', US = 'gradientUnits="userSpaceOnUse"';
  // each eye: the open eye (the crease, the white, the iris that looks where
  // she turns, the lashes), then the closed lid that shows when she blinks
  const eye = s => {
    const m = d => mx(d, s), c = 100 + s * 16.3;
    return `<g class="eye"><g class="open">${P(m('M107.5,90c1.5,-6 6.5,-9.5 12,-9.8c4.5,-.2 8,1.8 9.5,4.3l-3.6,.7c-2.9,-2.6 -6.9,-2.9 -9.4,-2.4c-4,.5 -7,3.2 -8.5,7.2z'), a('#6b3524', .3))}`
      + P(m('M107.5,93.2c1.4,-5.4 6.6,-8 10.9,-7.8c3.8,.2 6.6,2.4 7.6,4.9c-1.4,4.3 -5.2,7.2 -10,7.2c-4.3,0 -7.1,-2 -8.5,-4.3z'), u('White'))
      + `<g class="iris"><circle cx="${c}" cy="91" r="5.8" fill="${u('Iris')}" stroke="#140804" stroke-width=".6"/><circle cx="${c}" cy="91" r="2.5" fill="#0d0504"/><circle cx="${c - 2.2}" cy="88.6" r="1.6" fill="#fff"/></g>`
      + P(m('M107.5,93.2c1.4,-5.4 6.6,-8 10.9,-7.8c3.8,.2 6.6,2.4 7.6,4.9l3.5,-2.7c-2.1,.3 -3.4,-1.1 -4.6,-2c-2.7,-2.2 -6.5,-2.4 -8.9,-2c-3.8,.5 -7.6,3.4 -8.5,9.6z'), '#1a0e0a')
      + L(m('M123,85.2c1.2,-1.2 1.8,-2.3 1.9,-3.4m.8,4.9c1.3,-.9 2.3,-1.8 2.9,-2.9'), '#1a0e0a', .8)
      + `</g>${P(m('M107.3,92.8c4,3.6 13.5,4 18.8,-1.3l3.2,-1.9c-1.9,2.8 -3.8,4.6 -6.1,5.7c-4.3,2.4 -12.3,2 -15.9,-2.5z'), '#1a0e0a', ' class="lid"')}</g>`;
  };
  const lapel = 'M112.5,144.5l-12.5,42.5c8,-8 19,-18 28.5,-31l-8,-4.5l3.5,-6c-3.5,-3 -8,-3 -11.5,-1z';
  return `<svg viewBox="0 0 200 220" aria-hidden="true" focusable="false" fill="none" stroke-linecap="round"><defs>
${g('radial', 'Shell', `${US} cx="70" cy="150" r="110"`, [0, '#4b63ab'], [.45, '#2b3b73'], [1, '#141c3c'])}
${g('linear', 'Lapel', 'x2="1" y2="1"', [0, '#5068b0'], [1, '#1f2b5a'])}
${g('radial', 'Skin', `${US} cx="98" cy="88" r="52" fx="86" fy="72"`, [0, '#eeb88f'], [.55, '#cc8d64'], [1, '#97593a'])}
${g('linear', 'Clump', 'x2=".6" y2="1"', [0, '#5e3c2d'], [.42, '#2c1912'], [1, '#110907'])}
${g('linear', 'Top', V, [0, '#fbf4e6'], [1, '#dccdb3'])}
${g('linear', 'White', V, [0, '#dcc8bd'], [.5, '#fbf5f0'])}
${g('radial', 'Iris', 'cy="68%" r="62%"', [0, '#b67a45'], [.5, '#5e3119'], [1, '#1f0e07'])}
${g('linear', 'Lip', V, [0, '#d27d86'], [1, '#a8505e'])}
${fade('Cheek', '#dd5f68', .28)}${fade('Shine', '#ffffff', .38)}${fade('Shadow', '#000000', .45)}${sheen('SheenH')}${sheen('SheenV', V)}
<radialGradient id="${p}Light" cx="40%" cy="35%" r="75%"><stop class="bowc" style="stop-color:color-mix(in srgb,var(--c1) 40%,#fff)"/><stop class="bowc" offset=".6" style="stop-color:var(--c1)"/><stop class="bowc" offset="1" style="stop-color:color-mix(in srgb,var(--c1) 70%,#000)"/></radialGradient>
<radialGradient id="${p}Halo"><stop class="bowc" style="stop-color:var(--c1);stop-opacity:.7"/><stop class="bowc" offset="1" style="stop-color:var(--c1);stop-opacity:0"/></radialGradient>
<path id="${p}Lp" d="${D(both(lapel))}"/>
<path id="${p}HL" d="${D('M82,30c-14,1 -24,12 -25.5,28c-1,12 1,23 5,30c2,.5 3.5,0 4,-.5c.5,-8.5 1,-16.5 4,-23.5c3.5,-7 8.5,-10 15.5,-11l5,-17z')}"/>
<path id="${p}HR" d="${D('M85,53c11,-3 23,1 31,7c7,5 12,12 14,20c2,10 0,20 -.5,28c-.5,8 1.5,16 6.5,22c6,4 12,2 15,-4c4,-8 4,-20 2,-30c-2,-10 2,-22 0,-34c-3,-22 -15,-38 -35,-41c-14,-1.5 -26,1 -34,7c-1,8 0,18 1,25z')}"/></defs>
<ellipse cx="100" cy="210" rx="62" ry="6.5" fill="${u('Shadow')}"/>
<g class="pb"><g class="turn">
${P('M100,23c26,-4 48,8 54,33c4,14 2,28 5,42c2,14 -3,28 -8,40l-25,14h-50l-20,6c-6,-10 -2,-20 -5,-32c-3,-12 4,-22 1,-34c-3,-12 0,-30 6,-46c8,-14 24,-22 42,-22z', u('Clump'), ' class="bd"')}
${P('M55,96c-4,10 -2.5,20 -4.5,31c3,-10 2.5,-21 4.5,-31z', u('SheenV'))}
${P('M91.5,112c.5,12 -.5,20 -1,29h19c-.5,-9 -1.5,-17 -1,-29z', u('Skin'))}
${P('M100,137c-12,0 -20,4 -28,9c-8,4 -20,6 -27,13c-7,7 -9,19 -9,31v6c0,9 26,14 64,14c38,0 64,-5 64,-14v-6c0,-12 -2,-24 -9,-31c-7,-7 -19,-9 -27,-13c-8,-5 -16,-9 -28,-9z', u('Shell'), ' class="bd"')}
<ellipse cx="62" cy="155" rx="14" ry="5" transform="rotate(-22 62 155)" fill="${u('Shine')}"/>
<g class="front">${P('M111,146l-11,40l-11,-40c4,2.5 18,2.5 22,0zm-24,-10.5c7,3.5 19,3.5 26,0l1.5,11.5c-6.5,3.5 -22.5,3.5 -29,0z', u('Top'))}${L('M86.4,141.4c6.6,3.2 20.6,3.2 27.2,0', '#d3c2a5', .8)}
<use href="#${p}Lp" x="1.4" y="1.4" fill="${a('#0b1029', .55)}"/><use href="#${p}Lp" fill="${u('Lapel')}"/>${L(both('M100,187c8,-8 19,-18 28.5,-31l-8,-4.5'), '#ffffff', .8, .18)}
${P('M126,124c12,-4 30,2 31,18c1,12 -9,16 -8,26c1,10 11,16 7,28c-3,8 -14,12 -24,8c6,-3 9,-8 7,-14c-3,-8 -13,-12 -13,-22c0,-10 10,-16 8,-26c-1,-8 -8,-12 -12,-14z', u('Clump'))}
${P('M149,129c6.5,7 7,18 2,26c2,-9 2,-18 -2,-26zm-17.5,28c-5,7 -5,15 -1,22c-1.9,-7 -1.9,-15 1,-22zm19,22c5.5,7 5.5,15 1.5,20c1.5,-6 1.4,-13 -1.5,-20z', u('SheenV'))}</g>
<g class="head">${P('M65.5,86c-4.5,-2 -6.5,4 -5.9,10c.6,6 2.9,10.5 6.9,10z', u('Skin'))}<circle cx="62.8" cy="105" r="1.5" fill="#f4ede6"/>
${P('M100,46c21,0 35,16 35,38c0,16 -5,28 -14,38c-7,7 -14,12 -21,12c-7,0 -14,-5 -21,-12c-9,-10 -14,-22 -14,-38c0,-22 14,-38 35,-38z', u('Skin'))}
<g fill="${u('Cheek')}"><ellipse cx="79" cy="107" rx="9" ry="5"/><ellipse cx="121" cy="107" rx="9" ry="5"/></g><g fill="${u('Shine')}"><ellipse cx="78" cy="98" rx="6" ry="3"/><ellipse cx="99.4" cy="105.2" rx="2.8" ry="2.2"/></g>
<g class="eyes">${P(both('M106.4,79.4c3.6,-3.8 9.2,-6.4 14.6,-6.6c3.8,-.1 7,1.4 9.2,4.1c-3.2,-1.3 -6.4,-1.9 -9.6,-1.7c-5,.4 -9.8,2.4 -13.4,5.8z'), '#2b1912')}${eye(-1)}${eye(1)}</g>
${L('M96.4,108.2c1.4,2 5.8,2 7.2,0', '#7a4029', 1.2, .6)}
${P('M91.2,118.4c3.6,2.1 6.3,2.9 8.8,2.9c2.5,0 5.2,-.8 8.8,-2.9c-1.2,4.2 -4.6,6.6 -8.8,6.6c-4.2,0 -7.6,-2.4 -8.8,-6.6z', u('Lip'))}
<g class="lower">${P('M91.8,119.6c3.2,.7 5.8,1 8.2,1c2.4,0 5,-.3 8.2,-1c-1,5.2 -4,8 -8.2,8c-4.2,0 -7.2,-2.8 -8.2,-8z', '#3a0f17')}${P('M95,119.8c1.6,.6 3.4,.8 5,.8c1.6,0 3.4,-.2 5,-.8l-.4,1.9c-1.4,.6 -3,.9 -4.6,.9c-1.6,0 -3.2,-.3 -4.6,-.9z', '#f6efe8')}${P('M91.8,119.6c1.2,4.4 4.2,6.8 8.2,6.8c4,0 7,-2.4 8.2,-6.8c-.8,6.2 -4.2,10 -8.2,10c-4,0 -7.4,-3.8 -8.2,-10z', u('Lip'))}</g>
${P('M90.6,117.8c3,-1.2 5.4,-2.3 7.4,-2.2c1,.1 1.6,.8 2,.9c.4,-.1 1,-.8 2,-.9c2,-.1 4.4,1 7.4,2.2c-4,2.5 -6.8,3.5 -9.4,3.5c-2.6,0 -5.4,-1 -9.4,-3.5z', '#a8505e')}
<path class="smile" d="${D('M89.2,116.6c.2,.7 .5,1.2 .8,1.5c4,2.7 7,3.3 10,3.3c3,0 6,-.8 10.2,-3.3c.3,-.3 .6,-1 .8,-2.1')}" stroke="#6e2a35" stroke-width="1.1"/>
<g fill="${a('#4a2214', .3)}" transform="translate(.8 2.4)"><use href="#${p}HL"/><use href="#${p}HR"/></g><g fill="${u('Clump')}"><use href="#${p}HL"/><use href="#${p}HR"/><path d="${D('M108,55c11,3 19,10 22,21c1.5,6 1,12 0,17c-1.5,-9 -5,-18 -11,-24.5c-4,-4 -8,-7.5 -11,-13.5z')}"/></g>
${L('M87,36c15,-6 41,0 58,24m-57,-16c16,-4 36,2 50,22c4,8 3,20 3,30m-61,-56c-8,6 -12,16 -13,24', '#070302', .8, .45)}
${P('M90,32c15,-8.5 39,-6.5 56,12c-17,-14 -39,-15.5 -56,-12z', u('SheenH'))}${P('M78,33.5c-11.5,5.5 -17,16.5 -17.7,29.5c2.5,-12 8.3,-21.4 17.7,-29.5zm69,38.5c3.5,12 2.5,25 -1.5,38c1.5,-13 1.9,-26 1.5,-38z', u('SheenV'))}
<circle cx="66" cy="55" r="14" fill="${u('Halo')}"/>${P('M66,47q1,7 8,8q-7,1 -8,8q-1,-7 -8,-8q7,-1 8,-8zm7.5,-1.5q.4,2.6 3,3q-2.6,.4 -3,3q-.4,-2.6 -3,-3q2.6,-.4 3,-3z', u('Light'))}</g>
</g></g>
<g class="dots" fill="#fff"><circle cx="166" cy="46" r="5"/><circle cx="178" cy="33" r="6"/><circle cx="192" cy="19" r="7"/></g></svg>`.replace(/\n/g, '');
}
// Mirrin: a man with swept-back dark hair, short faded sides and a
// neat short beard, in a black dinner jacket with satin peak lapels, a white
// dress shirt with studs, a black bow tie and a white pocket square. Nyra's
// counterpart, drawn the same way on the same hooks (.pb .turn .front .head
// .eyes .eye .open .iris .lid .smile .lower .dots .bd), so every pose, the
// blink and the pointer apply. The hair at the back of his head stays with
// the body while his face, ears and hair slide over it, so he turns his head;
// he has no flippers (.fl), so he nods hello. A knowing half-smile: the left
// corner of his mouth (the viewer's right) lifts and that brow sits a touch
// higher. The pin on his lapel, Nyra's sparkle, is his only part in the
// state's colour. p prefixes his ids: 'mg' on the screen, 'mw' in the widget.
function maverickSVG(p){
  // kept small (about 9 KB), as Nyra is: numbers without a leading 0, a
  // colour's opacity in the colour, path data with no separator before a
  // minus, whole numbers where half a unit can't be seen
  const u = n => `url(#${p}${n})`;
  const num = v => String(v).replace(/^(-?)0\./, '$1.');
  const a = (c, o) => c + Math.round(o * 255).toString(16).padStart(2, '0');
  const D = d => d.replace(/[ ,]-/g, '-').replace(/([^\d.])0\./g, '$1.');
  const g = (t, id, at, ...s) => `<${t}Gradient id="${p}${id}"${at ? ' ' + at : ''}>${s.map(([o, c]) => `<stop${o ? ` offset="${num(o)}"` : ''} stop-color="${c}"/>`).join('')}</${t}Gradient>`;
  const fade = (id, c, o) => g('radial', id, '', [0, a(c, o)], [1, a(c, 0)]);
  const sheen = (id, at, c = '#a87a5e') => g('linear', id, at, [0, a(c, 0)], [.5, a(c, .8)], [1, a(c, 0)]);
  const P = (d, f, x = '') => `<path d="${D(d)}" fill="${f}"${x}/>`;
  const L = (d, c, w, o = 1) => `<path d="${D(d)}" stroke="${o < 1 ? a(c, o) : c}" stroke-width="${num(w)}"/>`;
  const mx = (d, s, i = 0) => s > 0 ? d : d.replace(/-?[\d.]+/g, n => i++ % 2 ? n : i > 1 ? +(-n).toFixed(1) : +(200 - n).toFixed(1));
  // the right side is drawn and the left is its mirror (relative paths: the
  // first x flips about 100 and every dx turns round; no h or v in them)
  const both = d => d + mx(d, -1);
  const V = 'x2="0" y2="1"', US = 'gradientUnits="userSpaceOnUse"';
  // each eye: the white, the iris (it looks where he turns), skin below the
  // white so the iris never shows under it, the crease, the upper lid line;
  // then the closed lid that shows when he blinks
  const eye = s => {
    const m = d => mx(d, s), c = 100 + s * 17;
    return `<g class="eye"><g class="open">${P(m('M107.8,92.6c2.4,-5 14.6,-7 19,-1.8c-3,5.4 -15.4,6.6 -19,1.8z'), u('W'))}`
      + `<g class="iris"><circle cx="${c}" cy="91.4" r="5.2" fill="${u('I')}" stroke="#140804" stroke-width=".6"/><circle cx="${c}" cy="91.4" r="2.3" fill="#0d0504"/><circle cx="${c - 2}" cy="90" r="1.3" fill="#fff"/></g>`
      + P(m('M107.8,92.6c3.6,4.8 16,3.6 19,-1.8l.6,2.2c-3.4,6 -17,7 -20.2,1.2z'), u('S'))
      + P(m('M107.4,89.6c2.4,-8 16.6,-9.4 20.2,-2.2l-1.2,.8c-4,-6 -14.6,-5.4 -19,1.4z'), a('#6b3524', .3))
      + P(m('M107.6,93c2.4,-5.4 14.8,-7.4 19.4,-2l.8,-.2c-4,-7 -17.8,-7 -20.2,2.2z'), '#24140e')
      + `</g>${P(m('M107.6,92.6c4.2,2.8 13.6,3 19.4,-1.6l.6,.8c-5.4,4.8 -15.4,4.6 -20,.8z'), '#24140e', ' class="lid"')}</g>`;
  };
  // his brows; the left one (the viewer's right) is drawn 1.6 higher
  const brow = 'M106,81.2l-.2,-2.2c4.2,-2.2 8.6,-3.4 12.8,-3.4c4.4,0 8,1 10.8,2.8c-3.2,-.8 -6.6,-1 -10.4,-.8c-4.4,.2 -8.8,1.4 -13,3.6z';
  const lapel = 'M113,150l-12,48c10,-11 22,-26 31,-40l6,-7l-9,4l-9,-14c-2,2 -5,5 -7,8z';
  // the beard, whole round the mouth and up to the sideburns, and the hair:
  // the swept-back top (hairOut is its outside, for the rim in the dark), and
  // the short sides under it
  const beard = 'M100,113c5,1 10,1 15,2c3,0 6,-1 8,-2c4,-2 7,-4 9,-8c0,-3 1,-10 1,-16c1,0 1,0 2,0c0,7 -1,17 -5,27c-4,8 -11,16 -20,20c-3,1 -6,2 -10,2c-4,0 -7,-1 -10,-2c-9,-4 -16,-12 -20,-20c-4,-10 -5,-20 -5,-27c1,0 1,0 2,0c0,6 1,13 1,16c2,4 5,6 9,8c2,1 5,2 8,2c5,-1 10,-1 15,-2z';
  const hairOut = 'M64,64c-3,-9 -3,-19 2,-27c7,-10 19,-15 34,-15c16,0 28,6 34,15c5,8 5,18 1,27';
  const hair = hairOut + 'c-1.6,-2.6 -3.8,-4.6 -6.2,-5.4c-2.2,-.6 -4.8,-.6 -7.2,-.8c-2.4,-1.6 -5,-2.8 -8.4,-3.5c-4.6,-.9 -9.4,-1.3 -14,-1.3c-5,0 -9.6,.4 -14,1.3c-3.4,.7 -6,1.9 -8.4,3.5c-2.4,.2 -5,.2 -7.2,.8c-2.6,.8 -4.6,2.8 -6,5.8z';
  const side = 'M129,58c3,4 4,10 4,17c1,7 0,13 0,19c1.4,.8 2.6,.8 3.4,0c.4,-8 .6,-18 0,-28c-.4,-8 -1.4,-15 -3.4,-20zm-57,0c-3,4 -4,10 -5,17c0,7 1,13 1,19c-1.4,.8 -2.6,.8 -3.4,0c-.6,-8 -.6,-18 -.2,-28c.4,-8 1.6,-15 3.6,-20z';
  return `<svg viewBox="0 0 200 220" aria-hidden="true" focusable="false" fill="none" stroke-linecap="round"><defs>
${g('radial', 'Suit', `${US} cx="70" cy="150" r="120"`, [0, '#4c5262'], [.45, '#262a34'], [1, '#0b0c10'])}
${g('linear', 'La', 'x2="1" y2="1"', [0, '#5c6272'], [.45, '#22252e'], [1, '#08090c'])}
${g('radial', 'S', `${US} cx="98" cy="88" r="54" fx="86" fy="72"`, [0, '#f2c4a0'], [.55, '#d09670'], [1, '#975c3f'])}
${g('linear', 'H', 'x2=".6" y2="1"', [0, '#664431'], [.45, '#2c1b12'], [1, '#110906'])}
${g('radial', 'B', `${US} cx="100" cy="100" r="40"`, [.3, a('#3a2418', .32)], [.8, a('#24150e', .6)], [1, a('#1e110b', .72)])}
${g('linear', 'D', `${US} x1="0" y1="52" x2="0" y2="94"`, [.2, '#24150e'], [1, '#5e4030'])}
${g('linear', 'T', V, [0, '#ffffff'], [1, '#d8d6d0'])}
${g('linear', 'W', V, [0, '#d8c6bc'], [.5, '#fbf6f2'])}
${g('radial', 'I', 'cy="68%" r="62%"', [0, '#a77446'], [.5, '#55301a'], [1, '#1c0d06'])}
${g('linear', 'Li', V, [0, '#b6766c'], [1, '#8a4c46'])}
${fade('Sh', '#ffffff', .36)}${fade('F', '#000000', .45)}${sheen('Y', V)}
<radialGradient id="${p}Lt" cx="40%" cy="35%" r="75%"><stop class="bowc" style="stop-color:color-mix(in srgb,var(--c1) 40%,#fff)"/><stop class="bowc" offset=".6" style="stop-color:var(--c1)"/><stop class="bowc" offset="1" style="stop-color:color-mix(in srgb,var(--c1) 70%,#000)"/></radialGradient>
<radialGradient id="${p}Hl"><stop class="bowc" style="stop-color:var(--c1);stop-opacity:.7"/><stop class="bowc" offset="1" style="stop-color:var(--c1);stop-opacity:0"/></radialGradient>
<path id="${p}Lp" d="${D(both(lapel))}"/>
<path id="${p}Hr" d="${D(hair)}"/></defs>
<ellipse cx="100" cy="210" rx="66" ry="6.5" fill="${u('F')}"/>
<g class="pb"><g class="turn">
${P('M100,34c20,0 34,12 36,30c1,10 0,20 -2,28c-6,4 -20,6 -34,6c-14,0 -28,-2 -34,-6c-2,-8 -3,-18 -2,-28c1,-18 16,-30 36,-30z', u('H'), ' class="bd"')}
${P('M87,110c1,12 0,24 -1,36h27c-1,-12 -1,-24 -1,-36z', u('S'))}
${P('M100,140c6,0 12,-2 17,-5c5,1 10,3 14,6c8,6 25,8 35,17c7,7 9,20 9,34v2c0,10 -30,16 -75,16c-45,0 -75,-6 -75,-16v-2c0,-14 2,-27 9,-34c10,-9 27,-11 35,-17c4,-3 9,-5 14,-6c5,3 11,5 17,5z', u('Suit'), ' class="bd"')}
<ellipse cx="58" cy="158" rx="15" ry="5" transform="rotate(-22 58 158)" fill="${u('Sh')}"/>
<g class="front">${P('M84,138h32l-16,64z', u('T'))}
${P(both('M112,131c2,1 4,2 5,5l1,6l-6,11l-12,-4c4,-4 8,-9 10,-15z'), u('T'))}
<g fill="#16181e"><circle cx="100" cy="166" r="1.3"/><circle cx="100" cy="177" r="1.3"/><circle cx="100" cy="188" r="1.3"/></g>
<use href="#${p}Lp" x="1.4" y="1.4" fill="${a('#000000', .45)}"/><use href="#${p}Lp" fill="${u('La')}"/>
${P(both('M102,146.4l8.8,-3.2c1,-.2 1.6,.6 1.6,2l0,6.4c0,1.4 -.6,2.2 -1.6,2l-8.8,-3.2z'), u('La'))}
<rect x="97" y="146" width="6" height="4.8" rx="1.2" fill="#1c1e25"/>
${P('M134.2,178.6l11.4,-1.2l.3,2.2l-11.4,1.2z', u('T'))}${P('M134.6,178.4l2.8,-3.8l2,2.8l3.2,-4.2l2.8,3.8z', u('T'))}
<circle cx="121" cy="164" r="11" fill="${u('Hl')}"/><g transform="matrix(1.18 0 0 1.18 -21.8 -29.5)">${P('M121,158.5q.7,4.8 5.5,5.5q-4.8,.7 -5.5,5.5q-.7,-4.8 -5.5,-5.5q4.8,-.7 5.5,-5.5z', u('Lt'), ' stroke="#ffffff73" stroke-width=".5"')}</g></g>
<g class="head">${P(both('M135,85c5,-2 8,4 7,11c-1,7 -3,12 -8,12z'), u('S'))}
${P('M100,45.6c21.6,0 35.8,14.8 35.8,36.4c0,12 -1.4,23.6 -5,33c-3.8,9.2 -12,16.8 -20.8,20.2c-3.2,1.2 -6.6,1.8 -10,1.8c-3.4,0 -6.8,-.6 -10,-1.8c-8.8,-3.4 -17,-11 -20.8,-20.2c-3.6,-9.4 -5,-21 -5,-33c0,-21.6 14.2,-36.4 35.8,-36.4z', u('S'))}
<g fill="${u('Sh')}"><ellipse cx="78" cy="96" rx="6" ry="3"/><ellipse cx="99.4" cy="104" rx="2.6" ry="2"/></g>
${P(beard, u('B'))}
${P('M102.6,86c1.2,5 2.2,10.4 4,15.6c-.6,1.6 -1.8,2.4 -3,2.6c.4,-6 .2,-12 -1,-18.2z', a('#8a4a2e', .16))}${L('M93.8,106c.2,2 1.8,3.4 3.6,3.4c1,.9 4.2,.9 5.2,0c1.8,0 3.4,-1.4 3.6,-3.4', '#7a4029', 1.2, .55)}
${P('M91.4,120.6c2.8,.6 5.8,.9 8.8,.7c3.6,-.2 6.8,-1.2 9.6,-3c-.8,3.6 -4.2,5.8 -9,6c-4.6,.2 -8,-1.4 -9.4,-3.7z', u('Li'))}
<g class="lower">${P('M91.4,120.6c2.8,.6 5.8,.9 8.8,.7c3.6,-.2 6.8,-1.2 9.6,-3c-.8,5.6 -4.2,8.8 -9,9c-4.6,.2 -8,-2.6 -9.4,-6.7z', '#3a1414')}${P('M94,121.1c4,.9 9,.7 13.2,-1.3l-.4,1.8c-4,1.8 -8.6,2.1 -12.4,1.1z', '#f3ece4')}${P('M91.4,120.6c1,4.2 4.4,6.8 9,6.6c4.4,-.2 7.6,-3.2 9.4,-8.9c-.4,6.6 -4,10.8 -9.2,11c-4.8,.2 -8.4,-3.4 -9.2,-8.7z', u('Li'))}</g>
${P('M90,120.4c3.4,-1.6 7,-2.6 10,-2.6c3.4,0 7,-.6 11.6,-2.2c-3,2.8 -6.8,4.4 -11,4.8c-3.6,.3 -7,.3 -10.6,0z', '#a0605a')}
${P('M100,112.6c2.8,-.8 6,-1.4 8.8,-1.2c3,.2 5.2,1.6 6.6,3.8c-2.8,-.4 -5.6,-.2 -8.2,.2c-2.6,.4 -5,1 -7.2,1.6c-2.2,-.6 -4.6,-1 -7,-1c-2.4,0 -4.8,.6 -6.8,1.6c1,-3 3,-5 5.6,-5.6c2.6,-.6 5.4,-.2 8.2,.6z', a('#22140d', .85))}
<path class="smile" d="${D('M89.8,119.6c3.6,1.2 7.2,1.6 10.4,1.5c3.8,-.3 7.2,-1.3 10,-3.2c.8,-.7 1.5,-1.8 2,-3.2')}" stroke="#5e2a26" stroke-width="1.1"/>
<g class="eyes">${P(mx(brow, -1) + brow.replace('M106,81.2', 'M106,79.6'), '#2a1810')}${eye(-1)}${eye(1)}</g>
<use href="#${p}Hr" y="2.2" fill="${a('#4a2214', .3)}"/><path class="bd" d="${D('M64,94c-1,-8 -1,-18 -1,-28' + hairOut + 'M137,66c0,10 0,20 -1,28')}"/>${P(side, u('D'))}<use href="#${p}Hr" fill="${u('H')}"/>
${L('M122,57c-2,-7 -2,-15 0,-24m-12,20c-4,-7 -9,-15 -17,-22m3,21c-6,-5 -12,-10 -19,-13m49,19c3,-6 7,-10 11,-12', '#070302', .8, .4)}
${P('M104,52c-5,-8 -11,-14 -20,-19c9,3 16,9 22,19zm-14,2c-5,-5 -11,-8 -18,-9c8,0 14,3 20,8z', u('Y'))}
${P('M72,42c10,-11 32,-15 52,-9c-20,-3 -38,1 -52,9z', u('Y'))}</g>
</g></g>
<g class="dots" fill="#fff"><circle cx="166" cy="46" r="5"/><circle cx="178" cy="33" r="6"/><circle cx="192" cy="19" r="7"/></g></svg>`.replace(/\n/g, '');
}
// Which character a persona gets: Nyra and Mirrin (his drawing is
// 'maverick', and his id was mavrk before he was Mirrin) their own;
// everyone else (Pickoo, a pack's) the penguin. A pack's persona is
// "pack/id". A stored kind ('maverick', 'nyra') maps to itself, so the
// screen's last character (localStorage) comes back as itself. The retired
// persona plain, and the rover it was drawn as, are Mirrin now, as the
// twin resolves them.
const kindFor = id => {
  const k = String(id || '').split('/').pop().toLowerCase();
  return k === 'nyra' ? 'nyra' : ['mirrin', 'mavrk', 'maverick', 'plain', 'robot'].includes(k) ? 'maverick' : 'penguin';
};
const DRAW = {penguin: penguinSVG, nyra: nyraSVG, maverick: maverickSVG};
window.Characters = Object.freeze({
  draw: (kind, prefix) => (DRAW[kind] || penguinSVG)(String(prefix || '')),
  kindFor,
});
})();
