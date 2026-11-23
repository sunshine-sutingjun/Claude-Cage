import assert from "node:assert/strict";
import { createWorkbench } from "./workbench-fixture.mjs";

const chrome = process.argv.includes("--chrome");
const base = chrome ? "/api/macos-browser" : "/api/macos/claude";
const actions = [], subscriptions = [], acknowledgements = [];
let activeSocket, image, alternate, rejectInput = false, closed = 0;
const status = { installed: true, running: true, screen: true, input: true, persistent: true, push_frames: true };
function frame(sequence, revision, geometry = "geometry-1", pixels = image) {
  const header = Buffer.from(JSON.stringify({
    imageLength: pixels.length, mime: "image/png", sequence, revision, geometry,
    window: 70, pid: 23, contentWidth: 400, contentHeight: 300, pixelWidth: 800, pixelHeight: 600,
  }));
  const size = Buffer.alloc(4);
  size.writeUInt32BE(header.length);
  activeSocket.send(Buffer.concat([size, header, pixels]));
}
function frameFailure(error) {
  const header = Buffer.from(JSON.stringify({ error, revision: subscriptions.at(-1).revision, imageLength: 0 }));
  const size = Buffer.alloc(4);
  size.writeUInt32BE(header.length);
  activeSocket.send(Buffer.concat([size, header]));
}
const { page, browser, state, errors } = await createWorkbench({
  async beforeNavigate(page, state) {
    state.account.guest_system = state.status.guest_system = "macos";
    if (chrome) {
      status.surface = "browser";
      status.login = true;
      state.tabs = [{ id: "native-login", kind: "web", title: "账号授权", native_window: true }];
      state.active = "native-login";
    }
    state.status.vm_running = state.status.ready = true;
    [image, alternate] = (await page.evaluate(() => {
      const canvas = document.createElement("canvas");
      canvas.width = 800; canvas.height = 600;
      const context = canvas.getContext("2d");
      return ["#f7f5f1", "#1278a2"].map(color => {
        context.fillStyle = color; context.fillRect(0, 0, 800, 600);
        return canvas.toDataURL().split(",")[1];
      });
    })).map(value => Buffer.from(value, "base64"));
    await page.addInitScript(() => {
      const create = URL.createObjectURL.bind(URL), revoke = URL.revokeObjectURL.bind(URL);
      window.frameURLs = new Set();
      URL.createObjectURL = blob => { const url = create(blob); window.frameURLs.add(url); return url; };
      URL.revokeObjectURL = url => { window.frameURLs.delete(url); revoke(url); };
    });
    await page.route("**" + base + "/window", route => {
      assert.equal(route.request().postDataJSON().action, "status");
      return route.fulfill({ json: status });
    });
    await page.routeWebSocket("**" + base + "/connection**", socket => {
      socket.onClose(() => { closed++; });
      socket.onMessage(data => {
        const { id, request } = JSON.parse(data);
        actions.push(request.action);
        assert.notEqual(request.action, "frame", "capture waits must use the separate frame channel");
        socket.send(JSON.stringify({ id, result: rejectInput && request.action === "key" ? { error: "window_changed" } : status }));
      });
    });
    await page.routeWebSocket("**" + base + "/frames**", socket => {
      activeSocket = socket;
      socket.onClose(() => { closed++; });
      socket.onMessage(data => {
        const request = JSON.parse(data);
        if (request.ack) { acknowledgements.push(request.ack); return; }
        subscriptions.push(request);
        if (request.revision === 1) frame(`initial-${subscriptions.length}`, request.revision);
      });
    });
  },
});
async function until(condition) {
  const deadline = Date.now() + 5000;
  while (!condition()) {
    assert(Date.now() < deadline, "timed out waiting for a frame channel event");
    await new Promise(resolve => setTimeout(resolve, 20));
  }
}
try {
  await page.getByRole("button", { name: chrome ? "打开浏览器" : "Claude Desktop", exact: true }).click();
  await page.waitForFunction(() => document.querySelector(".mac-claude-frame canvas")?.width === 800);
  await until(() => acknowledgements.length === 1);
  const pixels = () => page.locator(".mac-claude-frame canvas").evaluate(el => [...el.getContext("2d").getImageData(0, 0, 1, 1).data]);
  const original = await pixels();
  assert.deepEqual(original, [247, 245, 241, 255]);
  await page.locator(".mac-claude-frame canvas").click();
  await page.locator(".mac-claude-typing").press("ArrowRight");
  await until(() => actions.includes("pointer") && actions.includes("key"));
  await new Promise(resolve => setTimeout(resolve, 350));
  assert.equal(subscriptions.length, 1, "static windows must retain one subscription");
  assert.equal(acknowledgements.length, 1, "static windows must not request repeated images");
  const pane = page.locator(".mac-claude-frame");
  const beforeError = await pane.boundingBox();
  const resets = actions.filter(action => action === "input-reset").length;
  const launches = actions.filter(action => action === "launch").length;
  rejectInput = true;
  await page.locator(".mac-claude-typing").press("ArrowLeft");
  await until(() => actions.filter(action => action === "input-reset").length > resets);
  await until(() => subscriptions.length > 1 || actions.filter(action => action === "launch").length > launches);
  assert.equal(await page.getByText("活动窗口已变化，请在新画面中重试。", { exact: true }).count(), 0);
  assert.deepEqual(await pane.boundingBox(), beforeError, "input errors must not resize the guest");
  assert.equal(await page.getByRole("button", { name: "重新连接应用" }).count(), 0);
  assert.equal(actions.filter(action => action === "launch").length, launches);
  assert.deepEqual(await pixels(), original, "the painted canvas must survive a window change");
  const rejectedKeys = actions.filter(action => action === "key").length;
  rejectInput = false;
  const refreshedRevision = subscriptions.at(-1).revision;
  frame("changed", refreshedRevision, "geometry-refreshed");
  await until(() => acknowledgements.includes("changed"));
  assert.equal(actions.filter(action => action === "key").length, rejectedKeys, "disputed input must not be replayed");
  await page.locator(".mac-claude-typing").press("ArrowRight");
  await until(() => actions.filter(action => action === "key").length > rejectedKeys);
  assert.equal(await page.evaluate(() => window.frameURLs.size), 1, "replaced frames must release their blob URLs");
  // Hold a decoded replacement, then resize before it is allowed to paint.
  await page.evaluate(() => {
    const decode = HTMLImageElement.prototype.decode;
    HTMLImageElement.prototype.decode = async function () {
      await decode.call(this);
      HTMLImageElement.prototype.decode = decode;
      await new Promise(resolve => { window.releaseDecode = resolve; });
    };
  });
  frame("decoding", refreshedRevision, "pending-geometry", alternate);
  await page.waitForFunction(() => Boolean(window.releaseDecode));
  assert.deepEqual(await pixels(), original);
  assert(!acknowledgements.includes("decoding"));
  await page.setViewportSize({ width: 1380, height: 900 });
  await until(() => subscriptions.at(-1).revision > refreshedRevision);
  const revision = subscriptions.at(-1).revision;
  frame("stale", revision - 1, "stale-geometry");
  frame("resized", revision, "geometry-2");
  await until(() => acknowledgements.includes("resized"));
  await page.evaluate(() => window.releaseDecode());
  assert.deepEqual(await pixels(), original, "a decoded frame from the old size must not paint");
  assert(!acknowledgements.includes("decoding"));
  assert(!acknowledgements.includes("stale"), "a stale frame must not replace the resized view");
  activeSocket.close();
  await page.getByRole("status").filter({ hasText: "窗口画面" }).waitFor();
  assert.equal(await pane.getByText("窗口画面", { exact: false }).count(), 0);
  assert.deepEqual(await pixels(), original, "a transient disconnect must preserve the painted canvas");
  assert.equal(await page.locator(".mac-claude-typing").count(), 0, "a disconnected canvas must reject input");
  await until(() => acknowledgements.some(ack => ack.startsWith("initial-") && ack !== "initial-1"));
  const launchCount = actions.filter(action => action === "launch").length;
  const activeSubscriptions = subscriptions.length;
  const beforePause = await pane.boundingBox();
  state.status.ready = false;
  await page.getByText("正在重新连接虚拟 Mac…", { exact: true }).waitFor();
  assert.deepEqual(await pixels(), original, "losing guest readiness must retain the visible frame");
  assert.deepEqual(await pane.boundingBox(), beforePause, "reconnecting must preserve the viewport");
  assert.equal(await page.locator(".mac-claude-typing").count(), 0);
  assert.equal(subscriptions.length, activeSubscriptions);
  state.status.ready = true;
  await until(() => subscriptions.length > activeSubscriptions);
  await page.waitForFunction(() => Boolean(document.querySelector(".mac-claude-typing")));
  assert.equal(actions.filter(action => action === "launch").length, launchCount, "reconnecting must not relaunch an authorization");
  await page.evaluate(() => { window.cageWindowVisible = false; window.dispatchEvent(new Event("cage-window-visibility")); });
  await until(() => closed >= 2);
  await page.waitForFunction(() => document.querySelector(".mac-claude-frame canvas")?.width === 0);
  assert.equal(await page.evaluate(() => window.frameURLs.size), 0);
  const hiddenActions = actions.length, hiddenSubscriptions = subscriptions.length;
  await new Promise(resolve => setTimeout(resolve, 300));
  assert.equal(actions.length, hiddenActions);
  assert.equal(subscriptions.length, hiddenSubscriptions);
  await page.evaluate(() => { window.cageWindowVisible = true; window.dispatchEvent(new Event("cage-window-visibility")); });
  await until(() => subscriptions.length > hiddenSubscriptions);
  await page.waitForFunction(() => document.querySelector(".mac-claude-frame canvas")?.width === 800);
  frame("painted", subscriptions.at(-1).revision, "geometry-3", alternate);
  await until(() => acknowledgements.includes("painted"));
  assert.deepEqual(await pixels(), [18, 120, 162, 255], "an ACK must follow the replacement pixels");
  const beforeWindowChange = subscriptions.length;
  frameFailure("window_changed");
  await page.locator(".mac-claude-typing").waitFor({ state: "detached" });
  assert.deepEqual(await pixels(), [18, 120, 162, 255]);
  assert.equal(await page.getByText("活动窗口已变化，请在新画面中重试。", { exact: true }).count(), 0);
  await until(() => subscriptions.length > beforeWindowChange);
  await page.locator(".mac-claude-typing").waitFor();
  frameFailure("identity_error");
  await page.getByRole("status").filter({ hasText: "签名检查" }).waitFor();
  await page.waitForFunction(() => document.querySelector(".mac-claude-frame canvas")?.width === 0);
  assert.deepEqual(errors, []);
  console.log("Frame painting, delayed decode, stable alerts, reconnect, readiness, hide/resume, input and identity checks passed.");
} finally { await browser.close(); }
