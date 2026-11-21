const test = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const vm = require("node:vm");

function setup(versionResponse, ui = {}) {
  const source = fs.readFileSync(path.join(__dirname, "../static/pages.js"), "utf8");
  const start = source.indexOf("PAGES.apps = {");
  const end = source.indexOf("/* ================= 数据库", start);
  assert.ok(start >= 0 && end > start);
  const controls = {};
  const calls = [];
  function control(selector) {
    if (controls[selector]) return controls[selector];
    let html = "";
    return controls[selector] = {
      disabled: false, value: "", textContent: "",
      get innerHTML() { return html; },
      set innerHTML(v) { html = v; const option = v.match(/<option value="([^"]*)"/); this.value = option ? option[1] : ""; },
    };
  }
  const page = {};
  const ctx = vm.createContext({
    PAGES: {}, esc: (s) => s, appIcon: () => "", modal: () => ({el: {}, close() {}, setVeto() {}}),
    qs: (selector) => control(selector), qsa: (selector) => ui.groups?.[selector] || [],
    toast() {}, taskModal: ui.taskModal || (() => {}),
    api: (url, options) => {
      calls.push({url, options});
      if (ui.responses?.[url]) return Promise.resolve(ui.responses[url]);
      if (url === "/apps/php/versions") return typeof versionResponse === "function" ? versionResponse() : Promise.resolve(versionResponse || {"8.3":"8.3.37", "8.4":"8.4.28", "8.5":"8.5.13", "8.6":"8.6.0"});
      return Promise.resolve({id: "task"});
    },
  });
  vm.runInContext(source.slice(start, end), ctx);
  return {apps: ctx.PAGES.apps, page, control, calls};
}

test("native PHP installs the selected official patch release", async () => {
  const {apps, page, control, calls} = setup();
  apps.installNativePHPDialog(page);
  await new Promise(setImmediate);
  const select = control("#native-php-version");
  assert.match(select.innerHTML, /8\.6\.0/);
  assert.match(select.innerHTML, /8\.3\.37/);
  select.value = "8.4.28";
  control('[data-act="yes"]').onclick();
  const request = calls.find((c) => c.url === "/apps/install");
  assert.equal(request.options.body.key, "php");
  assert.equal(request.options.body.params.Version, "8.4.28");
});

test("native PHP extension install submits only the catalog name to the selected runtime", async () => {
  const base = "/apps/instances/php84/runtime";
  const tab = {getAttribute: () => "extensions", classList: {toggle() {}}};
  const button = {getAttribute: () => "redis", hasAttribute: () => false};
  let task;
  const {apps, control, calls} = setup(undefined, {
    groups: {"[data-native-php-tab]": [tab], "button[data-ext]": [button]},
    responses: {
      [base]: {version: "8.4.28", status: "running", extensions: ["gd"]},
      [base + "/php-ext-catalog"]: [{name: "redis", source: "https://github.com/phpredis/phpredis", loaded: false}],
      [base + "/php-ext"]: {task_id: "extension-task"},
    },
    taskModal: (id) => { task = id; },
  });
  apps.nativePHPManagerDialog("php84", {params: {Version: "8.4.28"}}, {});
  tab.onclick();
  await new Promise(setImmediate);
  assert.match(control("#native-php-view").innerHTML, /redis/);
  await button.onclick();
  const request = calls.find((c) => c.url === base + "/php-ext");
  assert.equal(request.options.method, "POST");
  assert.equal(JSON.stringify(request.options.body), '{"ext":"redis"}');
  assert.equal(task, "extension-task");
});

test("native PHP disabled functions loads existing settings and can save an empty list", async () => {
  const base = "/apps/instances/php84/runtime";
  const tab = {getAttribute: () => "functions", classList: {toggle() {}}};
  const {apps, control, calls} = setup(undefined, {
    groups: {"[data-native-php-tab]": [tab]},
    responses: {
      [base]: {version: "8.4.28", status: "running"},
      [base + "/php-disabled-functions-get"]: {functions: ["exec", "system"]},
      [base + "/php-disabled-functions-save"]: {functions: [], message: "已保存禁用函数"},
    },
  });
  apps.nativePHPManagerDialog("php84", {params: {Version: "8.4.28"}}, {});
  tab.onclick();
  await new Promise(setImmediate);
  const editor = control("#native-php-editor");
  assert.equal(editor.value, "exec\nsystem");
  editor.value = "";
  const save = control("#native-php-save");
  await save.onclick();
  const request = calls.find((c) => c.url === base + "/php-disabled-functions-save");
  assert.equal(request.options.body.content, "");
  assert.equal(editor.value, "");
});

test("native PHP upgrade stays on the installed branch", async () => {
  const {apps, page, control, calls} = setup();
  apps.installNativePHPDialog(page, {key: "php83", params: {Version: "8.3.36", Minor: "8.3"}});
  await new Promise(setImmediate);
  const select = control("#native-php-version");
  assert.match(select.innerHTML, /8\.3\.37/);
  assert.doesNotMatch(select.innerHTML, /8\.4\.28/);
  control('[data-act="yes"]').onclick();
  const request = calls.find((c) => c.url === "/apps/install");
  assert.equal(request.options.body.params.Version, "8.3.37");
  assert.equal(request.options.body.params.UpgradeKey, "php83");
});

test("a partial official response remains usable and names failed branches", async () => {
  const {apps, page, control} = setup({"8.4": "8.4.28", "8.6": "8.6.0"});
  apps.installNativePHPDialog(page);
  await new Promise(setImmediate);
  assert.equal(control('[data-act="yes"]').disabled, false);
  assert.match(control("#native-php-version-help").textContent, /8\.5/);
  assert.match(control("#native-php-version-help").textContent, /8\.3/);
  assert.match(control("#native-php-version-help").textContent, /获取失败/);
});

test("upgrading an already current branch does not submit an install", async () => {
  const {apps, page, control, calls} = setup();
  apps.installNativePHPDialog(page, {key: "php83", params: {Version: "8.3.37", Minor: "8.3"}});
  await new Promise(setImmediate);
  assert.equal(control('[data-act="yes"]').disabled, true);
  assert.match(control("#native-php-version-help").textContent, /已是官方最新/);
  control('[data-act="yes"]').onclick();
  assert.equal(calls.some((c) => c.url === "/apps/install"), false);
});

test("a slower earlier refresh cannot overwrite the latest versions", async () => {
  const pending = [];
  const {apps, page, control} = setup(() => new Promise((resolve) => pending.push(resolve)));
  apps.installNativePHPDialog(page);
  control('[data-act="retry"]').onclick();
  pending[1]({"8.4": "8.4.28"});
  await new Promise(setImmediate);
  pending[0]({"8.4": "8.4.27"});
  await new Promise(setImmediate);
  assert.match(control("#native-php-version").innerHTML, /8\.4\.28/);
  assert.doesNotMatch(control("#native-php-version").innerHTML, /8\.4\.27/);
});
