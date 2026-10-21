// 版本号显示验证（tools/version_smoke.js）
//
// ## 为什么需要它
//
// 版本号有**两个**必须一致的出口：
//
//   1. 面板 logo 旁与「关于」面板 —— 走 /api/state 下发 `app.Version`，
//      前端拼接 "v" 前缀；
//   2. 文件属性（资源管理器 → 右键 → 属性 → 详细信息）—— 来自 exe 内嵌的
//      RT_VERSION 资源，由 build/make_syso.py 生成。
//
// 这两个出口曾长期不一致：代码里是 0.1.0，RT_VERSION 里写死 1.0.0.0，
// 而 curl 后端正常、`go build` 正常、测试全绿 —— 没有任何一处会报错。
// 只有真实浏览器 + 文件属性才能发现。
//
// 本脚本只验第 1 个出口（页面显示）。第 2 个出口由 build/check_res.py
// 与发布前的二进制解析验证 —— 页面脚本读不到文件属性。
//
// ## 用法
//
//   启动 omnigate 后：
//   NODE_PATH=<node_modules> PANEL_URL=http://127.0.0.1:7863/ \
//     EXPECT_VERSION=0.1.2 node tools/version_smoke.js
//
// 不传 EXPECT_VERSION 时只打印当前显示值，不做断言。
'use strict';

const { chromium } = require('playwright');

const URL = process.env.PANEL_URL || 'http://127.0.0.1:7863/';
const EXPECT = process.env.EXPECT_VERSION || '';
const BROWSER = process.env.PANEL_BROWSER ||
  'C:/Program Files (x86)/Microsoft/Edge/Application/msedge.exe';

(async () => {
  const browser = await chromium.launch({ executablePath: BROWSER, headless: true });
  const page = await browser.newPage();
  await page.goto(URL, { waitUntil: 'networkidle', timeout: 30000 });
  // 版本号在 loadState() 之后才渲染，等 #ver 有内容
  await page.waitForFunction(
    () => { const e = document.getElementById('ver'); return e && e.textContent.trim(); },
    { timeout: 15000 }
  ).catch(() => {});

  const shown = await page.evaluate(() => {
    const ver = document.getElementById('ver');
    const about = document.getElementById('aboutVer');
    return {
      header: ver ? ver.textContent.trim() : '(元素不存在)',
      about: about ? about.textContent.trim() : '(未打开关于面板)',
      stateVer: (typeof state !== 'undefined' && state) ? state.version : '(state 未定义)',
    };
  });
  await browser.close();

  console.log('logo 旁显示 : ' + shown.header);
  console.log('state.version: ' + shown.stateVer);

  // 头部必须带 "v" 前缀且与 state 一致
  const want = 'v' + shown.stateVer;
  if (shown.header !== want) {
    console.error(`FAIL: 头部显示 "${shown.header}"，期望 "${want}"`);
    process.exit(1);
  }
  if (EXPECT && shown.stateVer !== EXPECT) {
    console.error(`FAIL: 版本是 ${shown.stateVer}，期望 ${EXPECT}`);
    process.exit(1);
  }
  console.log('OK: ' + shown.header);
})().catch((e) => { console.error('FAIL: ' + e.message); process.exit(1); });
