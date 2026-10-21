// CSP 专项浏览器验证（tools/csp_smoke.js）
//
// ## 为什么需要它
//
// 加了 Content-Security-Policy 之后，最危险的失败模式是**面板看着正常、
// 但某个功能静默失效**：内联脚本被拦、echarts CDN 被拦、样式被拦。
// 这些在 curl 与 go test 里都看不出来 —— 头是按预期发出去的，
// 真正"能不能用"只有浏览器执行一遍才知道。
//
// 本脚本只做一件事：打开面板，把 CSP 相关的加载失败全部收集起来。
// console 里的 "Refused to ..." 是 CSP 拦截的浏览器原生报错，
// pageerror 是脚本执行中断 —— 两者任一非空即判定失败。
//
// ## 用法
//
//   先启动 omnigate，再：
//   NODE_PATH=<node_modules 路径> PANEL_URL=http://127.0.0.1:7863/ \
//     node tools/csp_smoke.js
//
// 退出码 0 = 通过。
'use strict';

const { chromium } = require('playwright');

const URL = process.env.PANEL_URL || 'http://127.0.0.1:7899/';
const BROWSER = process.env.PANEL_BROWSER ||
  'C:/Program Files (x86)/Microsoft/Edge/Application/msedge.exe';

(async () => {
  const violations = [];
  const browser = await chromium.launch({ executablePath: BROWSER, headless: true });
  const page = await browser.newPage();

  // CSP 拦截在浏览器里表现为 console error（"Refused to execute inline
  // script"、"Refused to load the image..."），单独归一类便于报告。
  page.on('console', (m) => {
    if (m.type() !== 'error') return;
    const t = m.text();
    if (/Refused to|Content Security Policy/i.test(t)) {
      violations.push('CSP: ' + t);
    } else {
      violations.push('console: ' + t);
    }
  });
  page.on('pageerror', (e) => violations.push('pageerror: ' + e.message));

  const resp = await page.goto(URL, { waitUntil: 'networkidle', timeout: 30000 });

  const headers = resp.headers();
  const info = await page.evaluate(() => ({
    title: document.title,
    // 关键：app.js 必须真正执行完 —— bind() 里的代码不跑，onclick 全为 null
    appJsExecuted: typeof state !== 'undefined',
    bodyChildren: document.body.children.length,
    styleApplied: (() => {
      const el = document.querySelector('.acct-card, #btnSettings, .card, header');
      if (!el) return 'no-target-el';
      return getComputedStyle(el).fontFamily || 'empty';
    })(),
    cssLoaded: (() => {
      try {
        // 任意一条 CSS 专属规则命中即证明 style.css 生效
        for (const ss of document.styleSheets) {
          try { if (ss.cssRules.length > 0) return true; } catch (e) { /* cross-origin */ }
        }
        return false;
      } catch (e) { return 'err:' + e.message; }
    })(),
    hasInlineStyleApplied: (() => {
      const el = document.querySelector('[style]');
      return el ? el.getAttribute('style').slice(0, 40) : 'none';
    })(),
  }));

  await browser.close();

  console.log('=== 响应头 ===');
  console.log('  CSP:', headers['content-security-policy'] || '(缺失)');
  console.log('  X-Frame-Options:', headers['x-frame-options'] || '(缺失)');
  console.log('=== 页面 ===');
  for (const [k, v] of Object.entries(info)) console.log(`  ${k}: ${v}`);

  // CSP 头缺失本身就是失败 —— 那说明中间件没接上。
  if (!headers['content-security-policy']) {
    console.error('\nFAIL: 响应没有 CSP 头，中间件未生效');
    process.exit(1);
  }
  // app.js 没执行 = 面板完全不可用，这是最严重的回归。
  if (!info.appJsExecuted) {
    console.error('\nFAIL: app.js 未执行（state 未定义），面板不可用');
    process.exit(1);
  }
  // style.css 没生效 = CSP 把样式拦了。
  if (info.cssLoaded === false) {
    console.error('\nFAIL: style.css 未生效，CSP 误拦了自身资源');
    process.exit(1);
  }
  if (violations.length) {
    console.error('\nFAIL: 检测到 ' + violations.length + ' 条违规/错误：');
    violations.forEach(v => console.error('  - ' + v));
    process.exit(1);
  }
  console.log('\nOK: CSP 已生效且未破坏面板渲染');
})().catch((e) => { console.error('FAIL: ' + e.message); process.exit(1); });
