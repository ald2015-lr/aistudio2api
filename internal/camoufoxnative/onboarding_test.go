package camoufoxnative

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// fakeOnboardingDOM 为 node 里执行首次使用页表达式的最小 DOM 替身：元素带属性、文字、可见性（hidden 属性）、事件与一个只支持
// 标签、类、属性（=、*=）、:disabled 与后代组合的选择器引擎；勾选框 click 时切换状态并派发 click、input、change。
// 每个场景搭一页，按顺序执行若干步，记下每一步的返回值与页面上被点击的元素
const fakeOnboardingDOM = `
const fs = require('fs');
const expressions = JSON.parse(fs.readFileSync(process.argv[2], 'utf8'));
let clicks = [];
let currentHTML = null;
const parseCompound = text => {
  const parts = {tag: null, classes: [], attrs: [], pseudos: []};
  let index = 0;
  const tag = /^(\*|[a-zA-Z][\w-]*)/.exec(text);
  if (tag) {
    parts.tag = tag[1] === '*' ? null : tag[1].toLowerCase();
    index = tag[0].length;
  }
  while (index < text.length) {
    const rest = text.slice(index);
    let match;
    if ((match = /^\.([\w-]+)/.exec(rest))) parts.classes.push(match[1]);
    else if ((match = /^\[([\w-]+)(?:(\*?=)(?:"([^"]*)"|([^\]]*)))?\]/.exec(rest))) parts.attrs.push({name: match[1], op: match[2] || null, value: match[3] ?? match[4] ?? null});
    else if ((match = /^:([\w-]+)/.exec(rest))) parts.pseudos.push(match[1]);
    else throw new Error('不支持的选择器 ' + text);
    index += match[0].length;
  }
  return parts;
};
const matchCompound = (element, parts) => {
  if (parts.tag && element.localName !== parts.tag) return false;
  const classes = (element.getAttribute('class') || '').split(/\s+/);
  if (!parts.classes.every(name => classes.includes(name))) return false;
  for (const {name, op, value} of parts.attrs) {
    const actual = element.getAttribute(name);
    if (actual === null) return false;
    if (op === '=' && actual !== value) return false;
    if (op === '*=' && !actual.includes(value)) return false;
  }
  for (const pseudo of parts.pseudos) {
    if (pseudo !== 'disabled') throw new Error('不支持的伪类 ' + pseudo);
    if (!(['button', 'input'].includes(element.localName) && element.disabled)) return false;
  }
  return true;
};
const matchComplex = (element, selector) => {
  const compounds = selector.trim().split(/\s+/).map(parseCompound);
  if (!matchCompound(element, compounds[compounds.length - 1])) return false;
  let node = element.parentElement;
  for (let index = compounds.length - 2; index >= 0; index--) {
    while (node && !matchCompound(node, compounds[index])) node = node.parentElement;
    if (!node) return false;
    node = node.parentElement;
  }
  return true;
};
class Element {
  constructor(localName, attributes, children) {
    this.localName = localName;
    this.attributes = new Map(Object.entries(attributes || {}).map(([name, value]) => [name, String(value)]));
    this.parentElement = null;
    this.childNodes = [];
    this.listeners = {};
    this.checked = localName === 'input' && this.attributes.has('checked');
    for (const child of children || []) {
      if (typeof child !== 'string') child.parentElement = this;
      this.childNodes.push(child);
    }
  }
  get children() { return this.childNodes.filter(node => typeof node !== 'string'); }
  getAttribute(name) { return this.attributes.has(name) ? this.attributes.get(name) : null; }
  hasAttribute(name) { return this.attributes.has(name); }
  setAttribute(name, value) { this.attributes.set(name, String(value)); }
  removeAttribute(name) { this.attributes.delete(name); }
  get id() { return this.getAttribute('id') || ''; }
  get disabled() { return this.hasAttribute('disabled'); }
  set disabled(value) { if (value) this.setAttribute('disabled', ''); else this.removeAttribute('disabled'); }
  get required() { return this.hasAttribute('required'); }
  checkVisibility() {
    for (let node = this; node; node = node.parentElement) if (node.hasAttribute('hidden')) return false;
    return true;
  }
  get textContent() { return this.childNodes.map(node => typeof node === 'string' ? node : node.textContent).join(' '); }
  get innerText() { return this.checkVisibility() ? this.childNodes.map(node => typeof node === 'string' ? node : node.innerText).join(' ') : ''; }
  matches(selector) { return selector.split(',').some(item => matchComplex(this, item)); }
  closest(selector) {
    for (let node = this; node; node = node.parentElement) if (node.matches(selector)) return node;
    return null;
  }
  querySelectorAll(selector) {
    const found = [];
    const walk = parent => {
      for (const child of parent.children) {
        if (child.matches(selector)) found.push(child);
        walk(child);
      }
    };
    walk(this);
    return found;
  }
  querySelector(selector) { return this.querySelectorAll(selector)[0] || null; }
  get labels() {
    if (this.localName !== 'input') return undefined;
    const labels = [];
    const wrapping = this.closest('label');
    if (wrapping) labels.push(wrapping);
    for (const label of currentHTML.querySelectorAll('label')) {
      if (this.id && label.getAttribute('for') === this.id && !labels.includes(label)) labels.push(label);
    }
    return labels;
  }
  addEventListener(type, listener) { (this.listeners[type] = this.listeners[type] || []).push(listener); }
  dispatch(type) {
    for (let node = this; node; node = node.parentElement) for (const listener of node.listeners[type] || []) listener({type, target: this});
  }
  click() {
    clicks.push(this.getAttribute('data-name') || this.localName);
    if (['button', 'input'].includes(this.localName) && this.disabled) return;
    if (this.localName === 'input' && this.getAttribute('type') === 'checkbox') {
      this.checked = !this.checked;
      this.dispatch('click');
      this.dispatch('input');
      this.dispatch('change');
      return;
    }
    this.dispatch('click');
  }
}
const h = (tag, attributes, ...children) => new Element(tag, attributes, children);
// append 在页面已挂载后追加子元素（模拟 Angular 稍后才渲染出来）
const append = (parent, ...children) => {
  for (const child of children) {
    child.parentElement = parent;
    parent.childNodes.push(child);
  }
};
// mount 挂载一页：body 为 null 时模拟新文档还在解析 head
const mount = (body, pathname, readyState = 'complete') => {
  currentHTML = body ? h('html', {}, body) : h('html', {}, h('head', {}));
  globalThis.location = {hostname: 'aistudio.google.com', pathname, href: 'https://aistudio.google.com' + pathname};
  globalThis.document = {
    body,
    documentElement: currentHTML,
    readyState,
    title: 'Welcome to AI Studio | Google AI Studio',
    querySelectorAll: selector => [currentHTML, ...currentHTML.querySelectorAll('*')].filter(element => element.matches(selector)),
    getElementById: id => currentHTML.querySelectorAll('*').find(element => element.id === id) || null,
  };
  clicks = [];
};
// cookieBar 为 Google 页面底部的 Cookie 同意横幅
const cookieBar = text => h('div', {id: 'glue-cookie-notification-bar-1', class: 'glue-cookie-notification-bar'},
  h('button', {class: 'glue-cookie-notification-bar__accept', 'data-name': 'cookie-accept'}, text));
const run = name => JSON.parse((0, eval)(expressions[name]));
const evaluate = name => (0, eval)(expressions[name]);
// steps 按顺序执行 names 里的表达式，到 left 为止
const steps = names => {
  const replies = [];
  for (const name of names) {
    const reply = run(name);
    replies.push(reply);
    if (reply.state === 'left') break;
  }
  return replies;
};
// route 让按钮点击后像 SPA 一样离开首次使用页
const route = (button, onboarding) => button.addEventListener('click', () => {
  location.pathname = '/prompts/new_chat';
  onboarding.setAttribute('hidden', '');
});
const box = (input, ...label) => h('mat-checkbox', {class: 'mat-mdc-checkbox'},
  h('div', {class: 'mdc-form-field'}, h('div', {class: 'mdc-checkbox'}, input), h('label', {for: input.id}, ...label)));
const results = {};

// A：mat-checkbox 里的原生 input、标签带“*”（服务条款、年满 18 岁）；邮件订阅与调研邀请是可选项；继续按钮为主按钮样式，
// 必选项都勾上之前禁用；作用域里另有“閉じる”关闭按钮与 a[href] 条款链接，作用域外的全局横幅里有按钮
{
  const terms = h('input', {type: 'checkbox', id: 'terms', 'data-name': 'terms'});
  const age = h('input', {type: 'checkbox', id: 'age', 'data-name': 'age'});
  const mail = h('input', {type: 'checkbox', id: 'mail', 'data-name': 'mail'});
  const survey = h('input', {type: 'checkbox', id: 'survey', 'data-name': 'survey'});
  const proceed = h('button', {'mat-flat-button': '', color: 'primary', class: 'mat-mdc-unelevated-button', disabled: '', 'data-name': 'proceed'}, '続行');
  const close = h('button', {'aria-label': '閉じる', class: 'mat-mdc-icon-button', 'data-name': 'close'}, h('mat-icon', {}, 'close'));
  const onboarding = h('ms-onboarding', {},
    h('h1', {}, 'Google AI Studio へようこそ'),
    h('p', {}, '詳しくは、', h('a', {href: 'https://policies.google.com/terms', 'data-name': 'link'}, 'データの使用'), 'をご覧ください。'),
    close,
    h('ms-tos-disclaimer', {},
      box(terms, h('a', {href: 'https://ai.google.dev/terms', 'data-name': 'terms-link'}, 'Gemini API 追加利用規約'), 'に同意します', h('span', {class: 'required-marker'}, '*')),
      box(age, '* 私は 18 歳以上です'),
      box(mail, 'Google AI Studio の最新情報をメールで受け取る'),
      box(survey, 'ユーザー調査への参加に関するお知らせを受け取る')),
    proceed);
  const update = () => { proceed.disabled = !(terms.checked && age.checked); };
  terms.addEventListener('change', update);
  age.addEventListener('change', update);
  route(proceed, onboarding);
  const banner = h('ms-global-banner', {}, h('button', {'data-name': 'banner'}, 'Try it now'));
  mount(h('body', {}, h('ms-app', {}, banner, onboarding)), '/onboarding');
  const before = {detect: evaluate('detect'), ready: evaluate('ready'), observe: run('observe'), observeClicks: clicks.slice()};
  clicks = [];
  const replies = steps(['step', 'step', 'noclick', 'step', 'step']);
  results.A = Object.assign(before, {replies, clicks, mail: mail.checked, survey: survey.checked, detectAfter: evaluate('detect')});
}

// B：不在 /onboarding 路径，ms-onboarding 里是 role=checkbox 的 div（aria-required、aria-labelledby）与一个可选的资讯订阅项；
// 继续按钮 type=submit、一直启用，另有 Back 按钮
{
  const toggle = element => element.addEventListener('click', () => {
    element.setAttribute('aria-checked', element.getAttribute('aria-checked') === 'true' ? 'false' : 'true');
  });
  const tos = h('div', {role: 'checkbox', 'aria-checked': 'false', 'aria-required': 'true', 'aria-labelledby': 'tos-label', 'data-name': 'tos'});
  const age = h('div', {role: 'checkbox', 'aria-checked': 'false', 'aria-required': 'true', 'aria-label': 'I am 18 years of age or older', 'data-name': 'age'});
  const news = h('div', {role: 'checkbox', 'aria-checked': 'false', 'aria-labelledby': 'news-label', 'data-name': 'news'});
  [tos, age, news].forEach(toggle);
  const back = h('button', {type: 'button', 'data-name': 'back'}, 'Back');
  const submit = h('button', {type: 'submit', 'data-name': 'submit'}, 'Continue');
  const onboarding = h('ms-onboarding', {},
    h('div', {}, tos, h('span', {id: 'tos-label'}, 'I agree to the Google APIs Terms of Service')),
    h('div', {}, age),
    h('div', {}, news, h('span', {id: 'news-label'}, 'Send me product news and tips')),
    back, submit);
  route(submit, onboarding);
  mount(h('body', {}, h('ms-app', {}, onboarding)), '/');
  const detect = evaluate('detect');
  const replies = steps(['step', 'step', 'step', 'step']);
  results.B = {detect, replies, clicks, news: news.getAttribute('aria-checked')};
}

// C：勾选框都没有必选标记，继续按钮在服务条款与年满 18 岁都勾上之前禁用：不放宽时只等待；放宽后只勾标签含服务条款或年龄的项，
// 排在最前、标签同时提到条款与邮件的订阅项不勾
{
  const mailTerms = h('input', {type: 'checkbox', id: 'mail-terms', 'data-name': 'mail-terms'});
  const terms = h('input', {type: 'checkbox', id: 'terms', 'data-name': 'terms'});
  const age = h('input', {type: 'checkbox', id: 'age', 'data-name': 'age'});
  const proceed = h('button', {class: 'mat-mdc-raised-button mat-primary', disabled: '', 'data-name': 'proceed'}, 'Get started');
  const onboarding = h('ms-onboarding', {},
    box(mailTerms, 'Send me emails about updates to the Terms'),
    box(terms, 'I accept the Gemini API Additional Terms of Service'),
    box(age, 'I confirm that I am 18 or older'),
    proceed);
  const update = () => { proceed.disabled = !(terms.checked && age.checked); };
  terms.addEventListener('change', update);
  age.addEventListener('change', update);
  route(proceed, onboarding);
  mount(h('body', {}, h('ms-app', {}, onboarding)), '/onboarding');
  const strict = run('step');
  const replies = steps(['relaxed', 'relaxed', 'relaxed', 'relaxed']);
  results.C = {strict, replies, clicks, mailTerms: mailTerms.checked};
}

// D：只有“閉じる”关闭按钮与链接，作用域外只有页头与全局横幅里的按钮：找不到可点的继续按钮，返回 stuck 与结构摘要（邮箱打码）
{
  const done = h('input', {type: 'checkbox', id: 'done', checked: '', required: '', 'data-name': 'done'});
  const onboarding = h('ms-onboarding', {},
    h('p', {}, 'Signed in as someone@example.com'),
    box(done, 'someone.else@example.com で続行することに同意します'),
    h('button', {'data-name': 'close'}, '閉じる'),
    h('a', {href: '/terms', role: 'button', 'data-name': 'link'}, 'Continue to Terms'));
  const header = h('header', {}, h('button', {'data-name': 'header'}, 'Get started'));
  const banner = h('ms-global-banner', {}, h('button', {'data-name': 'banner'}, 'Continue'));
  mount(h('body', {}, header, banner, h('ms-app', {}, onboarding)), '/onboarding');
  results.D = {reply: run('step'), clicks};
}

// E：勾选框没有关联的 label，文字是兄弟元素：只包着这一个勾选框的容器文字带“*”时算必选；唯一剩下的按钮为继续按钮
{
  const age = h('input', {type: 'checkbox', 'data-name': 'age'});
  const mail = h('input', {type: 'checkbox', 'data-name': 'mail'});
  const proceed = h('button', {'data-name': 'proceed'}, '同意して続行');
  const onboarding = h('ms-onboarding', {},
    h('div', {}, age, h('span', {}, '私は 18 歳以上です *')),
    h('div', {}, mail, h('span', {}, 'メールを受け取る')),
    proceed);
  route(proceed, onboarding);
  mount(h('body', {}, h('ms-app', {}, onboarding)), '/onboarding');
  results.E = {replies: steps(['step', 'step', 'step']), clicks, mail: mail.checked};
}

// G：没有首次使用组件，作用域退到整页：侧栏里主按钮样式的按钮不点，只按关键词找继续按钮；“このアカウントで続行”不因带“アカウント”被排除
{
  const age = h('input', {type: 'checkbox', id: 'age', required: '', 'data-name': 'age'});
  const proceed = h('button', {'data-name': 'proceed'}, 'このアカウントで続行');
  const main = h('main', {}, h('input', {type: 'hidden'}), age, h('label', {for: 'age'}, '私は 18 歳以上です'), proceed);
  route(proceed, main);
  const sidebar = h('ms-navbar', {}, h('button', {'mat-flat-button': '', color: 'primary', 'data-name': 'api-key'}, 'Get API key'));
  mount(h('body', {}, sidebar, main), '/onboarding');
  results.G = {replies: steps(['step', 'step', 'step']), clicks};
}

// H：作用域里两个普通按钮都不像继续按钮（“詳細”“OK”），整页上有 Cookie 横幅“Accept all”：不到整页上找，返回 stuck、不点
{
  const age = h('input', {type: 'checkbox', id: 'age', required: '', checked: '', 'data-name': 'age'});
  const onboarding = h('ms-onboarding', {},
    box(age, '* 私は 18 歳以上です'),
    h('button', {'data-name': 'details'}, '詳細'),
    h('button', {'data-name': 'ok'}, 'OK'));
  mount(h('body', {}, h('ms-app', {}, onboarding), cookieBar('Accept all')), '/onboarding');
  results.H = {reply: run('step'), clicks};
}

// I：没有首次使用组件、作用域退到整页，唯一带关键词的是 Cookie 横幅的“同意する”：不点，返回 stuck
{
  const age = h('input', {type: 'checkbox', id: 'age', required: '', checked: '', 'data-name': 'age'});
  mount(h('body', {}, h('main', {}, age, h('label', {for: 'age'}, '私は 18 歳以上です')), cookieBar('同意する')), '/onboarding');
  results.I = {reply: run('step'), clicks};
}

// J：邮件订阅项的标签末尾带“*”（引出“* いつでも配信を停止できます”的脚注）：不算必选、不勾
{
  const age = h('input', {type: 'checkbox', id: 'age', 'data-name': 'age'});
  const mail = h('input', {type: 'checkbox', id: 'mail', 'data-name': 'mail'});
  const proceed = h('button', {type: 'submit', 'data-name': 'proceed'}, '続行');
  const onboarding = h('ms-onboarding', {},
    box(age, '* 私は 18 歳以上です'),
    box(mail, 'Google からのお知らせメールを受け取る*'),
    h('p', {}, '* いつでも配信を停止できます'),
    proceed);
  route(proceed, onboarding);
  mount(h('body', {}, h('ms-app', {}, onboarding)), '/onboarding');
  results.J = {replies: steps(['step', 'step', 'step']), clicks, mail: mail.checked};
}

// K：只有可选邮件订阅项的一步：主按钮是“Subscribe”，另一个是“No thanks”：两个都不点，返回 stuck
{
  const mail = h('input', {type: 'checkbox', id: 'mail', 'data-name': 'mail'});
  const onboarding = h('ms-onboarding', {},
    box(mail, 'Send me AI Studio news by email'),
    h('button', {'data-name': 'no'}, 'No thanks'),
    h('button', {'mat-flat-button': '', color: 'primary', 'data-name': 'subscribe'}, 'Subscribe'));
  mount(h('body', {}, h('ms-app', {}, onboarding)), '/onboarding');
  results.K = {reply: run('step'), clicks, mail: mail.checked};
}

// L：ms-onboarding 与 ms-tos-disclaimer 是兄弟节点，ms-onboarding 里只有欢迎文字与文档按钮，必选项与继续按钮在 ms-tos-disclaimer 里：
// 作用域取 ms-tos-disclaimer，文档按钮不点
{
  const terms = h('input', {type: 'checkbox', id: 'terms', 'data-name': 'terms'});
  const age = h('input', {type: 'checkbox', id: 'age', 'data-name': 'age'});
  const proceed = h('button', {type: 'submit', disabled: '', 'data-name': 'proceed'}, '続行');
  const update = () => { proceed.disabled = !(terms.checked && age.checked); };
  terms.addEventListener('change', update);
  age.addEventListener('change', update);
  const onboarding = h('ms-onboarding', {}, h('h1', {}, 'ようこそ'), h('button', {'data-name': 'learn'}, 'Get started with the docs'));
  const tos = h('ms-tos-disclaimer', {}, box(terms, '利用規約に同意します *'), box(age, '* 私は 18 歳以上です'), proceed);
  proceed.addEventListener('click', () => {
    location.pathname = '/prompts/new_chat';
    onboarding.setAttribute('hidden', '');
    tos.setAttribute('hidden', '');
  });
  mount(h('body', {}, h('ms-app', {}, onboarding, tos)), '/onboarding');
  const observe = run('observe');
  results.L = {observe, replies: steps(['step', 'step', 'step', 'step']), clicks};
}

// L2：必选项在 ms-tos-disclaimer 里，继续按钮在兄弟节点 ms-onboarding 里：作用域里挑不出时在其余首次使用组件里找
{
  const age = h('input', {type: 'checkbox', id: 'age', required: '', 'data-name': 'age'});
  const proceed = h('button', {'data-name': 'proceed'}, '続行');
  const onboarding = h('ms-onboarding', {}, h('h1', {}, 'ようこそ'), proceed);
  const tos = h('ms-tos-disclaimer', {}, box(age, '私は 18 歳以上です'));
  proceed.addEventListener('click', () => {
    location.pathname = '/prompts/new_chat';
    onboarding.setAttribute('hidden', '');
    tos.setAttribute('hidden', '');
  });
  mount(h('body', {}, h('ms-app', {}, onboarding, tos)), '/onboarding');
  results.L2 = {replies: steps(['step', 'step', 'step']), clicks};
}

// M：新文档还在解析 head（body 为 null）：识别为首次使用页，一步式表达式返回 waiting、不抛错
{
  mount(null, '/onboarding', 'loading');
  results.M = {detect: evaluate('detect'), step: run('step'), observe: run('observe')};
}

// N：/onboarding 页面只有外壳（Angular 还没渲染出首次使用组件），再是组件出来了、整页载入未完成、按钮还没出来：都返回 waiting
// （不算找不到按钮）；渲染完成后照常勾选、点击
{
  const app = h('ms-app', {});
  mount(h('body', {}, app), '/onboarding', 'interactive');
  const shell = run('step');
  document.readyState = 'complete';
  const shellComplete = run('step');
  const onboarding = h('ms-onboarding', {}, h('h1', {}, 'ようこそ'));
  append(app, onboarding);
  document.readyState = 'interactive';
  const partial = run('step');
  const age = h('input', {type: 'checkbox', id: 'age', 'data-name': 'age'});
  const proceed = h('button', {type: 'submit', 'data-name': 'proceed'}, '続行');
  append(onboarding, box(age, '* 私は 18 歳以上です'), proceed);
  route(proceed, onboarding);
  document.readyState = 'complete';
  results.N = {shell, shellComplete, partial, replies: steps(['step', 'step', 'step']), clicks};
}

// O：两个普通按钮“I do not agree”“I agree”都含 agree：拒绝类不点，点“I agree”
{
  const age = h('input', {type: 'checkbox', id: 'age', required: '', checked: '', 'data-name': 'age'});
  const agree = h('button', {'data-name': 'agree'}, 'I agree');
  const onboarding = h('ms-onboarding', {}, box(age, 'I am 18 or older'), h('button', {'data-name': 'disagree'}, 'I do not agree'), agree);
  route(agree, onboarding);
  mount(h('body', {}, h('ms-app', {}, onboarding)), '/onboarding');
  results.O = {replies: steps(['step', 'step']), clicks};
}

// P：唯一带主按钮样式的是“詳細”，“続行”是普通按钮：有带关键词的候选时只在它们里面挑，点“続行”
{
  const age = h('input', {type: 'checkbox', id: 'age', required: '', checked: '', 'data-name': 'age'});
  const proceed = h('button', {'data-name': 'proceed'}, '続行');
  const onboarding = h('ms-onboarding', {}, box(age, '* 私は 18 歳以上です'),
    h('button', {'mat-flat-button': '', color: 'primary', 'data-name': 'details'}, '詳細'), proceed);
  route(proceed, onboarding);
  mount(h('body', {}, h('ms-app', {}, onboarding)), '/onboarding');
  results.P = {replies: steps(['step', 'step']), clicks};
}

// Q：继续是带主按钮样式的链接（a[href] mat-flat-button），整页上有 Cookie 横幅“すべて同意”：链接与横幅都不点，返回 stuck，
// 结构摘要的 links 里列出链接文字
{
  const age = h('input', {type: 'checkbox', id: 'age', required: '', checked: '', 'data-name': 'age'});
  const onboarding = h('ms-onboarding', {}, box(age, '* 私は 18 歳以上です'),
    h('a', {href: '/prompts/new_chat', 'mat-flat-button': '', class: 'mat-mdc-unelevated-button', 'data-name': 'link-continue'}, '続行'));
  mount(h('body', {}, h('ms-app', {}, onboarding), cookieBar('すべて同意')), '/onboarding');
  results.Q = {reply: run('step'), clicks};
}

// R：继续按钮一直启用（点了才校验），服务条款项没有必选标记、年满 18 岁项带“*”：勾上 18 岁后点继续仍停在首次使用页，
// 不放宽时照常再点；放宽且点过继续之后勾服务条款项（邮件项不勾），再点继续离开
{
  const terms = h('input', {type: 'checkbox', id: 'terms', 'data-name': 'terms'});
  const age = h('input', {type: 'checkbox', id: 'age', 'data-name': 'age'});
  const mail = h('input', {type: 'checkbox', id: 'mail', 'data-name': 'mail'});
  const proceed = h('button', {type: 'submit', 'data-name': 'proceed'}, '続行');
  const onboarding = h('ms-onboarding', {},
    box(terms, 'Google 利用規約に同意します'), box(age, '* 私は 18 歳以上です'), box(mail, 'メールで最新情報を受け取る'), proceed);
  proceed.addEventListener('click', () => {
    if (!(terms.checked && age.checked)) return;
    location.pathname = '/prompts/new_chat';
    onboarding.setAttribute('hidden', '');
  });
  mount(h('body', {}, h('ms-app', {}, onboarding)), '/onboarding');
  results.R = {replies: steps(['step', 'step', 'relaxed', 'relaxedClicked', 'step', 'step']), clicks, mail: mail.checked};
}

// T：必选项不可勾（disabled）：等待，结构摘要标出不可勾与必选依据
{
  const age = h('input', {type: 'checkbox', id: 'age', required: '', disabled: '', 'data-name': 'age'});
  const onboarding = h('ms-onboarding', {}, box(age, '私は 18 歳以上です'), h('button', {'data-name': 'proceed'}, '続行'));
  mount(h('body', {}, h('ms-app', {}, onboarding)), '/onboarding');
  results.T = {reply: run('step'), clicks};
}

// U：首次使用组件渲染在 Material 对话框里，继续按钮在组件之外的对话框操作区；侧栏里另有“Get started”：在对话框里找，侧栏不点
{
  const age = h('input', {type: 'checkbox', id: 'age', 'data-name': 'age'});
  const proceed = h('button', {'mat-flat-button': '', 'data-name': 'proceed'}, '続行');
  const dialog = h('mat-dialog-container', {}, h('ms-onboarding', {}, box(age, '* 私は 18 歳以上です')), h('mat-dialog-actions', {}, proceed));
  route(proceed, dialog);
  mount(h('body', {}, h('ms-navbar', {}, h('button', {'data-name': 'sidebar'}, 'Get started')), h('div', {class: 'cdk-overlay-container'}, dialog)), '/onboarding');
  results.U = {replies: steps(['step', 'step', 'step']), clicks};
}

// V：继续是链接样式（a[href]），作用域里另有一个没有 aria-label 的图标按钮（close_small、arrow_back、chevron_left、help_outline）：
// 勾上必选项后图标按钮不点（ligature 的下划线换成空格后按词认出关闭与返回，认不出是什么的按 icon 排除，不当作唯一剩下的按钮），返回 stuck
{
  results.V = {};
  for (const icon of ['close_small', 'arrow_back', 'chevron_left', 'help_outline']) {
    const age = h('input', {type: 'checkbox', id: 'age', 'data-name': 'age'});
    const onboarding = h('ms-onboarding', {}, box(age, '* 私は 18 歳以上です'),
      h('button', {class: 'mat-mdc-icon-button', 'data-name': icon}, h('mat-icon', {class: 'material-symbols-outlined'}, icon)),
      h('a', {href: '/prompts/new_chat', 'data-name': 'link-continue'}, '続行'));
    mount(h('body', {}, h('ms-app', {}, onboarding)), '/onboarding');
    results.V[icon] = {replies: steps(['step', 'step', 'step']), clicks};
  }
}

// W：作用域里唯一的按钮是带标点的拒绝或切换账号类（“No, thanks”“Use another account”“別のアカウントを使用”“Not you?”“使用其他账号”），
// 或只有“No, thanks”与订阅类主按钮：都不点，返回 stuck
{
  results.W = {};
  for (const text of ['No, thanks', 'Use another account', '別のアカウントを使用', 'Not you?', '使用其他账号']) {
    const age = h('input', {type: 'checkbox', id: 'age', required: '', checked: '', 'data-name': 'age'});
    const onboarding = h('ms-onboarding', {}, box(age, '私は 18 歳以上です'), h('button', {'data-name': 'only'}, text));
    mount(h('body', {}, h('ms-app', {}, onboarding)), '/onboarding');
    results.W[text] = {reply: run('step'), clicks};
  }
  const mail = h('input', {type: 'checkbox', id: 'mail', 'data-name': 'mail'});
  const onboarding = h('ms-onboarding', {}, box(mail, 'Send me AI Studio news by e-mail'),
    h('button', {'data-name': 'no'}, 'No, thanks'),
    h('button', {'mat-flat-button': '', color: 'primary', 'data-name': 'subscribe'}, 'Subscribe'));
  mount(h('body', {}, h('ms-app', {}, onboarding)), '/onboarding');
  results.W.subscribe = {reply: run('step'), clicks, mail: mail.checked};
}

// Y：没有首次使用组件、作用域退到整页，全局横幅（“Try Gemini 3 now”）与页头（“Get started”）在文档顺序上排在前面、都含继续类关键词：
// 不点，点 main 里的“続行”
{
  const age = h('input', {type: 'checkbox', id: 'age', required: '', 'data-name': 'age'});
  const proceed = h('button', {'data-name': 'proceed'}, '続行');
  const main = h('main', {}, age, h('label', {for: 'age'}, '私は 18 歳以上です'), proceed);
  route(proceed, main);
  mount(h('body', {}, h('ms-global-banner', {}, h('button', {'data-name': 'banner'}, 'Try Gemini 3 now')),
    h('header', {}, h('button', {'data-name': 'header'}, 'Get started')), main), '/onboarding');
  results.Y = {replies: steps(['step', 'step', 'step']), clicks};
}

// Z1：原生 input 被隐藏（只显示自绘样式），包着它的 label 可见：算可见的勾选框（不在 /onboarding 路径时识别同样认得），
// 标签带“*”为必选，点隐藏的 input 勾上
{
  const age = h('input', {type: 'checkbox', hidden: '', 'data-name': 'age'});
  const proceed = h('button', {type: 'submit', disabled: '', 'data-name': 'proceed'}, '続行');
  age.addEventListener('change', () => { proceed.disabled = !age.checked; });
  const onboarding = h('ms-onboarding', {}, h('label', {}, age, h('span', {class: 'fake-box'}), h('span', {}, '* 私は 18 歳以上です')), proceed);
  route(proceed, onboarding);
  mount(h('body', {}, h('ms-app', {}, onboarding)), '/');
  results.Z1 = {detect: evaluate('detect'), observe: run('observe'), replies: steps(['step', 'step', 'step']), clicks};
}

// Z2：role=checkbox 的自绘勾选框里只有图标（check_box_outline_blank），标签是兄弟元素：去掉图标文字后没有标签，取只包着这一个勾选框的
// 容器文字（带“*”为必选）；同样写法的邮件项不勾
{
  const iconBox = name => {
    const element = h('div', {role: 'checkbox', 'aria-checked': 'false', 'data-name': name}, h('mat-icon', {}, 'check_box_outline_blank'));
    element.addEventListener('click', () => element.setAttribute('aria-checked', element.getAttribute('aria-checked') === 'true' ? 'false' : 'true'));
    return element;
  };
  const age = iconBox('age');
  const mail = iconBox('mail');
  const proceed = h('button', {type: 'submit', 'data-name': 'proceed'}, '続行');
  const onboarding = h('ms-onboarding', {},
    h('div', {class: 'row'}, age, h('span', {}, '* 私は 18 歳以上です')),
    h('div', {class: 'row'}, mail, h('span', {}, 'メールで最新情報を受け取る')),
    proceed);
  route(proceed, onboarding);
  mount(h('body', {}, h('ms-app', {}, onboarding)), '/onboarding');
  results.Z2 = {observe: run('observe'), replies: steps(['step', 'step', 'step']), clicks, mail: mail.getAttribute('aria-checked')};
}

// S：结构摘要的补充信息：服务条款项的标签很长（保留开头与结尾，结尾的“*”不丢），作用域里有下拉、日期、单选等其他表单控件，
// 作用域外有侧栏按钮与一个不可见的勾选框
{
  const terms = h('input', {type: 'checkbox', id: 'terms', 'data-name': 'terms'});
  const onboarding = h('ms-onboarding', {},
    box(terms, 'Google 利用規約、Gemini API 追加利用規約、および Google のプライバシー ポリシーに記載のとおりデータが処理されることに同意します *'),
    h('mat-select', {role: 'combobox'}, '国を選択'),
    h('input', {type: 'date'}),
    h('div', {role: 'radio', 'aria-checked': 'false'}, '個人'),
    h('input', {type: 'hidden'}),
    h('button', {type: 'submit', disabled: '', 'data-name': 'proceed'}, '続行'));
  const navbar = h('ms-navbar', {}, h('button', {'data-name': 'api-key'}, 'Get API key'), h('input', {type: 'checkbox', hidden: ''}));
  mount(h('body', {}, h('ms-app', {}, navbar, onboarding)), '/onboarding');
  results.S = {observe: run('observe'), clicks};
}

// F：识别。对话页上同时可见的输入框与带勾选框的条款组件不算首次使用页；没有输入框时带可见勾选框的 ms-tos-disclaimer 算；
// 不带勾选框的 ms-onboarding 不算；其他站点不算
{
  const chat = () => h('ms-prompt-box', {}, h('textarea', {}));
  const disclaimer = () => h('ms-tos-disclaimer', {}, h('input', {type: 'checkbox'}));
  mount(h('body', {}, h('ms-app', {}, chat(), disclaimer())), '/prompts/new_chat');
  const chatPage = {detect: evaluate('detect'), ready: evaluate('ready'), step: run('step')};
  mount(h('body', {}, h('ms-app', {}, disclaimer())), '/prompts/new_chat');
  const disclaimerOnly = evaluate('detect');
  mount(h('body', {}, h('ms-app', {}, h('ms-onboarding', {}, h('button', {}, 'Get started')))), '/');
  const tourOnly = evaluate('detect');
  mount(h('body', {}, h('ms-onboarding', {}, h('input', {type: 'checkbox'}))), '/onboarding');
  location.hostname = 'example.com';
  const otherSite = evaluate('detect');
  results.F = {chatPage, disclaimerOnly, tourOnly, otherSite};
}
console.log(JSON.stringify(results));
`

// TestOnboardingStepExpressionInNode 在 node 里用最小 DOM 替身执行首次使用页的识别与一步式表达式（机器上没有 node 时跳过）：
// 只勾必选项（标签带“*”、aria-required、required、容器文字带“*”），可选的邮件订阅、调研邀请不勾；必选项勾上前按钮禁用时等待，
// 勾上后才点继续；关闭按钮、链接、页头与横幅里的按钮不点；离开页面返回 left；找不到按钮返回 stuck 并带打码的结构摘要；
// 按钮一直禁用时放宽为勾标签含服务条款或年龄的项，邮件类仍不勾
func TestOnboardingStepExpressionInNode(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("没有 node，跳过首次使用页表达式的执行检查")
	}
	directory := t.TempDir()
	expressions, err := json.Marshal(map[string]string{
		"detect":         onboardingPageExpression,
		"ready":          workerPageReadyExpression,
		"step":           onboardingStepExpression(onboardingStepOptions{Click: true}),
		"noclick":        onboardingStepExpression(onboardingStepOptions{}),
		"relaxed":        onboardingStepExpression(onboardingStepOptions{Click: true, Relaxed: true}),
		"relaxedClicked": onboardingStepExpression(onboardingStepOptions{Click: true, Relaxed: true, Clicked: true}),
		"observe":        onboardingStepExpression(onboardingStepOptions{Observe: true}),
	})
	if err != nil {
		t.Fatal(err)
	}
	expressionPath := filepath.Join(directory, "expressions.json")
	scriptPath := filepath.Join(directory, "dom.js")
	if err := os.WriteFile(expressionPath, expressions, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(scriptPath, []byte(fakeOnboardingDOM), 0o600); err != nil {
		t.Fatal(err)
	}
	output, err := exec.Command(node, scriptPath, expressionPath).CombinedOutput()
	if err != nil {
		t.Fatalf("node 执行首次使用页表达式失败: %v\n%s", err, output)
	}
	var results map[string]json.RawMessage
	if err := json.Unmarshal(output, &results); err != nil {
		t.Fatalf("node 输出无法解析: %v\n%s", err, output)
	}
	scenario := func(name string, target any) {
		t.Helper()
		if err := json.Unmarshal(results[name], target); err != nil {
			t.Fatalf("场景 %s 无法解析: %v\n%s", name, err, results[name])
		}
	}
	states := func(replies []onboardingStepReply) string {
		names := make([]string, len(replies))
		for index, reply := range replies {
			names[index] = reply.State
		}
		return strings.Join(names, ",")
	}

	var a struct {
		Detect, Ready, DetectAfter, Mail, Survey bool
		Observe                                  onboardingStepReply
		ObserveClicks                            []string
		Replies                                  []onboardingStepReply
		Clicks                                   []string
	}
	scenario("A", &a)
	if !a.Detect || !a.Ready || a.DetectAfter {
		t.Fatalf("A：/onboarding 应识别为首次使用页、就绪等待立即结束，离开后不再识别: %+v", a)
	}
	if a.Observe.State != "observe" || len(a.ObserveClicks) != 0 {
		t.Fatalf("A：只读摘要不应点击任何东西: %+v", a)
	}
	summary := mustJSON(t, a.Observe.Summary)
	wantSummary := `{"scope":"ms-onboarding","bodyBoxes":4,"allBoxes":4,"boxes":[` +
		`{"req":true,"on":false,"by":"star","t":true,"kind":"input/mat-checkbox","label":"Gemini API 追加利用規約 に同意します *"},` +
		`{"req":true,"on":false,"by":"star","t":true,"kind":"input/mat-checkbox","label":"* 私は 18 歳以上です"},` +
		`{"req":false,"on":false,"o":true,"kind":"input/mat-checkbox","label":"Google AI Studio の最新情報をメールで受け取る"},` +
		`{"req":false,"on":false,"o":true,"kind":"input/mat-checkbox","label":"ユーザー調査への参加に関するお知らせを受け取る"}],` +
		`"buttons":[{"text":"閉じる","on":true,"skip":"neg"},{"text":"続行","on":false}],"links":["データの使用","Gemini API 追加利用規約"],` +
		`"outside":[{"text":"Try it now","on":true,"skip":"area"}]}`
	if summary != wantSummary {
		t.Fatalf("A：结构摘要\n得到 %s\n期望 %s", summary, wantSummary)
	}
	if got := states(a.Replies); got != "checked,checked,waiting,clicked,left" {
		t.Fatalf("A：步骤应为勾两个必选项、刚点过不点、点继续、离开，得到 %s: %+v", got, a.Replies)
	}
	if a.Replies[2].Reason != "等待上一次点击生效" || a.Replies[3].Button != "続行" {
		t.Fatalf("A：不允许点击时应等待，之后点击“続行”: %+v", a.Replies)
	}
	if strings.Join(a.Clicks, ",") != "terms,age,proceed" || a.Mail || a.Survey {
		t.Fatalf("A：只应点两个必选项与继续按钮（关闭按钮、链接、横幅与可选项都不碰），得到 %v mail=%t survey=%t", a.Clicks, a.Mail, a.Survey)
	}

	var b struct {
		Detect  bool
		Replies []onboardingStepReply
		Clicks  []string
		News    string
	}
	scenario("B", &b)
	if !b.Detect || states(b.Replies) != "checked,checked,clicked,left" || strings.Join(b.Clicks, ",") != "tos,age,submit" || b.News != "false" {
		t.Fatalf("B：role=checkbox 的必选项勾上后才点 type=submit 的继续按钮，资讯订阅与 Back 不碰: %+v", b)
	}
	if b.Replies[0].Label != "I agree to the Google APIs Terms of Service" || b.Replies[1].Label != "I am 18 years of age or older" {
		t.Fatalf("B：标签应取 aria-labelledby 与 aria-label: %+v", b.Replies)
	}

	var c struct {
		Strict    onboardingStepReply
		Replies   []onboardingStepReply
		Clicks    []string
		MailTerms bool
	}
	scenario("C", &c)
	if c.Strict.State != "waiting" || c.Strict.Reason != "继续按钮未启用" {
		t.Fatalf("C：没有必选标记、按钮禁用时不放宽只等待: %+v", c.Strict)
	}
	if states(c.Replies) != "checked,checked,clicked,left" || !c.Replies[0].Relaxed || strings.Join(c.Clicks, ",") != "terms,age,proceed" || c.MailTerms {
		t.Fatalf("C：放宽后只勾服务条款与年龄项，邮件类不勾: %+v", c)
	}

	var d struct {
		Reply  onboardingStepReply
		Clicks []string
	}
	scenario("D", &d)
	if d.Reply.State != "stuck" || d.Reply.Reason != "找不到可点的继续按钮" || len(d.Clicks) != 0 {
		t.Fatalf("D：只有关闭按钮与链接时应返回 stuck、不点击: %+v", d)
	}
	structure := formatOnboardingStructure(d.Reply.Summary)
	wantStructure := ` | 结构={"scope":"ms-onboarding","bodyBoxes":1,"allBoxes":1,` +
		`"boxes":[{"req":true,"on":true,"by":"required","kind":"input/mat-checkbox","label":"so***@*** で続行することに同意します"}],` +
		`"buttons":[{"text":"閉じる","on":true,"skip":"neg"},{"text":"Continue to Terms","on":true,"skip":"link"}],` +
		`"outside":[{"text":"Get started","on":true,"skip":"area"},{"text":"Continue","on":true,"skip":"area"}]}`
	if structure != wantStructure || strings.Contains(structure, "example") {
		t.Fatalf("D：结构摘要\n得到 %s\n期望 %s", structure, wantStructure)
	}

	var e struct {
		Replies []onboardingStepReply
		Clicks  []string
		Mail    bool
	}
	scenario("E", &e)
	if states(e.Replies) != "checked,clicked,left" || strings.Join(e.Clicks, ",") != "age,proceed" || e.Mail {
		t.Fatalf("E：容器文字带“*”的勾选框算必选，唯一剩下的按钮为继续按钮: %+v", e)
	}

	var g struct {
		Replies []onboardingStepReply
		Clicks  []string
	}
	scenario("G", &g)
	if states(g.Replies) != "checked,clicked,left" || strings.Join(g.Clicks, ",") != "age,proceed" || g.Replies[0].Summary.Scope != "body" {
		t.Fatalf("G：整页作用域只按关键词找继续按钮，侧栏的主按钮不点: %+v", g)
	}

	var f struct {
		ChatPage struct {
			Detect, Ready bool
			Step          onboardingStepReply
		}
		DisclaimerOnly, TourOnly, OtherSite bool
	}
	scenario("F", &f)
	if f.ChatPage.Detect || !f.ChatPage.Ready || f.ChatPage.Step.State != "left" {
		t.Fatalf("F：对话页（输入框可见）不算首次使用页，一步式表达式返回 left: %+v", f.ChatPage)
	}
	if !f.DisclaimerOnly || f.TourOnly || f.OtherSite {
		t.Fatalf("F：识别 disclaimer=%t tour=%t other=%t，期望 true false false", f.DisclaimerOnly, f.TourOnly, f.OtherSite)
	}

	type single struct {
		Reply  onboardingStepReply
		Clicks []string
		Mail   bool
	}
	type sequence struct {
		Replies []onboardingStepReply
		Clicks  []string
		Mail    bool
	}
	var hCase, iCase, kCase, qCase, tCase single
	scenario("H", &hCase)
	if hCase.Reply.State != "stuck" || len(hCase.Clicks) != 0 ||
		mustJSON(t, hCase.Reply.Summary.Buttons) != `[{"text":"詳細","on":true},{"text":"OK","on":true}]` {
		t.Fatalf("H：作用域里挑不出继续按钮时不到整页上找，Cookie 横幅不点: %+v", hCase)
	}
	scenario("I", &iCase)
	if iCase.Reply.State != "stuck" || len(iCase.Clicks) != 0 || iCase.Reply.Summary.Scope != "body" ||
		mustJSON(t, iCase.Reply.Summary.Buttons) != `[{"text":"同意する","on":true,"skip":"cookie"}]` {
		t.Fatalf("I：整页作用域下 Cookie 横幅的按钮不点: %+v", iCase)
	}
	var jCase sequence
	scenario("J", &jCase)
	if states(jCase.Replies) != "checked,clicked,left" || strings.Join(jCase.Clicks, ",") != "age,proceed" || jCase.Mail {
		t.Fatalf("J：邮件项标签末尾带“*”也不勾: %+v", jCase)
	}
	scenario("K", &kCase)
	if kCase.Reply.State != "stuck" || len(kCase.Clicks) != 0 || kCase.Mail ||
		mustJSON(t, kCase.Reply.Summary.Buttons) != `[{"text":"No thanks","on":true,"skip":"neg"},{"text":"Subscribe","on":true,"skip":"opt"}]` {
		t.Fatalf("K：订阅类主按钮与“No thanks”都不点: %+v", kCase)
	}
	var l struct {
		Observe onboardingStepReply
		Replies []onboardingStepReply
		Clicks  []string
	}
	scenario("L", &l)
	wantL := `{"scope":"ms-tos-disclaimer","bodyBoxes":2,"allBoxes":2,"boxes":[` +
		`{"req":true,"on":false,"by":"star","t":true,"kind":"input/mat-checkbox","label":"利用規約に同意します *"},` +
		`{"req":true,"on":false,"by":"star","t":true,"kind":"input/mat-checkbox","label":"* 私は 18 歳以上です"}],"buttons":[{"text":"続行","on":false}],` +
		`"outside":[{"text":"Get started with the docs","on":true}]}`
	if got := mustJSON(t, l.Observe.Summary); got != wantL {
		t.Fatalf("L：两者是兄弟节点时作用域取含勾选框的 ms-tos-disclaimer\n得到 %s\n期望 %s", got, wantL)
	}
	if states(l.Replies) != "checked,checked,clicked,left" || strings.Join(l.Clicks, ",") != "terms,age,proceed" {
		t.Fatalf("L：勾两个必选项后点“続行”，文档按钮不点: %+v", l)
	}
	var l2 sequence
	scenario("L2", &l2)
	if states(l2.Replies) != "checked,clicked,left" || strings.Join(l2.Clicks, ",") != "age,proceed" {
		t.Fatalf("L2：继续按钮在兄弟组件里时也能找到: %+v", l2)
	}
	var m struct {
		Detect  bool
		Step    onboardingStepReply
		Observe onboardingStepReply
	}
	scenario("M", &m)
	if !m.Detect || m.Step.State != "waiting" || m.Step.Reason != "页面正在载入" || m.Observe.State != "waiting" {
		t.Fatalf("M：body 为 null 时应等待而不是抛错: %+v", m)
	}
	var n struct {
		Shell, ShellComplete, Partial onboardingStepReply
		Replies                       []onboardingStepReply
		Clicks                        []string
	}
	scenario("N", &n)
	for name, reply := range map[string]onboardingStepReply{"shell": n.Shell, "shellComplete": n.ShellComplete, "partial": n.Partial} {
		if reply.State != "waiting" || reply.Reason != "首次使用页尚未渲染" {
			t.Fatalf("N：%s 首次使用页尚未渲染时应等待、不计 stuck: %+v", name, reply)
		}
	}
	if states(n.Replies) != "checked,clicked,left" || strings.Join(n.Clicks, ",") != "age,proceed" {
		t.Fatalf("N：渲染出来之后照常完成: %+v", n)
	}
	var o, pCase sequence
	scenario("O", &o)
	if states(o.Replies) != "clicked,left" || strings.Join(o.Clicks, ",") != "agree" {
		t.Fatalf("O：“I do not agree”不点，点“I agree”: %+v", o)
	}
	scenario("P", &pCase)
	if states(pCase.Replies) != "clicked,left" || strings.Join(pCase.Clicks, ",") != "proceed" {
		t.Fatalf("P：主按钮样式的“詳細”不抢在“続行”前面: %+v", pCase)
	}
	scenario("Q", &qCase)
	if qCase.Reply.State != "stuck" || len(qCase.Clicks) != 0 || mustJSON(t, qCase.Reply.Summary.Links) != `["続行"]` ||
		len(qCase.Reply.Summary.Buttons) != 0 {
		t.Fatalf("Q：链接样式的继续与 Cookie 横幅都不点，摘要列出链接: %+v", qCase)
	}
	var r sequence
	scenario("R", &r)
	if states(r.Replies) != "checked,clicked,clicked,checked,clicked,left" || strings.Join(r.Clicks, ",") != "age,proceed,proceed,terms,proceed" ||
		!r.Replies[3].Relaxed || r.Mail {
		t.Fatalf("R：按钮一直启用时，点过继续仍停在首次使用页才放宽、勾服务条款项，邮件项不勾: %+v", r)
	}
	scenario("T", &tCase)
	if tCase.Reply.State != "waiting" || tCase.Reply.Reason != "必选项不可勾选" || len(tCase.Clicks) != 0 ||
		mustJSON(t, tCase.Reply.Summary.Boxes) != `[{"req":true,"on":false,"dis":true,"by":"required","t":true,"kind":"input/mat-checkbox","label":"私は 18 歳以上です"}]` {
		t.Fatalf("T：必选项不可勾时等待，摘要标出不可勾: %+v", tCase)
	}
	var u sequence
	scenario("U", &u)
	if states(u.Replies) != "checked,clicked,left" || strings.Join(u.Clicks, ",") != "age,proceed" {
		t.Fatalf("U：继续按钮在作用域所在的对话框里，侧栏不点: %+v", u)
	}
	var v map[string]sequence
	scenario("V", &v)
	for icon, skip := range map[string]string{"close_small": "neg", "arrow_back": "neg", "chevron_left": "neg", "help_outline": "icon"} {
		got := v[icon]
		if states(got.Replies) != "checked,stuck,stuck" || strings.Join(got.Clicks, ",") != "age" {
			t.Fatalf("V：没有 aria-label 的图标按钮 %s 不应被当作唯一剩下的按钮点击: %+v", icon, got)
		}
		if buttons := mustJSON(t, got.Replies[1].Summary.Buttons); buttons != `[{"text":"`+icon+`","on":true,"skip":"`+skip+`"}]` {
			t.Fatalf("V：图标按钮 %s 的不点原因应为 %s，得到 %s", icon, skip, buttons)
		}
	}
	var w map[string]single
	scenario("W", &w)
	for _, text := range []string{"No, thanks", "Use another account", "別のアカウントを使用", "Not you?", "使用其他账号"} {
		got := w[text]
		if got.Reply.State != "stuck" || len(got.Clicks) != 0 ||
			mustJSON(t, got.Reply.Summary.Buttons) != `[{"text":"`+text+`","on":true,"skip":"neg"}]` {
			t.Fatalf("W：唯一的按钮“%s”是拒绝或切换账号类，不应点击: %+v", text, got)
		}
	}
	if got := w["subscribe"]; got.Reply.State != "stuck" || len(got.Clicks) != 0 || got.Mail ||
		mustJSON(t, got.Reply.Summary.Buttons) != `[{"text":"No, thanks","on":true,"skip":"neg"},{"text":"Subscribe","on":true,"skip":"opt"}]` {
		t.Fatalf("W：“No, thanks”与订阅类主按钮都不点、带连字符的 e-mail 项不勾: %+v", got)
	}
	var y sequence
	scenario("Y", &y)
	if states(y.Replies) != "checked,clicked,left" || strings.Join(y.Clicks, ",") != "age,proceed" || y.Replies[0].Summary.Scope != "body" ||
		mustJSON(t, y.Replies[0].Summary.Buttons) != `[{"text":"Try Gemini 3 now","on":true,"skip":"area"},{"text":"Get started","on":true,"skip":"area"},{"text":"続行","on":true}]` {
		t.Fatalf("Y：整页作用域下全局横幅与页头里含关键词的按钮不点: %+v", y)
	}
	var z1 struct {
		Detect  bool
		Observe onboardingStepReply
		Replies []onboardingStepReply
		Clicks  []string
	}
	scenario("Z1", &z1)
	if !z1.Detect || states(z1.Replies) != "checked,clicked,left" || strings.Join(z1.Clicks, ",") != "age,proceed" ||
		mustJSON(t, z1.Observe.Summary.Boxes) != `[{"req":true,"on":false,"by":"star","t":true,"hid":true,"kind":"input","label":"* 私は 18 歳以上です"}]` {
		t.Fatalf("Z1：原生 input 被隐藏、label 可见时算可见的必选项并勾上: %+v", z1)
	}
	var z2 struct {
		Observe onboardingStepReply
		Replies []onboardingStepReply
		Clicks  []string
		Mail    string
	}
	scenario("Z2", &z2)
	wantZ2 := `[{"req":true,"on":false,"by":"ctx","t":true,"kind":"role/div","label":"* 私は 18 歳以上です"},` +
		`{"req":false,"on":false,"o":true,"kind":"role/div","label":"メールで最新情報を受け取る"}]`
	if got := mustJSON(t, z2.Observe.Summary.Boxes); got != wantZ2 {
		t.Fatalf("Z2：只有图标的自绘勾选框取容器文字作标签\n得到 %s\n期望 %s", got, wantZ2)
	}
	if states(z2.Replies) != "checked,clicked,left" || strings.Join(z2.Clicks, ",") != "age,proceed" || z2.Mail != "false" {
		t.Fatalf("Z2：只勾容器文字带“*”的项，邮件项不勾: %+v", z2)
	}
	var s struct {
		Observe onboardingStepReply
		Clicks  []string
	}
	scenario("S", &s)
	longTerms := "Google 利用規約、Gemini API 追加利用規約、および Google のプライバシー ポリシーに記載のとおりデータが処理されることに同意します *"
	wantS := `{"scope":"ms-onboarding","bodyBoxes":1,"allBoxes":2,"boxes":[{"req":true,"on":false,"by":"star","t":true,"kind":"input/mat-checkbox",` +
		`"label":"` + string([]rune(longTerms)[:40]) + `…` + string([]rune(longTerms)[utf8.RuneCountInString(longTerms)-15:]) + `"}],` +
		`"buttons":[{"text":"続行","on":false}],"controls":{"n":3,"kinds":["mat-select","input:date","role:radio"]},` +
		`"outside":[{"text":"Get API key","on":true}]}`
	if got := mustJSON(t, s.Observe.Summary); got != wantS || len(s.Clicks) != 0 || !strings.HasSuffix(s.Observe.Summary.Boxes[0].Label, "同意します *") {
		t.Fatalf("S：结构摘要\n得到 %s\n期望 %s", got, wantS)
	}
	if structure := formatOnboardingStructure(s.Observe.Summary); structure != OnboardingStructureMarker+wantS {
		t.Fatalf("S：整理后的结构摘要应保留标签结尾、控件与范围外按钮\n得到 %s\n期望 %s", structure, OnboardingStructureMarker+wantS)
	}
}

// TestOnboardingPageExpressionReadOnly 识别表达式只读：不点击、不写值、不派发事件、不导航
func TestOnboardingPageExpressionReadOnly(t *testing.T) {
	for _, forbidden := range []string{".click(", "dispatchEvent", ".value =", "setter", "location.assign", "location.reload",
		"location.href =", "history.", "localStorage", "setAttribute", ".focus("} {
		if strings.Contains(onboardingPageExpression, forbidden) {
			t.Fatalf("识别表达式不应包含 %q", forbidden)
		}
	}
	// 一步式表达式只点勾选框与挑出的继续按钮：不导航、不写存储
	step := onboardingStepExpression(onboardingStepOptions{Click: true, Relaxed: true})
	for _, forbidden := range []string{"location.assign", "location.reload", "location.href =", "history.", "localStorage", "dispatchEvent"} {
		if strings.Contains(step, forbidden) {
			t.Fatalf("一步式表达式不应包含 %q", forbidden)
		}
	}
	if strings.Count(step, ".click()") != 2 {
		t.Fatalf("一步式表达式只应有勾选与继续两处点击")
	}
}

// TestFormatOnboardingStructure 结构摘要打码、“|”换成“¦”、单行、不超过上限，超长时先缩短标签再减少条目
func TestFormatOnboardingStructure(t *testing.T) {
	if got := formatOnboardingStructure(nil); got != "" {
		t.Fatalf("没有摘要时应为空串，得到 %q", got)
	}
	summary := &onboardingSummary{Scope: "ms-onboarding", BodyBoxes: 9, AllBoxes: 11,
		Controls: &onboardingControls{Count: 5, Kinds: []string{"mat-select", "input:date", "role:radio", "role:switch", "select"}}}
	for index := 0; index < 8; index++ {
		box := onboardingBox{Required: index%2 == 0, Disabled: index%2 == 1, Terms: true, Optional: index == 1, Hidden: index == 0, Kind: "input/mat-checkbox",
			Label: fmt.Sprintf("%d | someone@example.com %s 同意します *", index, strings.Repeat("長", 80))}
		if box.Required {
			box.By = "star"
		}
		summary.Boxes = append(summary.Boxes, box)
		summary.Buttons = append(summary.Buttons, onboardingButton{Text: fmt.Sprintf("按钮 %d %s", index, strings.Repeat("字", 50)), Enabled: true, Skip: "neg"})
		summary.Links = append(summary.Links, fmt.Sprintf("链接 %d %s", index, strings.Repeat("字", 50)))
		summary.Outside = append(summary.Outside, onboardingButton{Text: fmt.Sprintf("外 %d %s", index, strings.Repeat("字", 50)), Enabled: true, Skip: "area"})
	}
	got := formatOnboardingStructure(summary)
	if !strings.HasPrefix(got, OnboardingStructureMarker+"{") {
		t.Fatalf("结构摘要应以标记开头: %s", got)
	}
	text := strings.TrimPrefix(got, OnboardingStructureMarker)
	if utf8.RuneCountInString(text) > onboardingStructureLimit || strings.ContainsAny(text, "|\n") || strings.Contains(text, "someone") {
		t.Fatalf("结构摘要应打码、不含“|”与换行、不超过 %d 字: %s", onboardingStructureLimit, text)
	}
	var decoded onboardingSummary
	if err := json.Unmarshal([]byte(text), &decoded); err != nil {
		t.Fatalf("缩短后应仍是合法 JSON: %v: %s", err, text)
	}
	if len(decoded.Boxes) == 0 || len(decoded.Buttons) == 0 || !strings.HasPrefix(decoded.Boxes[0].Label, "0 ¦ so***") {
		t.Fatalf("缩短后应保留前面的条目: %+v", decoded)
	}
	if decoded.Scope != "ms-onboarding" || decoded.BodyBoxes != 9 || decoded.AllBoxes != 11 || decoded.Boxes[0].By != "star" || !decoded.Boxes[1].Disabled ||
		!decoded.Boxes[0].Terms || !decoded.Boxes[1].Optional || !decoded.Boxes[0].Hidden || decoded.Boxes[1].Kind != "input/mat-checkbox" ||
		decoded.Buttons[0].Skip != "neg" || decoded.Controls == nil || decoded.Controls.Count != 5 || len(decoded.Controls.Kinds) != 4 {
		t.Fatalf("缩减时应保留范围、勾选框数、表单控件计数与各条目的标记: %+v", decoded)
	}
	for _, box := range decoded.Boxes {
		if !strings.HasSuffix(box.Label, "*") || !strings.Contains(box.Label, "…") {
			t.Fatalf("缩短标签时应保留开头与结尾（结尾的“*”不丢）: %q", box.Label)
		}
	}
	// 不超长时链接照常列出（打码、截短）
	short := formatOnboardingStructure(&onboardingSummary{Scope: "body", Links: []string{"someone@example.com の利用規約", "b", "c", "d"}})
	if want := OnboardingStructureMarker + `{"scope":"body","bodyBoxes":0,"allBoxes":0,"boxes":[],"buttons":[],"links":["so***@*** の利用規約","b","c"]}`; short != want {
		t.Fatalf("链接\n得到 %s\n期望 %s", short, want)
	}
}

// fastOnboarding 把首次使用页的各项时限缩短到测试可接受的长度，测试结束时恢复
func fastOnboarding(t *testing.T) {
	t.Helper()
	saved := onboardingTiming
	onboardingTiming.total = 1500 * time.Millisecond
	onboardingTiming.poll = 5 * time.Millisecond
	onboardingTiming.settle = 10 * time.Millisecond
	onboardingTiming.retry = 40 * time.Millisecond
	onboardingTiming.relax = 60 * time.Millisecond
	onboardingTiming.stuck = 80 * time.Millisecond
	onboardingTiming.saved = 10 * time.Millisecond
	t.Cleanup(func() { onboardingTiming = saved })
}

// onboardingStepVariants 返回一步式表达式的各种参数组合，供假页面认出这条表达式与它的参数
func onboardingStepVariants() map[string]onboardingStepOptions {
	variants := make(map[string]onboardingStepOptions)
	for mask := 0; mask < 16; mask++ {
		options := onboardingStepOptions{Click: mask&1 != 0, Relaxed: mask&2 != 0, Clicked: mask&4 != 0, Observe: mask&8 != 0}
		variants[onboardingStepExpression(options)] = options
	}
	return variants
}

// onboardingReply 编码一步式表达式的假返回值（带一份固定的结构摘要）
func onboardingReply(state string, extra map[string]any) fakeEvaluate {
	reply := map[string]any{"state": state}
	if state != "left" {
		reply["summary"] = map[string]any{
			"scope": "ms-onboarding",
			"boxes": []any{
				map[string]any{"req": true, "on": state != "observe", "label": "* 私は 18 歳以上です"},
				map[string]any{"req": false, "on": false, "label": "最新情報をメールで受け取る"},
			},
			"buttons": []any{map[string]any{"text": "続行", "on": true}},
		}
	}
	for key, value := range extra {
		reply[key] = value
	}
	encoded, _ := json.Marshal(reply)
	return fakeEvaluate{value: string(encoded)}
}

// TestCompleteOnboardingLoop Go 侧轮询：勾选、点击、离开时完成并记下勾选数与按钮；整页跳转中的 evaluate 失败照常重试；
// 一直找不到按钮、一直禁用、页面脚本异常与 ctx 结束时以固定前缀失败，附页面快照与结构摘要
func TestCompleteOnboardingLoop(t *testing.T) {
	fastOnboarding(t)
	variants := onboardingStepVariants()
	navigationLost := fakeEvaluate{commandError: "no such frame"}
	run := func(t *testing.T, replies func(step int, options onboardingStepOptions) fakeEvaluate, ctx context.Context) (OnboardingResult, error, *fakeBiDiPage) {
		t.Helper()
		step := 0
		page := newFakeBiDiPage(t, func(expression string) fakeEvaluate {
			options, ok := variants[expression]
			if !ok {
				return fakeEvaluate{value: true}
			}
			step++
			return replies(step, options)
		})
		result, err := page.client.completeOnboarding(ctx, "ctx-1")
		return result, err, page
	}
	t.Run("完成", func(t *testing.T) {
		result, err, page := run(t, func(step int, options onboardingStepOptions) fakeEvaluate {
			switch step {
			case 1:
				return onboardingReply("checked", map[string]any{"label": "* 私は 18 歳以上です"})
			case 2:
				return onboardingReply("checked", map[string]any{"label": "Google 利用規約と Gemini API 追加利用規約に同意します（someone@example.com）", "relaxed": true})
			case 3:
				return onboardingReply("clicked", map[string]any{"button": "続行 | someone@example.com"})
			case 4:
				return navigationLost
			}
			return onboardingReply("left", nil)
		}, context.Background())
		if err != nil || result.Checked != 2 || result.Button != "続行 ¦ so***@***" || result.Elapsed <= 0 || !result.acted() {
			t.Fatalf("完成结果 = %+v, %v", result, err)
		}
		// 标签打码、保留开头 20 字与结尾 9 字，按放宽规则补勾的带“[补]”
		if want := []string{"* 私は 18 歳以上です", "[补]Google 利用規約と Gemini …o***@***）"}; !slices.Equal(result.Labels, want) {
			t.Fatalf("勾选标签\n得到 %q\n期望 %q", result.Labels, want)
		}
		if page.snapshots.Load() != 0 {
			t.Fatalf("完成时不应采集快照")
		}
	})
	t.Run("找不到按钮", func(t *testing.T) {
		_, err, page := run(t, func(int, onboardingStepOptions) fakeEvaluate {
			return onboardingReply("stuck", map[string]any{"reason": "找不到可点的继续按钮"})
		}, context.Background())
		if err == nil || !strings.HasPrefix(err.Error(), OnboardingFailedPrefix+": 找不到可点的继续按钮 | 页面={") ||
			!strings.Contains(err.Error(), OnboardingStructureMarker+`{"scope":"ms-onboarding"`) || page.snapshots.Load() != 1 {
			t.Fatalf("应以固定前缀失败并附快照与结构摘要: %v", err)
		}
		if !onboardingRelated(err) {
			t.Fatal("首次使用页失败应可识别")
		}
	})
	t.Run("按钮一直禁用", func(t *testing.T) {
		var relaxed bool
		_, err, _ := run(t, func(_ int, options onboardingStepOptions) fakeEvaluate {
			relaxed = relaxed || options.Relaxed
			return onboardingReply("waiting", map[string]any{"reason": "继续按钮未启用"})
		}, context.Background())
		if err == nil || !strings.HasPrefix(err.Error(), OnboardingFailedPrefix+": 等待超时（继续按钮未启用） | 页面=") || !relaxed {
			t.Fatalf("按钮一直禁用时应先放宽、到时限后失败: relaxed=%t %v", relaxed, err)
		}
	})
	t.Run("点击后一直不离开", func(t *testing.T) {
		clicks := 0
		_, err, _ := run(t, func(_ int, options onboardingStepOptions) fakeEvaluate {
			if !options.Click {
				return onboardingReply("waiting", map[string]any{"reason": "等待上一次点击生效"})
			}
			clicks++
			return onboardingReply("clicked", map[string]any{"button": "続行"})
		}, context.Background())
		want := fmt.Sprintf("%s: 点击继续 %d 次后仍停在首次使用页 | 页面=", OnboardingFailedPrefix, onboardingMaxButtonClicks)
		if err == nil || !strings.HasPrefix(err.Error(), want) || clicks != onboardingMaxButtonClicks {
			t.Fatalf("点击 %d 次后应失败: clicks=%d %v", onboardingMaxButtonClicks, clicks, err)
		}
	})
	t.Run("点过继续仍停在首次使用页时放宽", func(t *testing.T) {
		// 按钮一直启用：勾上必选项、点了继续仍不离开；距最后一次勾选超过 relax、距上一次点击超过 retry 时才带 Relaxed 与 Clicked
		var relaxedChecks, clicks int
		result, err, _ := run(t, func(step int, options onboardingStepOptions) fakeEvaluate {
			switch {
			case step == 1:
				return onboardingReply("checked", map[string]any{"label": "* 私は 18 歳以上です"})
			case relaxedChecks > 0 && options.Click:
				return onboardingReply("left", nil)
			case options.Relaxed && options.Clicked:
				relaxedChecks++
				return onboardingReply("checked", map[string]any{"label": "Google 利用規約に同意します", "relaxed": true})
			case options.Click:
				clicks++
				return onboardingReply("clicked", map[string]any{"button": "続行"})
			}
			return onboardingReply("waiting", map[string]any{"reason": "等待上一次点击生效"})
		}, context.Background())
		if err != nil || relaxedChecks != 1 || clicks == 0 || result.Checked != 2 {
			t.Fatalf("点过继续之后应放宽一次: relaxed=%d clicks=%d result=%+v err=%v", relaxedChecks, clicks, result, err)
		}
	})
	t.Run("离开后等保存请求", func(t *testing.T) {
		onboardingTiming.saved = 300 * time.Millisecond
		defer func() { onboardingTiming.saved = 10 * time.Millisecond }()
		started := time.Now()
		result, err, _ := run(t, func(step int, _ onboardingStepOptions) fakeEvaluate {
			if step == 1 {
				return onboardingReply("clicked", map[string]any{"button": "続行"})
			}
			return onboardingReply("left", nil)
		}, context.Background())
		if elapsed := time.Since(started); err != nil || !result.acted() || elapsed < 300*time.Millisecond || result.Elapsed >= 300*time.Millisecond {
			t.Fatalf("点击过时离开后应再等 saved（耗时不含这段）: elapsed=%s result=%+v err=%v", elapsed, result, err)
		}
		started = time.Now()
		result, err, _ = run(t, func(int, onboardingStepOptions) fakeEvaluate { return onboardingReply("left", nil) }, context.Background())
		if elapsed := time.Since(started); err != nil || result.acted() || elapsed >= 200*time.Millisecond {
			t.Fatalf("页面自己离开时不等: elapsed=%s result=%+v err=%v", elapsed, result, err)
		}
	})
	t.Run("页面脚本异常", func(t *testing.T) {
		_, err, _ := run(t, func(int, onboardingStepOptions) fakeEvaluate {
			return fakeEvaluate{exception: "checkVisibility is not a function"}
		}, context.Background())
		if err == nil || !strings.HasPrefix(err.Error(), OnboardingFailedPrefix+": 页面脚本失败: 页面表达式异常: ") {
			t.Fatalf("页面脚本异常应立即失败: %v", err)
		}
	})
	t.Run("ctx 到期", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		_, err, page := run(t, func(int, onboardingStepOptions) fakeEvaluate {
			return onboardingReply("waiting", map[string]any{"reason": "继续按钮未启用"})
		}, ctx)
		if err == nil || !strings.HasPrefix(err.Error(), OnboardingFailedPrefix+": context deadline exceeded | 页面={") ||
			!errors.Is(err, context.DeadlineExceeded) || page.snapshots.Load() != 1 {
			t.Fatalf("ctx 到期时应以固定前缀失败、保留错误链，快照用独立的短上下文: %v", err)
		}
	})
}
