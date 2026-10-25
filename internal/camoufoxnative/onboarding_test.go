package camoufoxnative

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

// fakeOnboardingDOMLib 为 node 里执行首次使用页表达式的最小 DOM 替身：元素带属性、文字、可见性（hidden 属性与 style 里的
// display:none 为 display:none，display:contents 的元素自己 checkVisibility 为假，style 里的 visibility 往下继承）、
// getComputedStyle（只有 display 与 visibility）、位置
// （rect，没有时为零尺寸）、事件与一个只支持标签、类、属性（=、*=）、:disabled 与后代组合的选择器引擎；勾选框 click 时切换状态
// 并派发 click、input、change。被点击的元素记在 clicks（data-name）
const fakeOnboardingDOMLib = `
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
  getBoundingClientRect() { return this.rect || {left: 0, right: 0, top: 0, bottom: 0, width: 0, height: 0}; }
  get disabled() { return this.hasAttribute('disabled'); }
  set disabled(value) { if (value) this.setAttribute('disabled', ''); else this.removeAttribute('disabled'); }
  get required() { return this.hasAttribute('required'); }
  get display() { return this.hasAttribute('hidden') ? 'none' : (/display:\s*([\w-]+)/.exec(this.getAttribute('style') || '') || [])[1] || 'inline'; }
  get visibility() {
    for (let node = this; node; node = node.parentElement) {
      const value = (/visibility:\s*([\w-]+)/.exec(node.getAttribute('style') || '') || [])[1];
      if (value) return value;
    }
    return 'visible';
  }
  checkVisibility() {
    if (this.display === 'contents' || this.visibility !== 'visible') return false;
    for (let node = this; node; node = node.parentElement) if (node.display === 'none') return false;
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
globalThis.getComputedStyle = element => ({display: element.display, visibility: element.visibility});
// append 在页面已挂载后追加子元素（模拟 Angular 稍后才渲染出来），prepend 插到最前面
const append = (parent, ...children) => {
  for (const child of children) {
    child.parentElement = parent;
    parent.childNodes.push(child);
  }
};
const prepend = (parent, child) => {
  child.parentElement = parent;
  parent.childNodes.unshift(child);
};
// replace 换掉 parent 的全部子元素（模拟多步向导换到下一步）
const replace = (parent, ...children) => {
  for (const child of parent.children) child.parentElement = null;
  parent.childNodes = [];
  append(parent, ...children);
};
// at 给元素一个位置（宽 100、高 30）：多步向导把其余各步平移到视口（宽 1280）之外
const at = (element, left) => {
  element.rect = {left, right: left + 100, top: 0, bottom: 30, width: 100, height: 30};
  return element;
};
// mount 挂载一页：body 为 null 时模拟新文档还在解析 head
const mount = (body, pathname, readyState = 'complete') => {
  currentHTML = body ? h('html', {}, body) : h('html', {}, h('head', {}));
  globalThis.innerWidth = 1280;
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
// route 让按钮点击后像 SPA 一样离开首次使用页
const route = (button, onboarding) => button.addEventListener('click', () => {
  location.pathname = '/prompts/new_chat';
  onboarding.setAttribute('hidden', '');
});
const box = (input, ...label) => h('mat-checkbox', {class: 'mat-mdc-checkbox'},
  h('div', {class: 'mdc-form-field'}, h('div', {class: 'mdc-checkbox'}, input), h('label', {for: input.id}, ...label)));
const icon = name => h('mat-icon', {class: 'material-symbols-outlined'}, name);
// surveyTexts 为用途调查一步的界面文字（照线上中文与英文界面）
const surveyTexts = {
  zh: {question: '是什么原因让您来到 Google AI Studio？', options: ['探索最新模型', '使用 Gemini API 开发', '开展氛围编程，让创意跃然屏上', '与 Gemini 对话'],
    skip: '跳过', proceed: '继续', card: 'Gemini 我们最智能的模型，可助您将任何创意变为现实', slide: n => '前往第 ' + n + ' 张幻灯片：Gemini', main: 'Skip to main content'},
  en: {question: 'What brings you to Google AI Studio?', options: ['Explore latest models', 'Develop with Gemini API', 'Vibe code and bring your ideas to life', 'Chat with Gemini'],
    skip: 'Skip', proceed: 'Continue', card: 'Gemini Our most intelligent model, helping you bring any idea to life', slide: n => 'Go to slide ' + n + ': Gemini', main: 'Skip to main content'},
};
// surveyStep 搭用途调查一步（照线上结构）：ms-intent-selection 里四个 role=checkbox 的 button 选项（带 add_circle 图标，点了切换
// aria-checked 并启用继续）；ms-horizontal-stepper 里“跳过”可点、“继续 arrow_forward”在选一个用途前禁用；ms-stacked-cards-preview 里是
// 模型卡片与“前往第 N 张幻灯片”轮播按钮（中文界面在 nav 里，英文界面为 role=tab）；另有 ms-outro。返回各部分供场景接线
const surveyStep = (lang, prefix = '') => {
  const texts = surveyTexts[lang];
  const proceed = h('button', {disabled: '', class: 'ms-button-primary', 'data-name': prefix + 'proceed'}, texts.proceed, icon('arrow_forward'));
  const options = texts.options.map((text, index) => {
    const option = h('button', {role: 'checkbox', 'aria-checked': 'false', 'data-name': prefix + 'intent-' + index}, text, icon('add_circle'));
    option.addEventListener('click', () => {
      option.setAttribute('aria-checked', option.getAttribute('aria-checked') === 'true' ? 'false' : 'true');
      proceed.disabled = !options.some(item => item.getAttribute('aria-checked') === 'true');
    });
    return option;
  });
  const skip = h('button', {class: 'ms-button-text', 'data-name': prefix + 'skip'}, texts.skip);
  const slides = [1, 2].map(n => lang === 'zh'
    ? h('button', {'aria-label': texts.slide(n), 'data-name': prefix + 'slide-' + n}, h('span', {class: 'dot'}))
    : h('button', {role: 'tab', 'aria-label': texts.slide(n), 'data-name': prefix + 'slide-' + n}, h('span', {class: 'dot'})));
  const card = h('div', {role: 'button', class: 'card', 'data-name': prefix + 'card'}, icon('spark'), texts.card);
  const parts = [
    h('ms-intent-selection', {}, h('h1', {}, texts.question), h('div', {class: 'options'}, ...options)),
    h('ms-horizontal-stepper', {}, h('div', {class: 'step-dots'}), skip, proceed),
    h('ms-stacked-cards-preview', {}, card, lang === 'zh' ? h('nav', {}, ...slides) : h('div', {role: 'tablist'}, ...slides)),
    h('ms-outro', {}, h('p', {}, '')),
  ];
  return {parts, options, skip, proceed, card, slides};
};
// skipToMain 为页面开头的无障碍跳转按钮（线上在作用域外）
const skipToMain = text => h('button', {class: 'skip-to-main', 'data-name': 'skip-main'}, text);
`

// fakeOnboardingDOM 在 fakeOnboardingDOMLib 上逐个场景执行首次使用页的表达式：每个场景搭一页，按顺序执行若干步，
// 记下每一步的返回值与页面上被点击的元素
const fakeOnboardingDOM = `
const fs = require('fs');
const expressions = JSON.parse(fs.readFileSync(process.argv[2], 'utf8'));
` + fakeOnboardingDOMLib + `
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
// IS：用途调查一步（中文与英文界面，照线上结构，见 surveyStep）：作用域外有“Skip to main content”、全局横幅与（英文界面）Cookie 横幅
// “Agree”，作用域里“跳过”前面另有一个“跳到主要内容”/“Skip to main content”。没有必选项与条款、年龄勾选框，不涉及同意：不带 skip 时
// 等待并报告跳过按钮，带 skip 时点“跳过”；用途选项、卡片、轮播、无障碍跳转与 Cookie 横幅都不点
{
  results.IS = {};
  for (const lang of ['zh', 'en']) {
    const step = surveyStep(lang);
    const onboarding = h('ms-onboarding', {}, ...step.parts);
    prepend(step.parts[1], h('button', {'data-name': 'main-inside'}, lang === 'zh' ? '跳到主要内容' : 'Skip to main content'));
    route(step.skip, onboarding);
    const page = [skipToMain(surveyTexts[lang].main), h('ms-app', {}, h('ms-global-banner', {}, h('button', {'data-name': 'banner'}, 'Try Gemini 3')), onboarding)];
    if (lang === 'en') page.push(cookieBar('Agree'));
    mount(h('body', {}, ...page), '/onboarding');
    const observe = run('observe');
    const replies = steps(['step', 'step', 'skip', 'skip']);
    results.IS[lang] = {observe, replies, clicks, checked: step.options.map(option => option.getAttribute('aria-checked'))};
  }
}

// TS：服务条款一步里同时有可点的“跳过”：涉及同意，带 skip 也不点跳过，只勾必选项后点继续（邮件项不勾）
{
  const age = h('input', {type: 'checkbox', id: 'age', 'data-name': 'age'});
  const mail = h('input', {type: 'checkbox', id: 'mail', 'data-name': 'mail'});
  const skip = h('button', {'data-name': 'skip'}, '跳过');
  const proceed = h('button', {type: 'submit', disabled: '', 'data-name': 'proceed'}, '继续', icon('arrow_forward'));
  age.addEventListener('change', () => { proceed.disabled = !age.checked; });
  const onboarding = h('ms-onboarding', {}, h('ms-tos-disclaimer', {}, h('h1', {}, '欢迎使用 Google AI Studio'),
    box(age, '* 我已年满 18 周岁'), box(mail, '通过电子邮件接收 Google AI Studio 的最新资讯'), skip, proceed), h('ms-eclipse', {}));
  route(proceed, onboarding);
  route(skip, onboarding);
  mount(h('body', {}, h('ms-app', {}, onboarding)), '/onboarding');
  results.TS = {replies: steps(['skip', 'skip', 'skip']), clicks, mail: mail.checked};
}

// TS2：继续按钮一直禁用、有可点的“Skip”，这一步涉及同意：必选项已勾上（checkedRequired）、勾选框没有必选标记但标签含服务条款关键词
// （termsOnly）、可见的 ms-tos-disclaimer 里没有勾选框（disclaimer）：都不点跳过、不报告跳过按钮
{
  results.TS2 = {};
  const variants = {
    checkedRequired: () => [box(h('input', {type: 'checkbox', id: 'age', required: '', checked: '', 'data-name': 'age'}), 'I am 18 years or older')],
    termsOnly: () => [box(h('input', {type: 'checkbox', id: 'terms', checked: '', 'data-name': 'terms'}), 'I accept the Gemini API Additional Terms of Service')],
    disclaimer: () => [h('ms-tos-disclaimer', {}, h('p', {}, 'By continuing you agree to the Gemini API Additional Terms of Service'))],
  };
  for (const [name, build] of Object.entries(variants)) {
    const onboarding = h('ms-onboarding', {}, ...build(), h('button', {'data-name': 'skip'}, 'Skip'),
      h('button', {disabled: '', 'data-name': 'proceed'}, 'Continue'));
    mount(h('body', {}, h('ms-app', {}, onboarding)), '/onboarding');
    results.TS2[name] = {replies: steps(['skip', 'skip']), clicks};
  }
}

// BD：不涉及同意的一步里继续按钮刚渲染时禁用：等待并报告跳过按钮；随即变为可点，之后即使允许跳过也点继续、不点跳过
{
  const step = surveyStep('zh');
  const onboarding = h('ms-onboarding', {}, ...step.parts);
  route(step.proceed, onboarding);
  route(step.skip, onboarding);
  mount(h('body', {}, h('ms-app', {}, onboarding)), '/onboarding');
  const first = run('step');
  step.proceed.disabled = false;
  results.BD = {first, replies: steps(['skip', 'skip']), clicks};
}

// HZ：横向多步向导把各步都渲染出来、其余各步平移到视口之外（见 at），结尾页在 aria-hidden 容器里：当前一步（用途调查）的继续禁用时
// 点当前一步的跳过；跳过后各步平移、下一步的继续进到视口里，点它（上一步禁用的继续排在前面、也是主按钮样式，不选），结尾页的“开始使用”不点
{
  const survey = surveyStep('zh');
  const nextSkip = at(h('button', {'data-name': 'next-skip'}, '跳过'), 1280);
  const nextProceed = at(h('button', {'data-name': 'next-proceed'}, '继续'), 1380);
  const start = h('button', {'mat-flat-button': '', 'data-name': 'start'}, '开始使用');
  const question = survey.parts[0].querySelector('h1');
  at(survey.skip, 100);
  at(survey.proceed, 200);
  at(question, 100);
  survey.options.forEach((item, index) => at(item, 100 + index * 100));
  const onboarding = h('ms-onboarding', {}, h('div', {class: 'step'}, ...survey.parts.slice(0, 2)),
    h('div', {class: 'step'}, h('ms-model-intro', {}, nextSkip, nextProceed)),
    h('div', {class: 'step', 'aria-hidden': 'true'}, h('ms-outro', {}, start)));
  survey.skip.addEventListener('click', () => {
    at(survey.skip, -1180);
    at(survey.proceed, -1080);
    at(question, -1180);
    survey.options.forEach(item => at(item, -1180));
    at(nextSkip, 100);
    at(nextProceed, 200);
  });
  route(nextProceed, onboarding);
  mount(h('body', {}, h('ms-app', {}, onboarding)), '/onboarding');
  results.HZ = {replies: steps(['skip', 'skip', 'skip']), clicks};
}

// CD：介绍卡片一步只有模型卡片（role=button，不含继续类关键词）与轮播按钮、没有继续与跳过：卡片与轮播不当作唯一剩下的按钮，返回 stuck；
// 同一组件里文字含继续类关键词的“继续”照常参与挑选（CD2）
{
  const cards = withProceed => {
    const proceed = h('button', {'data-name': 'proceed'}, '继续');
    const preview = h('ms-stacked-cards-preview', {}, h('div', {role: 'button', 'data-name': 'card'}, icon('spark'), 'Gemini 我们最智能的模型'),
      h('div', {role: 'tablist'}, h('button', {role: 'tab', 'aria-label': '前往第 2 张幻灯片：Veo', 'data-name': 'slide'})));
    if (withProceed) append(preview, proceed);
    const onboarding = h('ms-onboarding', {}, preview);
    route(proceed, onboarding);
    mount(h('body', {}, h('ms-app', {}, onboarding)), '/onboarding');
  };
  cards(false);
  results.CD = {reply: run('skip'), clicks};
  cards(true);
  results.CD2 = {replies: steps(['skip', 'skip']), clicks};
}

// option 为一个用途调查选项（button[role=checkbox]，带 add_circle 图标）；footer 为步进器页脚：跳过按钮与禁用的继续按钮
const option = text => h('button', {role: 'checkbox', 'aria-checked': 'false', 'data-name': 'intent'}, text, icon('add_circle'));
const footer = (skipText = 'Skip', proceedText = 'Continue') => h('ms-horizontal-stepper', {},
  h('button', {'data-name': 'skip'}, skipText), h('button', {disabled: '', 'data-name': 'proceed'}, proceedText, icon('arrow_forward')));

// CS：条款一步认不全时也算涉及同意，带 skip 也不点跳过、不报告跳过按钮；每种另加一个可见的用途调查组件（plus：有用途调查的证据，
// 只靠涉及同意的判定挡住）。korean 为韩文条款标签（没有星号）；policy 为没有星号、不含 terms 与 18 的英文条款标签（原生 input 的
// mat-checkbox，标签认不出也算）；hiddenTos 为 DOM 里不可见（hidden）的 ms-tos-disclaimer 里有未勾选的“* I am 18 or older”；
// hiddenContext 为不可见的条款组件里勾选框没有关联的标签、文字是兄弟元素；contentsTos 为宿主 display:contents（checkVisibility 为假）、
// 只有说明文字的 ms-tos-disclaimer；koreanOnly 为韩文条款一步只有“건너뛰기”（跳过类词，不当作唯一剩下的按钮点）。
// 条款勾选框写成 button[role=checkbox]（用途选项也是这种写法）、没有服务条款组件时：roleButtonPolicy 为标签是按钮自己的文字、没有星号、
// 不含 terms 与 18，不在 ms-intent-selection 里；roleButtonIcon 为按钮里只有图标、“* I am 18 or older”是兄弟元素，同一容器里另有
// 邮件勾选框（容器文字取不到，标签为空）；roleButtonLabelledby 为 aria-labelledby 指向的标签还没渲染出来（往上只取得到页脚文字）；
// checkedRoleAge 为已勾上的“I am 18 years of age or older *”与一个用途选项都在 ms-intent-selection 里（都算用途调查选项，
// 只靠“当前一步里可见、已勾的必选或条款勾选框”挡住）；intentUnlabelled 为 ms-intent-selection 里只有图标、aria-labelledby 指向的标签
// 还没渲染出来的 button[role=checkbox]（往上取到的页脚文字不算按钮自己的标签，不算用途调查选项）
{
  results.CS = {};
  const roleBox = (name, content, extra = {}) => h('button', Object.assign({role: 'checkbox', 'aria-checked': 'false', 'data-name': name}, extra), content);
  const variants = {
    korean: () => [box(h('input', {type: 'checkbox', id: 'tos', 'data-name': 'tos'}), '서비스 약관에 동의합니다'), footer('건너뛰기', '계속')],
    policy: () => [box(h('input', {type: 'checkbox', id: 'tos', 'data-name': 'tos'}), 'I accept the Generative AI Prohibited Use Policy'), footer()],
    hiddenTos: () => [h('ms-tos-disclaimer', {hidden: ''}, box(h('input', {type: 'checkbox', id: 'age', 'data-name': 'age'}), '* I am 18 or older')), footer()],
    hiddenContext: () => [h('ms-tos-disclaimer', {style: 'display: none'},
      h('div', {}, h('input', {type: 'checkbox', 'data-name': 'age'}), h('span', {}, 'I confirm that I am 18 or older'))), footer()],
    contentsTos: () => [h('ms-tos-disclaimer', {style: 'display: contents'}, h('p', {}, 'By continuing you agree to the Gemini API Additional Terms of Service')), footer()],
    koreanOnly: () => [box(h('input', {type: 'checkbox', id: 'tos', 'data-name': 'tos'}), '서비스 약관에 동의합니다'),
      h('ms-horizontal-stepper', {}, h('button', {'data-name': 'skip'}, '건너뛰기'))],
    roleButtonPolicy: () => [h('h1', {}, 'Welcome'), roleBox('tos', 'I accept the Generative AI Prohibited Use Policy'), footer()],
    roleButtonIcon: () => [h('div', {}, roleBox('age', icon('check_box_outline_blank')), h('span', {}, '* I am 18 or older'),
      roleBox('mail', icon('check_box_outline_blank')), h('span', {}, 'Email me updates')), footer()],
    roleButtonLabelledby: () => [h('div', {}, roleBox('age', icon('check_box_outline_blank'), {'aria-labelledby': 'age-label'})), footer()],
    checkedRoleAge: () => [h('ms-intent-selection', {}, option('Explore latest models'),
      roleBox('age', 'I am 18 years of age or older *', {'aria-checked': 'true'})), footer()],
    intentUnlabelled: () => [h('ms-intent-selection', {}, h('div', {}, roleBox('age', icon('check_box_outline_blank'), {'aria-labelledby': 'age-label'}))), footer()],
  };
  for (const [name, build] of Object.entries(variants)) {
    for (const plus of [false, true]) {
      const onboarding = h('ms-onboarding', {}, ...(plus ? [h('ms-intent-selection', {}, option('Explore latest models'))] : []), ...build());
      mount(h('body', {}, h('ms-app', {}, onboarding)), '/onboarding');
      results.CS[name + (plus ? '+survey' : '')] = {replies: steps(['skip', 'skip']), clicks};
    }
  }
}

// FT：只有步进器页脚（跳过与禁用的继续）、步骤内容还没渲染出来：没有用途调查的证据，不报告跳过（条款内容可能晚于页脚渲染）；
// 用途调查组件渲染出来之后照常报告并点跳过
{
  const onboarding = h('ms-onboarding', {}, h('h1', {}, 'Welcome to Google AI Studio'), footer());
  route(onboarding.querySelector('[data-name=skip]'), onboarding);
  mount(h('body', {}, h('ms-app', {}, onboarding)), '/onboarding');
  const before = run('skip');
  prepend(onboarding, h('ms-intent-selection', {}, option('Explore latest models'), option('Chat with Gemini')));
  results.FT = {before, replies: steps(['step', 'skip', 'skip']), clicks};
}

// IN：用途调查组件里的选项不是 button[role=checkbox]（aria-pressed 的按钮与 div[role=button] 的卡片）：只靠可见的 ms-intent-selection
// 认出是用途调查，照常报告并点跳过，选项不点
{
  const onboarding = h('ms-onboarding', {}, h('ms-intent-selection', {}, h('h1', {}, 'What brings you to Google AI Studio?'),
    h('button', {'aria-pressed': 'false', 'data-name': 'intent'}, 'Explore latest models', icon('add_circle')),
    h('div', {role: 'button', 'data-name': 'intent'}, 'Chat with Gemini', icon('add_circle'))), footer());
  route(onboarding.querySelector('[data-name=skip]'), onboarding);
  mount(h('body', {}, h('ms-app', {}, onboarding)), '/onboarding');
  results.IN = {replies: steps(['step', 'skip', 'skip']), clicks};
}

// HD：介绍卡片与结尾页的继续按钮都在共用的页脚（ms-horizontal-stepper）里、作用域里另有直接的标题：cur 另接当前一步第一个在 ms- 组件里的
// 标题所在的组件，分得出停在哪一步
{
  results.HD = {};
  const contents = {
    cards: () => h('ms-stacked-cards-preview', {}, h('h2', {}, '认识 Gemini 模型'), h('div', {role: 'button', 'data-name': 'card'}, icon('spark'), 'Gemini 我们最智能的模型')),
    outro: () => h('ms-outro', {}, h('h2', {}, '一切就绪'), h('p', {}, '开始构建吧')),
  };
  for (const [name, build] of Object.entries(contents)) {
    const onboarding = h('ms-onboarding', {}, h('h1', {}, 'Google AI Studio'), build(),
      h('ms-horizontal-stepper', {}, h('button', {disabled: '', 'data-name': 'proceed'}, '继续', icon('arrow_forward'))));
    mount(h('body', {}, h('ms-app', {}, onboarding)), '/onboarding');
    results.HD[name] = {observe: run('observe'), clicks};
  }
}

// SA：跳过按钮只在作用域里当前一步找：当前一步没有跳过按钮、aria-hidden 的另一步里有“Skip”（hiddenStep）；ms-onboarding 在对话框里、
// 作用域里没有继续按钮，对话框里作用域外有“Skip”（dialog）：都不点、不报告
{
  results.SA = {};
  {
    const onboarding = h('ms-onboarding', {},
      h('div', {class: 'step'}, h('ms-intent-selection', {}, option('Explore latest models')), h('button', {disabled: '', 'data-name': 'proceed'}, 'Continue')),
      h('div', {class: 'step', 'aria-hidden': 'true'}, h('ms-model-intro', {}, h('button', {'data-name': 'hidden-skip'}, 'Skip'))));
    mount(h('body', {}, h('ms-app', {}, onboarding)), '/onboarding');
    results.SA.hiddenStep = {replies: steps(['skip', 'skip']), clicks};
  }
  {
    const onboarding = h('ms-onboarding', {}, h('ms-intent-selection', {}, option('Explore latest models')));
    const dialog = h('mat-dialog-container', {}, onboarding, h('div', {class: 'actions'}, h('button', {'data-name': 'dialog-skip'}, 'Skip')));
    mount(h('body', {}, h('ms-app', {}, dialog)), '/onboarding');
    results.SA.dialog = {replies: steps(['skip', 'skip']), clicks};
  }
}

// SK：用途调查一步里作用域内的各种“跳过”都不算跳过按钮：链接（a[href][role=button]、a[href] 里的 button）、页头与导航里的、标签里的、
// Cookie 区域里的、菜单类、禁用的、文字为 Skip 但 aria-label 为 Skip to main content 的；plain 为普通的“Skip”按钮（对照：照常点）。
// 作用域退到整页（body：没有首次使用组件）、整页载入未完成（loading）时不点
{
  results.SK = {};
  const variants = {
    link: () => h('a', {href: '#skip', role: 'button', 'data-name': 'x'}, 'Skip'),
    buttonInLink: () => h('a', {href: '#skip'}, h('button', {'data-name': 'x'}, 'Skip')),
    header: () => h('header', {}, h('button', {'data-name': 'x'}, '跳过')),
    nav: () => h('nav', {}, h('button', {'data-name': 'x'}, '跳过')),
    label: () => h('label', {}, h('button', {'data-name': 'x'}, 'Skip')),
    cookie: () => h('div', {class: 'cookie-consent'}, h('button', {'data-name': 'x'}, 'Skip')),
    popup: () => h('button', {'aria-haspopup': 'menu', 'data-name': 'x'}, 'Skip'),
    disabled: () => h('button', {disabled: '', 'data-name': 'x'}, '跳过'),
    mainLabel: () => h('button', {'aria-label': 'Skip to main content', 'data-name': 'x'}, 'Skip'),
    preview: () => h('ms-stacked-cards-preview', {}, h('button', {'data-name': 'x'}, 'Skip')),
    tablist: () => h('div', {role: 'tablist'}, h('button', {'data-name': 'x'}, '跳过')),
    carousel: () => h('div', {'aria-roledescription': 'carousel'}, h('button', {'data-name': 'x'}, 'スキップ')),
    plain: () => h('button', {'data-name': 'x'}, 'Skip'),
  };
  const survey = () => h('ms-intent-selection', {}, option('Explore latest models'), option('Chat with Gemini'));
  const proceed = () => h('button', {disabled: '', 'data-name': 'proceed'}, 'Continue', icon('arrow_forward'));
  for (const [name, build] of Object.entries(variants)) {
    mount(h('body', {}, h('ms-app', {}, h('ms-onboarding', {}, survey(), h('div', {class: 'footer'}, build(), proceed())))), '/onboarding');
    results.SK[name] = {replies: steps(['skip']), clicks};
  }
  mount(h('body', {}, h('main', {}, survey(), h('button', {'data-name': 'x'}, 'Skip'), proceed())), '/onboarding');
  results.SK.body = {replies: steps(['skip']), clicks};
  mount(h('body', {}, h('ms-app', {}, h('ms-onboarding', {}, survey(), h('div', {class: 'footer'}, h('button', {'data-name': 'x'}, 'Skip'), proceed())))),
    '/onboarding', 'interactive');
  results.SK.loading = {replies: steps(['skip']), clicks};
  // previewFirst：卡片预览里的“Skip”（如跳过卡片动画）在 DOM 上排在页脚的“Skip”前面：点页脚的
  mount(h('body', {}, h('ms-app', {}, h('ms-onboarding', {}, survey(), h('ms-stacked-cards-preview', {}, h('button', {'data-name': 'preview-skip'}, 'Skip')),
    h('ms-horizontal-stepper', {}, h('button', {'data-name': 'x'}, 'Skip'), proceed())))), '/onboarding');
  results.SK.previewFirst = {replies: steps(['skip']), clicks};
}

// CT：用途调查一步里卡片预览的卡片文字含继续类关键词（div[role=button] 的“Veo Try our video model”、button 的“Try Veo”），在 DOM 上
// 排在不是主按钮样式的禁用“Continue”前面：卡片不点，报告并点跳过
{
  results.CT = {};
  const cards = {
    div: () => h('div', {role: 'button', class: 'card', 'data-name': 'card'}, icon('movie'), 'Veo Try our video model'),
    button: () => h('button', {class: 'card', 'data-name': 'card'}, 'Try Veo'),
  };
  for (const [name, build] of Object.entries(cards)) {
    const onboarding = h('ms-onboarding', {}, h('ms-intent-selection', {}, option('Explore latest models')),
      h('ms-stacked-cards-preview', {}, build()), footer());
    route(onboarding.querySelector('[data-name=skip]'), onboarding);
    mount(h('body', {}, h('ms-app', {}, onboarding)), '/onboarding');
    results.CT[name] = {replies: steps(['step', 'skip', 'skip']), clicks};
  }
}

// HO：横向多步向导把各步都渲染出来，用途调查与介绍卡片两步已平移到视口左边（继续禁用、跳过可点），结尾页在视口里：只有不含关键词的
// “前往 AI Studio”（goButton：作为唯一剩下的按钮点它），或只有链接样式的“开始使用”与关闭按钮（linkOnly：找不到可点的继续按钮）。
// 都不点上一步的继续与跳过；结构摘要先列当前一步的按钮，其余各步的带 off
{
  results.HO = {};
  for (const variant of ['goButton', 'linkOnly']) {
    const survey = surveyStep('zh', 'survey-');
    survey.options.forEach(item => at(item, -1180));
    at(survey.parts[0].querySelector('h1'), -1180);
    at(survey.skip, -1180);
    at(survey.proceed, -1080);
    at(survey.card, -900);
    survey.slides.forEach(item => at(item, -900));
    const intro = h('ms-model-intro', {}, at(h('button', {'data-name': 'intro-skip'}, '跳过'), -600),
      at(h('button', {disabled: '', 'data-name': 'intro-proceed'}, '继续'), -500));
    const outro = variant === 'goButton'
      ? h('ms-outro', {}, at(h('button', {'mat-flat-button': '', 'data-name': 'go'}, '前往 AI Studio'), 100))
      : h('ms-outro', {}, at(h('button', {'aria-label': '关闭', 'data-name': 'close'}, icon('close')), 50),
        at(h('a', {href: '/prompts/new_chat', 'mat-flat-button': '', 'data-name': 'start-link'}, '开始使用'), 100));
    const onboarding = h('ms-onboarding', {}, h('div', {class: 'step'}, ...survey.parts.slice(0, 2)), h('div', {class: 'step'}, intro), h('div', {class: 'step'}, outro));
    if (variant === 'goButton') route(outro.querySelector('[data-name=go]'), onboarding);
    mount(h('body', {}, h('ms-app', {}, onboarding)), '/onboarding');
    results.HO[variant] = {replies: steps(['skip', 'skip']), clicks};
  }
}

// TO：横向多步向导里条款一步留在 DOM、已平移到视口外，当前一步是用途调查：条款一步的必选项已勾上（checked）时这一步不涉及同意，
// 照常点跳过；条款一步还有未勾选的条款项（unchecked：没有必选标记、标签含 Terms）时不点跳过
{
  results.TO = {};
  for (const variant of ['checked', 'unchecked']) {
    const input = variant === 'checked'
      ? at(h('input', {type: 'checkbox', id: 'age', checked: '', required: '', 'data-name': 'age'}), -1180)
      : at(h('input', {type: 'checkbox', id: 'terms', 'data-name': 'terms'}), -1180);
    const tos = at(h('ms-tos-disclaimer', {}, box(input, variant === 'checked' ? '* I am 18 or older' : 'I accept the Gemini API Additional Terms of Service')), -1280);
    const survey = surveyStep('en');
    survey.options.forEach((item, index) => at(item, 100 + index * 100));
    at(survey.skip, 100);
    at(survey.proceed, 200);
    const onboarding = h('ms-onboarding', {}, h('div', {class: 'step'}, tos), h('div', {class: 'step'}, ...survey.parts.slice(0, 2)));
    route(survey.skip, onboarding);
    mount(h('body', {}, h('ms-app', {}, onboarding)), '/onboarding');
    results.TO[variant] = {replies: steps(['skip', 'skip']), clicks};
  }
}

// TK：上一步的条款组件留在 DOM 里、已隐藏（keptHidden：hidden，里面的必选项已勾上；keptInvisible：visibility:hidden，只有说明文字），
// 当前一步是用途调查：不算涉及同意，照常点跳过
{
  results.TK = {};
  const kept = {
    keptHidden: () => h('ms-tos-disclaimer', {hidden: ''}, box(h('input', {type: 'checkbox', id: 'age', checked: '', 'data-name': 'age'}), '* I am 18 or older')),
    keptInvisible: () => h('ms-tos-disclaimer', {style: 'visibility: hidden'}, h('p', {}, 'By continuing you agree to the Gemini API Additional Terms of Service')),
  };
  for (const [name, build] of Object.entries(kept)) {
    const survey = surveyStep('en');
    const onboarding = h('ms-onboarding', {}, build(), ...survey.parts);
    route(survey.skip, onboarding);
    mount(h('body', {}, h('ms-app', {}, onboarding)), '/onboarding');
    results.TK[name] = {replies: steps(['skip', 'skip']), clicks};
  }
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
		"skip":           onboardingStepExpression(onboardingStepOptions{Click: true, Skip: true}),
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
	wantSummary := `{"scope":"ms-onboarding","cur":["ms-tos-disclaimer","ms-onboarding"],"parts":["ms-tos-disclaimer"],"bodyBoxes":4,"allBoxes":4,"boxes":[` +
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
	wantStructure := ` | 结构={"scope":"ms-onboarding","cur":["ms-onboarding"],"bodyBoxes":1,"allBoxes":1,` +
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
	wantL := `{"scope":"ms-tos-disclaimer","cur":["ms-tos-disclaimer"],"bodyBoxes":2,"allBoxes":2,"boxes":[` +
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
	wantS := `{"scope":"ms-onboarding","cur":["ms-onboarding"],"bodyBoxes":1,"allBoxes":2,"boxes":[{"req":true,"on":false,"by":"star","t":true,"kind":"input/mat-checkbox",` +
		`"label":"` + string([]rune(longTerms)[:40]) + `…` + string([]rune(longTerms)[utf8.RuneCountInString(longTerms)-15:]) + `"}],` +
		`"buttons":[{"text":"続行","on":false}],"controls":{"n":3,"kinds":["mat-select","input:date","role:radio"]},` +
		`"outside":[{"text":"Get API key","on":true}]}`
	if got := mustJSON(t, s.Observe.Summary); got != wantS || len(s.Clicks) != 0 || !strings.HasSuffix(s.Observe.Summary.Boxes[0].Label, "同意します *") {
		t.Fatalf("S：结构摘要\n得到 %s\n期望 %s", got, wantS)
	}
	if structure := formatOnboardingStructure(s.Observe.Summary); structure != OnboardingStructureMarker+wantS {
		t.Fatalf("S：整理后的结构摘要应保留标签结尾、控件与范围外按钮\n得到 %s\n期望 %s", structure, OnboardingStructureMarker+wantS)
	}

	var survey map[string]struct {
		Observe onboardingStepReply
		Replies []onboardingStepReply
		Clicks  []string
		Checked []string
	}
	scenario("IS", &survey)
	// 用途选项本身是勾选框（boxes 里 kind 为 role/button、intent），不再列进 buttons：跳过、继续、卡片与轮播都列得下
	wantSurvey := map[string]struct{ skip, proceed, main, buttons, outside string }{
		"zh": {"跳过", "继续", "跳到主要内容",
			`[{"text":"跳到主要内容","on":true,"skip":"neg"},{"text":"跳过","on":true,"skip":"neg"},{"text":"继续 arrow_forward","on":false},` +
				`{"text":"spark Gemini 我们最智能的模型，可助您将任何创意变为现实","on":true,"skip":"card"},` +
				`{"text":"前往第 1 张幻灯片：Gemini","on":true,"skip":"area"},{"text":"前往第 2 张幻灯片：Gemini","on":true,"skip":"area"}]`,
			`[{"text":"Skip to main content","on":true,"skip":"neg"},{"text":"Try Gemini 3","on":true,"skip":"area"}]`},
		"en": {"Skip", "Continue", "Skip to main content",
			`[{"text":"Skip to main content","on":true,"skip":"neg"},{"text":"Skip","on":true,"skip":"neg"},{"text":"Continue arrow_forward","on":false},` +
				`{"text":"spark Gemini Our most intelligent model…","on":true,"skip":"card"},` +
				`{"text":"Go to slide 1: Gemini","on":true,"skip":"card"},{"text":"Go to slide 2: Gemini","on":true,"skip":"card"}]`,
			`[{"text":"Skip to main content","on":true,"skip":"neg"},{"text":"Try Gemini 3","on":true,"skip":"area"},{"text":"Agree","on":true,"skip":"cookie"}]`},
	}
	for lang, want := range wantSurvey {
		got := survey[lang]
		summary := got.Observe.Summary
		if summary == nil || summary.Scope != "ms-onboarding" || mustJSON(t, summary.Cur) != `["ms-intent-selection","ms-horizontal-stepper"]` ||
			mustJSON(t, summary.Parts) != `["ms-intent-selection","ms-horizontal-stepper","ms-stacked-cards-preview","ms-outro"]` ||
			mustJSON(t, summary.Buttons) != want.buttons || mustJSON(t, summary.Outside) != want.outside || len(summary.Boxes) != 4 {
			t.Fatalf("IS %s：用途调查一步的结构摘要（cur 为选项与页脚所在的组件，parts 另列出同时可见的卡片预览与结尾页，卡片为 card，"+
				"无障碍跳转为 neg）: %s", lang, mustJSON(t, summary))
		}
		for _, item := range summary.Boxes {
			if item.Required || item.Checked || item.Terms || item.Kind != "role/button" || !item.Intent {
				t.Fatalf("IS %s：用途选项是 ms-intent-selection 里非必选、不含条款关键词的 role=checkbox 按钮: %+v", lang, summary.Boxes)
			}
		}
		// 整理进失败文案的结构摘要里跳过与继续都在（不被用途选项、卡片与轮播挤掉）
		structure := formatOnboardingStructure(summary)
		if !strings.Contains(structure, `{"text":"`+want.skip+`","on":true,"skip":"neg"}`) ||
			!strings.Contains(structure, `{"text":"`+want.proceed+` arrow_forward","on":false}`) || utf8.RuneCountInString(structure) > onboardingStructureLimit+len([]rune(OnboardingStructureMarker)) {
			t.Fatalf("IS %s：整理后的结构摘要应保留跳过与继续: %s", lang, structure)
		}
		if states(got.Replies) != "waiting,waiting,skipped,left" {
			t.Fatalf("IS %s：不带 skip 时等待、带 skip 时点跳过后离开，得到 %s: %+v", lang, states(got.Replies), got.Replies)
		}
		first := got.Replies[0]
		if first.Reason != "继续按钮未启用" || first.Button != want.proceed || first.Skip != want.skip || got.Replies[2].Button != want.skip {
			t.Fatalf("IS %s：等待时报告禁用的继续按钮与跳过按钮（去掉图标文字），点跳过时报告其文字: %+v", lang, got.Replies[:3])
		}
		if strings.Join(got.Clicks, ",") != "skip" || strings.Join(got.Checked, ",") != "false,false,false,false" {
			t.Fatalf("IS %s：只点“%s”，不勾任何用途、不点卡片、轮播、无障碍跳转（%s）、横幅与 Cookie 横幅: clicks=%v checked=%v",
				lang, want.skip, want.main, got.Clicks, got.Checked)
		}
	}
	var tos sequence
	scenario("TS", &tos)
	if states(tos.Replies) != "checked,clicked,left" || strings.Join(tos.Clicks, ",") != "age,proceed" || tos.Mail ||
		tos.Replies[0].Skip != "" || tos.Replies[1].Button != "继续" || mustJSON(t, tos.Replies[0].Summary.Parts) != `["ms-tos-disclaimer","ms-eclipse"]` {
		t.Fatalf("TS：服务条款一步有可点的“跳过”时，带 skip 也只勾必选项后点继续，邮件项不勾: %+v", tos)
	}
	var consent map[string]sequence
	scenario("TS2", &consent)
	for _, name := range []string{"checkedRequired", "termsOnly", "disclaimer"} {
		got := consent[name]
		if states(got.Replies) != "waiting,waiting" || len(got.Clicks) != 0 || got.Replies[0].Skip != "" || got.Replies[0].Reason != "继续按钮未启用" {
			t.Fatalf("TS2 %s：涉及同意的一步继续按钮一直禁用时也不点跳过、不报告跳过按钮: %+v", name, got)
		}
	}
	var brief struct {
		First   onboardingStepReply
		Replies []onboardingStepReply
		Clicks  []string
	}
	scenario("BD", &brief)
	if brief.First.State != "waiting" || brief.First.Skip != "跳过" || states(brief.Replies) != "clicked,left" ||
		brief.Replies[0].Button != "继续" || strings.Join(brief.Clicks, ",") != "proceed" {
		t.Fatalf("BD：继续按钮刚渲染时禁用、随即可点：允许跳过时也点继续、不点跳过: %+v", brief)
	}
	var cardOnly single
	scenario("CD", &cardOnly)
	if cardOnly.Reply.State != "stuck" || len(cardOnly.Clicks) != 0 || cardOnly.Reply.Skip != "" ||
		mustJSON(t, cardOnly.Reply.Summary.Buttons) != `[{"text":"spark Gemini 我们最智能的模型","on":true,"skip":"card"},{"text":"前往第 2 张幻灯片：Veo","on":true,"skip":"card"}]` {
		t.Fatalf("CD：卡片与轮播按钮不当作唯一剩下的按钮点击: %+v", cardOnly)
	}
	var cardProceed sequence
	scenario("CD2", &cardProceed)
	if states(cardProceed.Replies) != "clicked,left" || strings.Join(cardProceed.Clicks, ",") != "proceed" {
		t.Fatalf("CD2：卡片预览里文字含继续类关键词的按钮照常点击: %+v", cardProceed)
	}
	var horizontal sequence
	scenario("HZ", &horizontal)
	if states(horizontal.Replies) != "skipped,clicked,left" || strings.Join(horizontal.Clicks, ",") != "skip,next-proceed" {
		t.Fatalf("HZ：各步都渲染出来时只点视口里当前一步的跳过与继续，aria-hidden 的结尾页不点: %+v", horizontal)
	}
	wantHZ := `[{"text":"跳过","on":true,"skip":"neg"},{"text":"继续 arrow_forward","on":false},` +
		`{"text":"跳过","on":true,"skip":"neg","off":true},{"text":"继续","on":true,"off":true}]`
	if first := horizontal.Replies[0].Summary; mustJSON(t, first.Cur) != `["ms-intent-selection","ms-horizontal-stepper"]` ||
		len(first.Buttons) < 4 || mustJSON(t, first.Buttons[:4]) != wantHZ || mustJSON(t, horizontal.Replies[1].Summary.Cur) != `["ms-model-intro"]` {
		t.Fatalf("HZ：cur 为当前一步的组件，按钮先列当前一步的、其余各步的带 off: cur=%v/%v buttons=%s",
			first.Cur, horizontal.Replies[1].Summary.Cur, mustJSON(t, first.Buttons))
	}

	var consentCases map[string]sequence
	scenario("CS", &consentCases)
	if len(consentCases) != 22 {
		t.Fatalf("CS：应有 22 个场景，得到 %d", len(consentCases))
	}
	for name, got := range consentCases {
		if len(got.Clicks) != 0 || len(got.Replies) != 2 {
			t.Fatalf("CS %s：条款一步认不全时也算涉及同意，不点任何东西: %+v", name, got)
		}
		for _, reply := range got.Replies {
			if reply.Skip != "" || (reply.State != "waiting" && reply.State != "stuck") {
				t.Fatalf("CS %s：涉及同意的一步不报告跳过按钮、不点跳过: %+v", name, got.Replies)
			}
		}
	}
	if got := consentCases["koreanOnly"].Replies[0]; got.State != "stuck" ||
		!strings.Contains(mustJSON(t, got.Summary.Buttons), `{"text":"건너뛰기","on":true,"skip":"neg"}`) {
		t.Fatalf("CS koreanOnly：“건너뛰기”是跳过类词，不当作唯一剩下的按钮点击: %+v", got)
	}
	if got := consentCases["korean"].Replies[0].Summary.Boxes; len(got) != 1 || !got[0].Terms {
		t.Fatalf("CS korean：韩文“약관”算服务条款关键词: %+v", got)
	}
	// 写成 button[role=checkbox] 的条款勾选框：不在 ms-intent-selection 里或标签认不出时不算用途调查选项（摘要里没有 intent）
	for name, want := range map[string]string{
		"roleButtonPolicy":     `[{"req":false,"on":false,"kind":"role/button","label":"I accept the Generative AI Prohibited Use Policy"}]`,
		"roleButtonIcon":       `[{"req":false,"on":false,"kind":"role/button","label":""},{"req":false,"on":false,"kind":"role/button","label":""}]`,
		"roleButtonLabelledby": `[{"req":false,"on":false,"kind":"role/button","label":"Skip Continue"}]`,
		"intentUnlabelled":     `[{"req":false,"on":false,"kind":"role/button","label":"Skip Continue"}]`,
		"checkedRoleAge": `[{"req":false,"on":false,"intent":true,"kind":"role/button","label":"Explore latest models"},` +
			`{"req":true,"on":true,"by":"star","t":true,"intent":true,"kind":"role/button","label":"I am 18 years of age or older *"}]`,
	} {
		if got := mustJSON(t, consentCases[name].Replies[0].Summary.Boxes); got != want {
			t.Fatalf("CS %s：勾选框\n得到 %s\n期望 %s", name, got, want)
		}
	}
	var intentPlain sequence
	scenario("IN", &intentPlain)
	if states(intentPlain.Replies) != "waiting,skipped,left" || intentPlain.Replies[0].Skip != "Skip" || strings.Join(intentPlain.Clicks, ",") != "skip" {
		t.Fatalf("IN：选项不是 role=checkbox 按钮时靠可见的 ms-intent-selection 认出用途调查，照常点跳过、不点选项: %+v", intentPlain)
	}
	var headed map[string]struct{ Observe onboardingStepReply }
	scenario("HD", &headed)
	for name, want := range map[string]string{"cards": `["ms-horizontal-stepper","ms-stacked-cards-preview"]`, "outro": `["ms-horizontal-stepper","ms-outro"]`} {
		if got := mustJSON(t, headed[name].Observe.Summary.Cur); got != want {
			t.Fatalf("HD %s：继续在共用页脚里时 cur 另接标题所在的组件\n得到 %s\n期望 %s", name, got, want)
		}
	}
	var footerFirst struct {
		Before  onboardingStepReply
		Replies []onboardingStepReply
		Clicks  []string
	}
	scenario("FT", &footerFirst)
	if footerFirst.Before.State != "waiting" || footerFirst.Before.Skip != "" || states(footerFirst.Replies) != "waiting,skipped,left" ||
		footerFirst.Replies[0].Skip != "Skip" || strings.Join(footerFirst.Clicks, ",") != "skip" {
		t.Fatalf("FT：只有页脚时不报告跳过，用途调查组件渲染出来之后才报告并点跳过: %+v", footerFirst)
	}
	var scoped map[string]sequence
	scenario("SA", &scoped)
	for name, wantState := range map[string]string{"hiddenStep": "waiting", "dialog": "stuck"} {
		got := scoped[name]
		if states(got.Replies) != wantState+","+wantState || len(got.Clicks) != 0 || got.Replies[0].Skip != "" || got.Replies[1].Skip != "" {
			t.Fatalf("SA %s：aria-hidden 的另一步与作用域外对话框里的“Skip”不点、不报告: %+v", name, got)
		}
	}
	var skipButtons map[string]sequence
	scenario("SK", &skipButtons)
	for _, name := range []string{"link", "buttonInLink", "header", "nav", "label", "cookie", "popup", "disabled", "mainLabel", "preview", "tablist", "carousel", "body", "loading"} {
		got := skipButtons[name]
		if states(got.Replies) != "waiting" || got.Replies[0].Reason != "继续按钮未启用" || got.Replies[0].Skip != "" || len(got.Clicks) != 0 {
			t.Fatalf("SK %s：不算跳过按钮（或这一步不点跳过），不报告、不点: %+v", name, got)
		}
	}
	if got := skipButtons["plain"]; states(got.Replies) != "skipped" || strings.Join(got.Clicks, ",") != "x" {
		t.Fatalf("SK plain：同样的页面上普通的“Skip”照常点: %+v", got)
	}
	if got := skipButtons["previewFirst"]; states(got.Replies) != "skipped" || strings.Join(got.Clicks, ",") != "x" {
		t.Fatalf("SK previewFirst：卡片预览里的“Skip”排在前面时点页脚的跳过: %+v", got)
	}
	var cardText map[string]sequence
	scenario("CT", &cardText)
	for _, name := range []string{"div", "button"} {
		got := cardText[name]
		if states(got.Replies) != "waiting,skipped,left" || got.Replies[0].Button != "Continue" || got.Replies[0].Skip != "Skip" ||
			strings.Join(got.Clicks, ",") != "skip" {
			t.Fatalf("CT %s：卡片文字含 Try 也不当作继续按钮，报告并点跳过: %+v", name, got)
		}
	}
	var horizontalOutro map[string]sequence
	scenario("HO", &horizontalOutro)
	if got := horizontalOutro["goButton"]; states(got.Replies) != "clicked,left" || got.Replies[0].Button != "前往 AI Studio" ||
		strings.Join(got.Clicks, ",") != "go" || mustJSON(t, got.Replies[0].Summary.Cur) != `["ms-outro"]` ||
		mustJSON(t, got.Replies[0].Summary.Buttons[0]) != `{"text":"前往 AI Studio","on":true}` {
		t.Fatalf("HO goButton：结尾页唯一剩下的按钮点它，不点视口外上一步的继续与跳过，摘要先列当前一步: %+v", got)
	}
	if got := horizontalOutro["linkOnly"]; states(got.Replies) != "stuck,stuck" || len(got.Clicks) != 0 || got.Replies[0].Skip != "" ||
		got.Replies[0].Reason != "找不到可点的继续按钮" || mustJSON(t, got.Replies[0].Summary.Links) != `["开始使用"]` ||
		mustJSON(t, got.Replies[0].Summary.Cur) != `["ms-outro"]` {
		t.Fatalf("HO linkOnly：结尾页只有链接与关闭按钮时找不到可点的继续按钮，不回去点上一步的跳过: %+v %s", got, mustJSON(t, got.Replies[0].Summary))
	}
	var tosKept map[string]sequence
	scenario("TK", &tosKept)
	for _, name := range []string{"keptHidden", "keptInvisible"} {
		if got := tosKept[name]; states(got.Replies) != "skipped,left" || strings.Join(got.Clicks, ",") != "skip" {
			t.Fatalf("TK %s：上一步留在 DOM 里、已隐藏的条款组件不挡当前用途调查一步的跳过: %+v", name, got)
		}
	}
	var tosOffscreen map[string]sequence
	scenario("TO", &tosOffscreen)
	if got := tosOffscreen["checked"]; states(got.Replies) != "skipped,left" || strings.Join(got.Clicks, ",") != "skip" {
		t.Fatalf("TO checked：视口外条款一步的必选项已勾上，当前的用途调查一步照常跳过: %+v", got)
	}
	if got := tosOffscreen["unchecked"]; states(got.Replies) != "waiting,waiting" || got.Replies[0].Skip != "" || len(got.Clicks) != 0 {
		t.Fatalf("TO unchecked：整页还有未勾选的条款项时不跳过: %+v", got)
	}
}

// fakeOnboardingWizard 在 fakeOnboardingDOMLib 上搭一个多步向导（照线上中文界面）：条款（ms-tos-disclaimer：“* 我已年满 18 周岁”必选、
// 邮件资讯可选，继续在勾上必选项前禁用）→ 用途调查（surveyStep：继续禁用，点“跳过”到下一步）→ 介绍卡片（ms-stacked-cards-preview 里
// 模型卡片与轮播按钮，“跳过”可点，“继续”刚渲染时禁用、前 2 次执行表达式之后可点；没有用途调查的证据，不报告跳过）→ 结尾页（ms-outro：
// “开始使用”）→ 离开。report 返回点击过的元素、所在步骤、邮件项与用途选项的状态，以及用途调查一步从出现到点跳过的毫秒数（surveyWait）
const fakeOnboardingWizard = `
const onboarding = h('ms-onboarding', {});
const state = {stage: 'tos', cardsEvaluations: 0, surveyAt: 0, skippedAt: 0};
let mail = null, survey = null, cardsProceed = null;
const outroStage = () => {
  state.stage = 'outro';
  const start = h('button', {'mat-flat-button': '', 'data-name': 'start'}, '开始使用');
  route(start, onboarding);
  replace(onboarding, h('ms-outro', {}, h('h2', {}, '一切就绪'), start));
};
const cardsStage = () => {
  state.stage = 'cards';
  const skip = h('button', {'data-name': 'cards-skip'}, '跳过');
  skip.addEventListener('click', outroStage);
  cardsProceed = h('button', {disabled: '', class: 'ms-button-primary', 'data-name': 'cards-proceed'}, '继续', icon('arrow_forward'));
  cardsProceed.addEventListener('click', outroStage);
  replace(onboarding,
    h('ms-stacked-cards-preview', {}, h('div', {role: 'button', 'data-name': 'cards-card'}, icon('movie'), 'Veo 我们最先进的视频生成模型'),
      h('nav', {}, h('button', {'aria-label': '前往第 2 张幻灯片：Veo', 'data-name': 'cards-slide'}, h('span', {class: 'dot'})))),
    h('ms-horizontal-stepper', {}, skip, cardsProceed));
};
const surveyStage = () => {
  state.stage = 'survey';
  state.surveyAt = Date.now();
  survey = surveyStep('zh', 'survey-');
  survey.skip.addEventListener('click', () => {
    state.skippedAt = Date.now();
    cardsStage();
  });
  replace(onboarding, ...survey.parts);
};
const tosStage = () => {
  const age = h('input', {type: 'checkbox', id: 'age', 'data-name': 'age'});
  mail = h('input', {type: 'checkbox', id: 'mail', 'data-name': 'mail'});
  const proceed = h('button', {type: 'submit', disabled: '', 'data-name': 'tos-proceed'}, '继续', icon('arrow_forward'));
  age.addEventListener('change', () => { proceed.disabled = !age.checked; });
  proceed.addEventListener('click', surveyStage);
  replace(onboarding, h('ms-tos-disclaimer', {}, h('h1', {}, '欢迎使用 Google AI Studio'), box(age, '* 我已年满 18 周岁'),
    box(mail, '通过电子邮件接收 Google AI Studio 的最新资讯'), proceed), h('ms-eclipse', {}));
};
mount(h('body', {}, skipToMain('Skip to main content'), h('ms-app', {}, onboarding)), '/onboarding');
tosStage();
const beforeEvaluate = () => {
  if (state.stage === 'cards' && ++state.cardsEvaluations > 2) cardsProceed.disabled = false;
};
const report = () => ({clicks, stage: state.stage, mail: mail.checked, surveyWait: state.skippedAt - state.surveyAt,
  intents: survey ? survey.options.map(option => option.getAttribute('aria-checked')) : []});
`

// fakeOnboardingNodeLoop 为常驻 node 进程的命令循环：每行是一条 JSON 编码的表达式，执行前调用 beforeEvaluate，回答一行
// {"value": …} 或 {"exception": "…"}；“__report”回答 report() 的 JSON
const fakeOnboardingNodeLoop = `
require('readline').createInterface({input: process.stdin}).on('line', line => {
  const request = JSON.parse(line);
  let answer;
  if (request === '__report') {
    answer = {value: JSON.stringify(report())};
  } else {
    beforeEvaluate();
    try {
      answer = {value: (0, eval)(request)};
    } catch (error) {
      answer = {exception: String(error && error.stack || error)};
    }
  }
  process.stdout.write(JSON.stringify(answer) + '\n');
});
`

// onboardingNode 为常驻的 node 进程：在 fakeOnboardingDOMLib 上搭好 setup 里的页面，逐条执行假浏览器转来的表达式，页面状态跨表达式保留
type onboardingNode struct {
	mu     sync.Mutex
	stdin  io.WriteCloser
	stdout *bufio.Reader
	stderr *bytes.Buffer
}

// startOnboardingNode 启动 node 进程（机器上没有 node 时跳过测试），测试结束时关闭
func startOnboardingNode(t *testing.T, setup string) *onboardingNode {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("没有 node，跳过首次使用页多步向导的执行检查")
	}
	scriptPath := filepath.Join(t.TempDir(), "wizard.js")
	if err := os.WriteFile(scriptPath, []byte(fakeOnboardingDOMLib+setup+fakeOnboardingNodeLoop), 0o600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(node, scriptPath)
	stdin, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	process := &onboardingNode{stdin: stdin, stdout: bufio.NewReader(stdout), stderr: &bytes.Buffer{}}
	command.Stderr = process.stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = command.Wait()
	})
	return process
}

// evaluate 把表达式交给 node 执行，按假浏览器的格式回答（在假浏览器的协程里调用，不调用 t.Fatal）
func (process *onboardingNode) evaluate(expression string) fakeEvaluate {
	process.mu.Lock()
	defer process.mu.Unlock()
	encoded, _ := json.Marshal(expression)
	if _, err := process.stdin.Write(append(encoded, '\n')); err != nil {
		return fakeEvaluate{exception: "写入 node 失败: " + err.Error()}
	}
	line, err := process.stdout.ReadString('\n')
	if err != nil {
		return fakeEvaluate{exception: "读取 node 失败: " + err.Error() + " " + process.stderr.String()}
	}
	var answer struct {
		Value     any    `json:"value"`
		Exception string `json:"exception"`
	}
	if err := json.Unmarshal([]byte(line), &answer); err != nil {
		return fakeEvaluate{exception: "node 回答无法解析: " + line}
	}
	if answer.Exception != "" {
		return fakeEvaluate{exception: answer.Exception}
	}
	return fakeEvaluate{value: answer.Value}
}

// TestCompleteOnboardingWizardInNode Go 侧轮询与一步式表达式在 node 最小 DOM 上连续走完多步向导（见 fakeOnboardingWizard）：
// 条款一步勾 18 岁必选项、点继续（邮件项不勾）；用途调查一步继续禁用，等页面稳定（持续 skip，比两次点击的最短间隔 retry 长）后点跳过，
// 不选任何用途；介绍卡片一步继续刚渲染时禁用、随即可点，点继续不点跳过（卡片与轮播不点）；结尾页点“开始使用”；离开后完成，
// 进度记录按顺序记下每一步
func TestCompleteOnboardingWizardInNode(t *testing.T) {
	fastOnboarding(t)
	// 这里不测总时限：放宽到 10 秒（ctx 另有 10 秒上限），负载高时不因调度慢失败；skip 比 retry 长，跳过的等待才参与判定
	onboardingTiming.total = 10 * time.Second
	onboardingTiming.skip = 200 * time.Millisecond
	process := startOnboardingNode(t, fakeOnboardingWizard)
	page := newFakeBiDiPage(t, process.evaluate)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, err := page.client.completeOnboarding(ctx, "ctx-1")
	if err != nil {
		t.Fatalf("多步向导应完成: %v", err)
	}
	wantSteps := []string{"勾选:* 我已年满 18 周岁", "点击:继续", "跳过:跳过", "点击:继续", "点击:开始使用"}
	if !slices.Equal(result.Steps, wantSteps) || result.Checked != 1 || result.Skipped != 1 || result.Button != "开始使用" ||
		!slices.Equal(result.Labels, []string{"* 我已年满 18 周岁"}) {
		t.Fatalf("进度记录\n得到 %+v\n期望 %q", result, wantSteps)
	}
	if want := "勾选:* 我已年满 18 周岁；点击:继续；跳过:跳过；点击:继续；点击:开始使用"; result.Progress() != want {
		t.Fatalf("写进日志的进度记录\n得到 %s\n期望 %s", result.Progress(), want)
	}
	reported := process.evaluate("__report")
	var report struct {
		Clicks     []string
		Stage      string
		Mail       bool
		Intents    []string
		SurveyWait int64
	}
	if text, ok := reported.value.(string); !ok || json.Unmarshal([]byte(text), &report) != nil {
		t.Fatalf("读取页面状态失败: %+v", reported)
	}
	if strings.Join(report.Clicks, ",") != "age,tos-proceed,survey-skip,cards-proceed,start" || report.Stage != "outro" || report.Mail ||
		strings.Join(report.Intents, ",") != "false,false,false,false" {
		t.Fatalf("页面上应依次点 18 岁必选项、条款的继续、用途调查的跳过、介绍卡片的继续与开始使用，不勾邮件与用途: %+v", report)
	}
	if time.Duration(report.SurveyWait)*time.Millisecond < onboardingTiming.skip {
		t.Fatalf("用途调查一步应等页面稳定 %s 再点跳过，只等了 %dms", onboardingTiming.skip, report.SurveyWait)
	}
	if page.snapshots.Load() != 0 {
		t.Fatal("完成时不应采集快照")
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
	// 一步式表达式只点勾选框、挑出的继续按钮与不涉及同意的一步里的跳过按钮：不导航、不写存储
	step := onboardingStepExpression(onboardingStepOptions{Click: true, Relaxed: true, Skip: true})
	for _, forbidden := range []string{"location.assign", "location.reload", "location.href =", "history.", "localStorage", "dispatchEvent"} {
		if strings.Contains(step, forbidden) {
			t.Fatalf("一步式表达式不应包含 %q", forbidden)
		}
	}
	if strings.Count(step, ".click()") != 3 {
		t.Fatalf("一步式表达式只应有勾选、继续与跳过三处点击")
	}
}

// TestFormatOnboardingStructure 结构摘要打码、“|”换成“¦”、单行、不超过上限，超长时先缩短标签再减少条目；当前一步的组件（cur）
// 与按钮不在当前一步的标记（off）一直保留
func TestFormatOnboardingStructure(t *testing.T) {
	if got := formatOnboardingStructure(nil); got != "" {
		t.Fatalf("没有摘要时应为空串，得到 %q", got)
	}
	summary := &onboardingSummary{Scope: "ms-onboarding", Cur: []string{"ms-intent-selection", "ms-horizontal-stepper"}, BodyBoxes: 9, AllBoxes: 11,
		Controls: &onboardingControls{Count: 5, Kinds: []string{"mat-select", "input:date", "role:radio", "role:switch", "select"}}}
	for index := 0; index < 8; index++ {
		box := onboardingBox{Required: index%2 == 0, Disabled: index%2 == 1, Terms: true, Optional: index == 1, Hidden: index == 0, Kind: "input/mat-checkbox",
			Label: fmt.Sprintf("%d | someone@example.com %s 同意します *", index, strings.Repeat("長", 80))}
		if box.Required {
			box.By = "star"
		}
		summary.Boxes = append(summary.Boxes, box)
		summary.Buttons = append(summary.Buttons, onboardingButton{Text: fmt.Sprintf("按钮 %d %s", index, strings.Repeat("字", 50)), Enabled: true, Skip: "neg", Off: index >= 3})
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
		decoded.Buttons[0].Skip != "neg" || decoded.Buttons[0].Off || len(decoded.Buttons) != 4 || !decoded.Buttons[3].Off || decoded.Controls == nil || decoded.Controls.Count != 5 ||
		len(decoded.Controls.Kinds) != 4 || !slices.Equal(decoded.Cur, []string{"ms-intent-selection", "ms-horizontal-stepper"}) {
		t.Fatalf("缩减时应保留范围、当前一步的组件、勾选框数、表单控件计数与各条目的标记: %+v", decoded)
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

// TestFormatOnboardingStructureKeepsActions 线上用途调查一步的结构摘要（旧版：四个用途选项也列在 buttons 里）补上 cur 与 parts 后超长：
// 先把 parts 减到 3 个，截按钮时可以点的与 neg（继续、跳过）优先于用途选项、轮播（area）保留，按原顺序输出
func TestFormatOnboardingStructureKeepsActions(t *testing.T) {
	const online = `{"scope":"ms-onboarding","cur":["ms-intent-selection","ms-horizontal-stepper"],` +
		`"parts":["ms-intent-selection","ms-horizontal-stepper","ms-stacked-cards-preview","ms-outro"],"bodyBoxes":4,"allBoxes":4,"boxes":[` +
		`{"req":false,"on":false,"kind":"role/button","label":"探索最新模型"},{"req":false,"on":false,"kind":"role/button","label":"使用 Gemini API 开发"},` +
		`{"req":false,"on":false,"kind":"role/button","label":"开展氛围编程，让创意跃然屏上"},{"req":false,"on":false,"kind":"role/button","label":"与 Gemini 对话"}],` +
		`"buttons":[{"text":"探索最新模型 add_circle","on":true,"skip":"area"},{"text":"使用 Gemini API 开发 add_circle","on":true,"skip":"area"},` +
		`{"text":"开展氛围编程，让创意跃然屏上 add_circle","on":true,"skip":"area"},{"text":"与 Gemini 对话 add_circle","on":true,"skip":"area"},` +
		`{"text":"跳过","on":true,"skip":"neg"},{"text":"继续 arrow_forward","on":false},{"text":"spark Gemini 我们最智能的模型，可助您将任何创意变为现实","on":true},` +
		`{"text":"前往第 1 张幻灯片：Gemini","on":true,"skip":"area"}],"outside":[{"text":"Skip to main content","on":true,"skip":"neg"}]}`
	var summary onboardingSummary
	if err := json.Unmarshal([]byte(online), &summary); err != nil {
		t.Fatal(err)
	}
	if utf8.RuneCountInString(marshalCompact(summary)) <= onboardingStructureLimit {
		t.Fatalf("线上摘要补上 cur 与 parts 后应超长，测不到缩减")
	}
	got := formatOnboardingStructure(&summary)
	var decoded onboardingSummary
	if err := json.Unmarshal([]byte(strings.TrimPrefix(got, OnboardingStructureMarker)), &decoded); err != nil {
		t.Fatalf("应仍是合法 JSON: %v: %s", err, got)
	}
	want := `[{"text":"探索最新模型 add_circle","on":true,"skip":"area"},{"text":"跳过","on":true,"skip":"neg"},{"text":"继续 arrow_forward","on":false},` +
		`{"text":"spark Gemini 我们最智能的模型，可助您将任何创意变为现实","on":true}]`
	if buttons := marshalCompact(decoded.Buttons); buttons != want || len(decoded.Parts) != 3 || len(decoded.Cur) != 2 || len(decoded.Boxes) != 4 {
		t.Fatalf("截按钮时应保留继续与跳过\n得到 %s\n期望 buttons=%s", got, want)
	}
	// 当前一步的优先于其余各步的（off），按原顺序输出；不超过 limit 时原样返回
	few := []onboardingButton{{Text: "a", Skip: "area"}, {Text: "b", Off: true}, {Text: "c"}}
	if kept := keepButtons(few, 2); marshalCompact(kept) != `[{"text":"a","on":false,"skip":"area"},{"text":"c","on":false}]` {
		t.Fatalf("当前一步的优先于其余各步的，按原顺序输出: %s", marshalCompact(kept))
	}
	if kept := keepButtons(few, 3); len(kept) != 3 || !kept[1].Off {
		t.Fatalf("不超过 limit 时原样返回: %+v", kept)
	}
}

// TestOnboardingProgress 进度记录：什么都没做时为“-”；不超过 onboardingProgressItems 步时全部列出，超过时保留开头 4 步与结尾 7 步
func TestOnboardingProgress(t *testing.T) {
	if got := (OnboardingResult{}).Progress(); got != "-" {
		t.Fatalf("什么都没做时应为 -，得到 %q", got)
	}
	steps := []string{"勾选:* 私は 18 歳以上です", "点击:続行"}
	for index := 1; index <= 11; index++ {
		steps = append(steps, fmt.Sprintf("跳过:第%d步", index))
	}
	result := OnboardingResult{Steps: steps}
	want := "勾选:* 私は 18 歳以上です；点击:続行；跳过:第1步；跳过:第2步；…（省略 2 步）；" +
		"跳过:第5步；跳过:第6步；跳过:第7步；跳过:第8步；跳过:第9步；跳过:第10步；跳过:第11步"
	if got := result.Progress(); got != want {
		t.Fatalf("进度记录\n得到 %s\n期望 %s", got, want)
	}
	if len(result.Steps) != 13 || result.Steps[4] != "跳过:第3步" {
		t.Fatalf("省略不应改动原记录: %v", result.Steps)
	}
	if got := (OnboardingResult{Steps: steps[:onboardingProgressItems]}).Progress(); strings.Contains(got, "省略") {
		t.Fatalf("不超过 %d 步时应全部列出: %s", onboardingProgressItems, got)
	}
}

// TestOnboardingLimits 多步向导的上限：总时限约 75 秒、点击继续 8 次、跳过 4 次（单独计数）、跳过前等页面稳定 2 秒
func TestOnboardingLimits(t *testing.T) {
	if onboardingTiming.total != 75*time.Second || onboardingTiming.skip != 2*time.Second ||
		onboardingMaxButtonClicks != 8 || onboardingMaxSkipClicks != 4 {
		t.Fatalf("上限 total=%s skip=%s clicks=%d skips=%d", onboardingTiming.total, onboardingTiming.skip, onboardingMaxButtonClicks, onboardingMaxSkipClicks)
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
	onboardingTiming.skip = 30 * time.Millisecond
	onboardingTiming.saved = 10 * time.Millisecond
	t.Cleanup(func() { onboardingTiming = saved })
}

// onboardingStepVariants 返回一步式表达式的各种参数组合，供假页面认出这条表达式与它的参数
func onboardingStepVariants() map[string]onboardingStepOptions {
	variants := make(map[string]onboardingStepOptions)
	for mask := 0; mask < 32; mask++ {
		options := onboardingStepOptions{Click: mask&1 != 0, Relaxed: mask&2 != 0, Clicked: mask&4 != 0, Observe: mask&8 != 0, Skip: mask&16 != 0}
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
	// 只有“按钮一直禁用”与“总时限”测总时限（见 shortTotal）：其余子测试放宽到 10 秒，负载高时连续几次点击不因调度慢超时
	onboardingTiming.total = 10 * time.Second
	shortTotal := func(t *testing.T) {
		onboardingTiming.total = 1500 * time.Millisecond
		t.Cleanup(func() { onboardingTiming.total = 10 * time.Second })
	}
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
		if err != nil || result.Checked != 2 || result.Button != "続行 ¦ so***@***" || result.Elapsed <= 0 || !result.acted() || result.Skipped != 0 {
			t.Fatalf("完成结果 = %+v, %v", result, err)
		}
		// 标签打码、保留开头 20 字与结尾 9 字，按放宽规则补勾的带“[补]”
		if want := []string{"* 私は 18 歳以上です", "[补]Google 利用規約と Gemini …o***@***）"}; !slices.Equal(result.Labels, want) {
			t.Fatalf("勾选标签\n得到 %q\n期望 %q", result.Labels, want)
		}
		// 进度记录按顺序记下勾选与点击，文字打码截短
		if want := "勾选:* 私は 18 歳以上です；勾选:[补]Google 利用規約と Gemini …o***@***）；点击:続行 ¦ so***@***"; result.Progress() != want {
			t.Fatalf("进度记录\n得到 %s\n期望 %s", result.Progress(), want)
		}
		if page.snapshots.Load() != 0 {
			t.Fatalf("完成时不应采集快照")
		}
	})
	t.Run("找不到按钮", func(t *testing.T) {
		_, err, page := run(t, func(int, onboardingStepOptions) fakeEvaluate {
			return onboardingReply("stuck", map[string]any{"reason": "找不到可点的继续按钮"})
		}, context.Background())
		if err == nil || !strings.HasPrefix(err.Error(), OnboardingFailedPrefix+": 找不到可点的继续按钮 | 已做=- | 页面={") ||
			!strings.Contains(err.Error(), OnboardingStructureMarker+`{"scope":"ms-onboarding"`) || page.snapshots.Load() != 1 {
			t.Fatalf("应以固定前缀失败并附快照与结构摘要: %v", err)
		}
		if !onboardingRelated(err) {
			t.Fatal("首次使用页失败应可识别")
		}
	})
	t.Run("按钮一直禁用", func(t *testing.T) {
		shortTotal(t)
		var relaxed bool
		_, err, _ := run(t, func(_ int, options onboardingStepOptions) fakeEvaluate {
			relaxed = relaxed || options.Relaxed
			return onboardingReply("waiting", map[string]any{"reason": "继续按钮未启用"})
		}, context.Background())
		if err == nil || !strings.HasPrefix(err.Error(), OnboardingFailedPrefix+": 等待超时（继续按钮未启用） | 已做=- | 页面=") || !relaxed {
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
		want := fmt.Sprintf("%s: 点击继续 %d 次后仍停在首次使用页 | 已做=%s | 页面=", OnboardingFailedPrefix, onboardingMaxButtonClicks,
			strings.Repeat("点击:続行；", onboardingMaxButtonClicks-1)+"点击:続行")
		if err == nil || !strings.HasPrefix(err.Error(), want) || clicks != onboardingMaxButtonClicks {
			t.Fatalf("点击 %d 次后应失败: clicks=%d %v", onboardingMaxButtonClicks, clicks, err)
		}
	})
	t.Run("次数用完时先确认页面", func(t *testing.T) {
		// 第 8 次点击继续后页面就离开了，但点击之后的等待（settle）比 retry 长（负载高时）：再读一次页面、离开即完成，不按次数用完失败
		onboardingTiming.settle = 60 * time.Millisecond
		defer func() { onboardingTiming.settle = 10 * time.Millisecond }()
		clicks := 0
		result, err, _ := run(t, func(_ int, options onboardingStepOptions) fakeEvaluate {
			switch {
			case clicks >= onboardingMaxButtonClicks:
				return onboardingReply("left", nil)
			case options.Click:
				clicks++
				return onboardingReply("clicked", map[string]any{"button": "继续"})
			}
			return onboardingReply("waiting", map[string]any{"reason": "等待上一次点击生效"})
		}, context.Background())
		if err != nil || clicks != onboardingMaxButtonClicks || len(result.Steps) != onboardingMaxButtonClicks {
			t.Fatalf("第 %d 次点击后离开应完成: clicks=%d result=%+v err=%v", onboardingMaxButtonClicks, clicks, result, err)
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
	t.Run("进度写进失败文案", func(t *testing.T) {
		// 条款一步勾选、点了继续，下一步找不到可点的东西：失败文案在原因之后、页面快照之前列出做过的每一步
		_, err, _ := run(t, func(step int, _ onboardingStepOptions) fakeEvaluate {
			switch step {
			case 1:
				return onboardingReply("checked", map[string]any{"label": "* 私は 18 歳以上です"})
			case 2:
				return onboardingReply("clicked", map[string]any{"button": "続行"})
			}
			return onboardingReply("stuck", map[string]any{"reason": "找不到可点的继续按钮"})
		}, context.Background())
		want := OnboardingFailedPrefix + ": 找不到可点的继续按钮 | 已做=勾选:* 私は 18 歳以上です；点击:続行 | 页面={"
		if err == nil || !strings.HasPrefix(err.Error(), want) || !strings.Contains(err.Error(), OnboardingStructureMarker) {
			t.Fatalf("失败文案应带进度记录\n得到 %v\n期望前缀 %s", err, want)
		}
	})
	t.Run("等页面稳定再跳过", func(t *testing.T) {
		// 不涉及同意的一步：继续按钮一直禁用、有跳过按钮，从第一次报告可以跳过起满 skip 才带 Skip；跳过后进入下一步、继续可点，
		// 点继续后离开。跳过与点击继续分别计数，进度记录按顺序记下
		var firstSkippable, skipSent time.Time
		stage := "survey"
		result, err, _ := run(t, func(_ int, options onboardingStepOptions) fakeEvaluate {
			switch {
			case stage == "survey" && options.Skip:
				skipSent = time.Now()
				stage = "intro"
				return onboardingReply("skipped", map[string]any{"button": "跳过"})
			case stage == "survey":
				if firstSkippable.IsZero() {
					firstSkippable = time.Now()
				}
				return onboardingReply("waiting", map[string]any{"reason": "继续按钮未启用", "button": "继续", "skip": "跳过"})
			case stage == "intro" && options.Click:
				stage = "done"
				return onboardingReply("clicked", map[string]any{"button": "开始使用"})
			case stage == "intro":
				return onboardingReply("waiting", map[string]any{"reason": "等待上一次点击生效", "button": "开始使用"})
			}
			return onboardingReply("left", nil)
		}, context.Background())
		if err != nil || skipSent.Sub(firstSkippable) < onboardingTiming.skip || result.Skipped != 1 || result.Checked != 0 ||
			result.Button != "开始使用" || !slices.Equal(result.Steps, []string{"跳过:跳过", "点击:开始使用"}) || !result.acted() {
			t.Fatalf("应等满 %s 再跳过、之后点继续离开: 等了 %s result=%+v err=%v", onboardingTiming.skip, skipSent.Sub(firstSkippable), result, err)
		}
	})
	t.Run("继续按钮短暂禁用", func(t *testing.T) {
		// 继续按钮刚渲染时禁用（报告可以跳过），不到 skip 就变为可点：点继续，不带 Skip、不点跳过
		onboardingTiming.skip = 300 * time.Millisecond
		defer func() { onboardingTiming.skip = 30 * time.Millisecond }()
		var rendered time.Time
		skipRequested, clicked := false, false
		result, err, _ := run(t, func(_ int, options onboardingStepOptions) fakeEvaluate {
			skipRequested = skipRequested || options.Skip
			if rendered.IsZero() {
				rendered = time.Now()
			}
			switch {
			case clicked:
				return onboardingReply("left", nil)
			case time.Since(rendered) < 30*time.Millisecond:
				return onboardingReply("waiting", map[string]any{"reason": "继续按钮未启用", "button": "继续", "skip": "跳过"})
			case options.Click:
				clicked = true
				return onboardingReply("clicked", map[string]any{"button": "继续"})
			}
			return onboardingReply("waiting", map[string]any{"reason": "等待上一次点击生效", "button": "继续"})
		}, context.Background())
		if err != nil || skipRequested || result.Skipped != 0 || !slices.Equal(result.Steps, []string{"点击:继续"}) {
			t.Fatalf("继续按钮随即可点时应点继续、不跳过: skip=%t result=%+v err=%v", skipRequested, result, err)
		}
	})
	t.Run("跳过次数上限", func(t *testing.T) {
		skips := 0
		_, err, _ := run(t, func(_ int, options onboardingStepOptions) fakeEvaluate {
			if options.Skip {
				skips++
				return onboardingReply("skipped", map[string]any{"button": "跳过"})
			}
			return onboardingReply("waiting", map[string]any{"reason": "继续按钮未启用", "skip": "跳过"})
		}, context.Background())
		want := fmt.Sprintf("%s: 跳过 %d 次后仍停在首次使用页 | 已做=%s | 页面={", OnboardingFailedPrefix, onboardingMaxSkipClicks,
			strings.Repeat("跳过:跳过；", onboardingMaxSkipClicks-1)+"跳过:跳过")
		if err == nil || !strings.HasPrefix(err.Error(), want) || skips != onboardingMaxSkipClicks {
			t.Fatalf("跳过 %d 次后应失败: skips=%d %v", onboardingMaxSkipClicks, skips, err)
		}
	})
	t.Run("跳过不占点击继续的名额", func(t *testing.T) {
		// 多步向导：每一步先跳过、再点继续（共 onboardingMaxSkipClicks 次跳过与 onboardingMaxButtonClicks 次继续），之后离开
		skips, clicks := 0, 0
		result, err, _ := run(t, func(_ int, options onboardingStepOptions) fakeEvaluate {
			switch {
			case clicks >= onboardingMaxButtonClicks:
				return onboardingReply("left", nil)
			case skips <= clicks && skips < onboardingMaxSkipClicks:
				if options.Skip {
					skips++
					return onboardingReply("skipped", map[string]any{"button": "跳过"})
				}
				return onboardingReply("waiting", map[string]any{"reason": "继续按钮未启用", "skip": "跳过"})
			case options.Click:
				clicks++
				return onboardingReply("clicked", map[string]any{"button": "继续"})
			}
			return onboardingReply("waiting", map[string]any{"reason": "等待上一次点击生效"})
		}, context.Background())
		if err != nil || result.Skipped != onboardingMaxSkipClicks || len(result.Steps) != onboardingMaxSkipClicks+onboardingMaxButtonClicks ||
			!strings.HasPrefix(result.Progress(), "跳过:跳过；点击:继续；跳过:跳过；点击:继续；") {
			t.Fatalf("跳过与点击继续应分别计数: result=%+v err=%v", result, err)
		}
	})
	t.Run("总时限", func(t *testing.T) {
		// 一直报告可以跳过、但跳过从不生效之外的情形（这里一直等待）：到总时限失败，不超过 ctx
		shortTotal(t)
		started := time.Now()
		_, err, _ := run(t, func(int, onboardingStepOptions) fakeEvaluate {
			return onboardingReply("waiting", map[string]any{"reason": "首次使用页尚未渲染"})
		}, context.Background())
		if elapsed := time.Since(started); err == nil || !strings.HasPrefix(err.Error(), OnboardingFailedPrefix+": 等待超时（首次使用页尚未渲染） | 已做=- | 页面=") ||
			elapsed < onboardingTiming.total || elapsed > onboardingTiming.total+pageSnapshotTimeout {
			t.Fatalf("应到总时限 %s 失败: elapsed=%s %v", onboardingTiming.total, elapsed, err)
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
		if err == nil || !strings.HasPrefix(err.Error(), OnboardingFailedPrefix+": context deadline exceeded（继续按钮未启用） | 已做=- | 页面={") ||
			!errors.Is(err, context.DeadlineExceeded) || page.snapshots.Load() != 1 {
			t.Fatalf("ctx 到期时应以固定前缀失败、保留错误链，快照用独立的短上下文: %v", err)
		}
	})
}
