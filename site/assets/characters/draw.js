// Draws the site's characters with the presence screen's own code,
// internal/api/characters.js, so the site shows exactly what the screen does.
// Run from the repository root after that file changes:
//
//   node site/assets/characters/draw.js
//
// It writes the still portraits here (mirrin.svg, nyra.svg and
// penguin.svg: resting, in the screen's idle colour and dark-theme body
// colours, eyes open; Mirrin's and Nyra's mouths closed, the penguin's
// just open) and puts Mirrin's drawing into index.html between the
// mirrin:start and mirrin:end markers, where site.css animates it. The
// characters' kinds keep the code's own ids; only the file names here use
// the personas' names. The home page's script draws the portraits into the
// page so they can react; site.css repeats their <style> as .cv, so change
// both together. Not linked from the site.
'use strict';
const fs = require('fs');
const path = require('path');

const here = __dirname;
const src = fs.readFileSync(path.join(here, '../../../internal/api/characters.js'), 'utf8');
const window = {};
new Function('window', src)(window);
const C = window.Characters;

// ui.html: .idle{--c1:#6d7cff}, and the dark theme's --pb1..3, --ice1..2 and --rim.
const vars = '--c1:#6d7cff;--pb1:#7f8ad9;--pb2:#3b4590;--pb3:#262d66;--ice1:#34406c;--ice2:rgba(20,26,48,0)';
const style = person => `<style>svg{${vars}}.lid{opacity:0}.dots{display:none}` +
  `.bd{stroke:rgba(165,180,255,.22);stroke-width:1.5}` +
  `.lower{transform-box:fill-box;transform-origin:50% 0;transform:scaleY(${person ? 0 : 0.25})}</style>`;
const still = (kind, prefix, person) => C.draw(kind, prefix)
  .replace('<svg ', '<svg xmlns="http://www.w3.org/2000/svg" ')
  .replace(' aria-hidden="true" focusable="false"', '')
  .replace('<defs>', style(person) + '<defs>') + '\n';

// Nyra has her own drawing; the penguin would mean characters.js lost it.
const nyra = C.kindFor('nyra');
if (!nyra || nyra === 'penguin') throw new Error("characters.js no longer draws Nyra's own character");
const kinds = {mirrin: C.kindFor('mirrin'), nyra, penguin: C.kindFor('pickoo')};
// Mirrin (persona mirrin) has his, the man in the dinner jacket; likewise.
if (!kinds.mirrin || kinds.mirrin === 'penguin') throw new Error("characters.js no longer draws Mirrin's own character");
fs.writeFileSync(path.join(here, 'mirrin.svg'), still(kinds.mirrin, 'm', true));
fs.writeFileSync(path.join(here, 'nyra.svg'), still(kinds.nyra, 'n', true));
fs.writeFileSync(path.join(here, 'penguin.svg'), still(kinds.penguin, 'p', false));

const index = path.join(here, '../../index.html');
const page = fs.readFileSync(index, 'utf8');
const re = /(<!-- mirrin:start -->)[\s\S]*?(<!-- mirrin:end -->)/;
if (!re.test(page)) throw new Error('index.html has no mirrin:start and mirrin:end markers');
fs.writeFileSync(index, page.replace(re, (_, a, b) => a + C.draw(kinds.mirrin, 'hm') + b));
console.log('drew mirrin.svg, nyra.svg, penguin.svg and the hero in index.html');
