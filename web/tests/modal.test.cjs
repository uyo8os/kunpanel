const test = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const vm = require("node:vm");

test("opening a new modal during a close animation keeps it visible", () => {
  const source = fs.readFileSync(path.join(__dirname, "../static/ui.js"), "utf8");
  const modalCode = source.slice(source.indexOf("let _modalStack = []"), source.indexOf("/* ---------- 独立弹层"));
  const classes = new Set();
  const mask = { style: {}, classList: { add: (c) => classes.add(c), remove: (c) => classes.delete(c) } };
  const controls = {};
  const box = {
    style: {}, innerHTML: "",
    classList: { toggle() {} },
    querySelectorAll: () => [],
  };
  const timers = new Map();
  let nextTimer = 0;
  const context = vm.createContext({
    window: { innerWidth: 1200 },
    qs: (selector) => selector === "#modal-mask" ? mask : selector === "#modal-box" ? box : (controls[selector] ||= {}),
    clearBusyBtns() {}, esc: (s) => s, I: () => "",
    setTimeout: (fn) => { const id = ++nextTimer; timers.set(id, fn); return id; },
    clearTimeout: (id) => timers.delete(id),
  });
  vm.runInContext(modalCode + `
    const previous = modal({title: "管理", body: "旧窗口"});
    previous.doClose();
    previous.doClose();
    modal({title: "切换版本", body: "版本选择"});
    previous.doClose();
  `, context);

  for (const callback of timers.values()) callback();
  assert.equal(mask.style.display, "flex");
  assert.equal(classes.has("closing"), false);
  assert.match(box.innerHTML, /版本选择/);

  vm.runInContext(`_modalStack[_modalStack.length - 1]()`, context);
  for (const callback of timers.values()) callback();
  assert.equal(mask.style.display, "none");
  assert.equal(box.innerHTML, "");
});
