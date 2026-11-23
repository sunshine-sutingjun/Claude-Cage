import React, { useEffect, useImperativeHandle, useLayoutEffect, useRef, useState } from "react";
import { DesktopToolbar, useDesktopZoom } from "./claude-desktop";
import { MacOSLogin } from "./macos-login";
import { api, native, nativeAvailable } from "./api";
import { BrowserFavorites } from "./browser-favorites";
import { BrowserUploads } from "./browser-uploads";
import { sendFiles } from "./file-transfer";
import { prepareClipboardPaste, sendImage } from "./image-transfer";
import { installRemoteInput } from "./remote-input";
import { createMacWindowInput } from "./macos-window-input.mjs";
import { copyGuestText } from "./shared-clipboard.mjs";
import { useWindowVisibility } from "./window-visibility";
import { createMacWindowConnection } from "./macos-window-connection.mjs";
import { createMacFrameConnection } from "./macos-frame-connection.mjs";
import "./macos-claude-window.css";

const messages = {
  requires_gui_login: "请先登录并解锁虚拟 Mac 的图形账户。",
  screen_recording_permission: "请在虚拟 Mac 的系统授权中，允许 Cage Guest Window 录制屏幕。",
  accessibility_permission: "要操作窗口，请允许虚拟 Mac 中的 Cage Guest Window 使用辅助功能。",
  not_running: "请打开虚拟 Mac 中的应用。",
  no_visible_window: "应用尚未显示窗口，请重新打开应用。",
  identity_error: "虚拟 Mac 中的应用未通过签名检查。",
  window_changed: "活动窗口已变化，请在新画面中重试。",
  unknown_action: "客机窗口输入组件需要更新，请在环境设置中更新组件后重试。",
  input_connection_required: "窗口输入需要重新连接，请恢复画面后重试。",
  input_owned_by_another_view: "另一个连接正在操作此窗口，请等待它释放按键或拖动。",
};

async function windowRequest(base, action, fields = {}, connection) {
  const result = connection ? await connection.request(action, fields) : await api(base + "/window", { action, ...fields });
  if (result.error) {
    const error = new Error(messages[result.error] || "未能读取或操作虚拟 Mac 中的应用窗口。");
    error.code = result.error;
    throw error;
  }
  return result;
}

// Fixed-size native sign-in sheets stay at their real text size. Extend a
// uniform window background across the pane without scaling those pixels.
function matchWindowBackground(surface, img, pane, canvasRef) {
  if (!pane) return;
  if (surface.clientWidth >= pane.clientWidth - 1 && surface.clientHeight >= pane.clientHeight - 1) return;
  if (!canvasRef.current) {
    canvasRef.current = document.createElement("canvas");
    canvasRef.current.width = 4;
    canvasRef.current.height = 1;
  }
  const canvas = canvasRef.current;
  const ctx = canvas.getContext("2d", {willReadFrequently:true});
  if (!ctx) return;
  const w = img.naturalWidth, h = img.naturalHeight;
  if (w < 16 || h < 16) return;
  [[8,8],[w-9,8],[8,h-9],[w-9,h-9]].forEach(([x,y],i) =>
    ctx.drawImage(img,x,y,1,1,i,0,1,1));
  const pixels = ctx.getImageData(0,0,4,1).data;
  const uniform = pixels.every((value, index) => Math.abs(value - pixels[index % 4]) <= 4);
  pane.style.backgroundColor = uniform && pixels[3] === 255 ? `rgb(${pixels[0]}, ${pixels[1]}, ${pixels[2]})` : "";
}

export function MacOSAppWindow({ browser = false, onOpenPage, visible, ready = true, keyboard = "mac", remoteRef, sidebarCollapsed, onExpand, onConfirm, uploads = [], onStatus = () => {}, onWindowStatus, onFallback = () => {} }) {
  const appName = browser ? "Google Chrome" : "Claude Desktop";
  const base = browser ? "/macos-browser" : "/macos/claude";
  const windowVisible = useWindowVisibility();
  const screenVisible = visible && windowVisible;
  const connected = screenVisible && ready;
  const connection = useRef(null);
  const [inputChannel, setInputChannel] = useState(null);
  const frameConnection = useRef(null);
  const refreshFrame = useRef(null);
  const request = (action, fields) => windowRequest(base, action, fields, connection.current);
  const [zoom, setZoom] = useDesktopZoom();
  const [state, setState] = useState(null);
  const [error, setError] = useState("");
  const [fileError, setFileError] = useState("");
  const feedback = ready ? error || fileError : "正在重新连接虚拟 Mac…";
  useEffect(() => {
    if (!screenVisible || !feedback || !onWindowStatus) return;
    onWindowStatus(feedback);
    return () => onWindowStatus("");
  }, [screenVisible, feedback, onWindowStatus]);
  const [transferring, setTransferring] = useState(false);
  const [revision, refresh] = useState(0);
  const [inputTarget, setInputTarget] = useState("");
  const displayed = useRef(null);
  const frame = useRef(null);
  const surface = useRef(null);
  const controls = useRef(null);
  const view = useRef(null);
  view.current = { connected, keyboard, input: state?.input, onStatus, onFallback };
  const pane = useRef(null);
  const backgroundSample = useRef(null);
  const viewport = useRef(null);
  useLayoutEffect(() => {
    const element = pane.current;
    if (!visible || !element) { viewport.current = null; return; }
    const measure = () => {
      const { width, height } = element.getBoundingClientRect();
      if (width <= 0 || height <= 0) return;
      const next = {
        width: Math.min(2560, Math.max(320, Math.round(width / zoom))),
        height: Math.min(1600, Math.max(240, Math.round(height / zoom))),
        scale: Math.max(1, Math.min(2, window.devicePixelRatio || 1)),
      };
      if (viewport.current?.width === next.width && viewport.current?.height === next.height && viewport.current?.scale === next.scale) return;
      viewport.current = next;
      frame.current = null;
      frameConnection.current?.resize(next);
    };
    measure();
    const observer = new ResizeObserver(measure);
    observer.observe(element);
    return () => observer.disconnect();
  }, [visible, zoom]);
  const picker = useRef(null);
  const pickerTarget = useRef(null);
  const generation = useRef(0);
  const launched = useRef(false);
  const login = useRef(undefined);

  useLayoutEffect(() => {
    if (screenVisible) return;
    if (browser) launched.current = false;
    displayed.current = null;
    if (surface.current) surface.current.width = 0;
    if (pane.current) pane.current.style.backgroundColor = "";
  }, [screenVisible, browser]);

  useEffect(() => {
    if (!connected) return;
    const epoch = ++generation.current;
    let timer;
    let current, nextStatus = 0, after = "", captureSize = "", checkFrame = true;
    let captureRevision = 0;
    const live = () => generation.current === epoch && view.current.connected;
    refreshFrame.current = () => {
      captureRevision++;
      after = "";
      checkFrame = true;
      frameConnection.current?.refresh();
    };
    function invalidate(clearPixels) {
      frame.current = null;
      setInputTarget("");
      if (clearPixels) {
        displayed.current = null;
        if (surface.current) surface.current.width = 0;
        if (pane.current) pane.current.style.backgroundColor = "";
      }
    }
    function frameError(code, message) {
      invalidate(["requires_gui_login", "screen_recording_permission", "identity_error", "not_running", "no_visible_window"].includes(code));
      if (code !== "window_changed") setError(message);
    }
    async function receiveFrame(next, size, requestedRevision) {
      const valid = () => live() && (next.isCurrent ? next.isCurrent() : viewport.current === size && requestedRevision === captureRevision);
      if (!valid()) return;
      if (displayed.current && displayed.current.pid !== next.pid) invalidate(true);
      const target = { window: next.window, pid: next.pid, geometry: next.geometry, surface: browser ? "browser" : undefined };
      const identity = JSON.stringify(target);
      if (displayed.current?.identity !== identity) frame.current = null;
      const img = new Image();
      img.src = next.src || `data:${next.mime === "image/jpeg" ? "image/jpeg" : "image/png"};base64,${next.image}`;
      try { await img.decode(); }
      catch (error) { if (valid()) throw error; return; }
      const canvas = surface.current;
      if (!valid() || !canvas) return;
      const ctx = canvas.getContext("2d");
      if (!ctx) throw new Error("窗口画面无法显示，请重新连接。");
      // Change the visible backing store only after the replacement has decoded.
      if (canvas.width !== img.naturalWidth) canvas.width = img.naturalWidth;
      if (canvas.height !== img.naturalHeight) canvas.height = img.naturalHeight;
      ctx.globalCompositeOperation = "copy";
      ctx.drawImage(img, 0, 0);
      const width = next.contentWidth || img.naturalWidth, height = next.contentHeight || img.naturalHeight;
      canvas.style.setProperty("--frame-width", width + "px");
      canvas.style.setProperty("--frame-aspect", width / height);
      matchWindowBackground(canvas, img, pane.current, backgroundSample);
      displayed.current = { pid: next.pid, identity };
      frame.current = target;
      setInputTarget(identity);
      setError("");
      next.acknowledge?.();
    }
    async function poll() {
      let delay = 2200;
      try {
        if (!current || Date.now() >= nextStatus) {
          current = await request("status");
          if (!live()) return;
          if (browser && login.current !== undefined && login.current !== current.login) {
            frameConnection.current?.close();
            frameConnection.current = null;
            invalidate(true);
            after = "";
          }
          login.current = current.login;
          if (browser && current.surface !== "browser") throw new Error("请更新 macOS 环境组件以显示 Chrome 窗口。");
          checkFrame = true;
          nextStatus = Date.now() + 2200;
          if (live() && current.persistent && !connection.current) {
            connection.current = createMacWindowConnection(base);
            setInputChannel(connection.current);
          }
        }
        if (!live()) return;
        setState(current);
        // Opening this tab focuses the signed guest application. A background
        // Electron window can still be captured but cannot receive targeted input.
        if (current.installed && !launched.current) {
          launched.current = true;
          await request("launch");
          nextStatus = 0;
          delay = 100;
          return;
        }
        if (current.running && current.screen) {
          if (current.push_frames && viewport.current) {
            if (!frameConnection.current) {
              frameConnection.current = createMacFrameConnection(next => {
                receiveFrame(next, viewport.current).catch(() => {
                  if (!live() || !next.isCurrent()) return;
                  frameConnection.current?.close();
                  frameConnection.current = null;
                  frameError("decode_failed", "窗口画面无法解码，请重新连接。");
                });
              }, reason => {
                if (!live()) return;
                frameConnection.current = null;
                nextStatus = 0;
                frameError(reason, messages[reason] || "未能读取虚拟 Mac 的窗口画面，请重新连接。");
              }, base);
            }
            frameConnection.current.resize(viewport.current);
            return;
          }
          const size = viewport.current;
          const requestedRevision = captureRevision;
          const nextSize = JSON.stringify(size);
          if (nextSize !== captureSize) { after = ""; captureSize = nextSize; }
          const next = await request("frame", viewport.current ? {
            after, refresh: checkFrame, ...size,
          } : {});
          if (!live() || requestedRevision !== captureRevision) return;
          checkFrame = false;
          delay = current.persistent ? 50 : 350;
          if (next.unchanged) return;
          after = next.sequence || "";
          await receiveFrame(next, size, requestedRevision);
        } else { frameConnection.current?.close(); frameConnection.current = null; invalidate(true); }
        setError("");
      } catch (e) {
        if (!live()) return;
        connection.current?.close(); connection.current = null;
        setInputChannel(null);
        frameConnection.current?.close(); frameConnection.current = null;
        current = null; after = "";
        frameError(e.code, e.message);
      } finally { if (live()) timer = setTimeout(poll, delay); }
    }
    poll();
    return () => {
      generation.current++; clearTimeout(timer); invalidate(false);
      refreshFrame.current = null;
      connection.current?.close(); connection.current = null;
      setInputChannel(null);
      frameConnection.current?.close(); frameConnection.current = null;
    };
  }, [connected, revision, base]);

  async function action(name) {
    try { if (name === "stop") launched.current = true; await request(name); setError(""); refresh((n) => n + 1); }
    catch (e) { setError(e.message); }
  }
  const reportInputError = error => {
    if (error.code !== "window_changed") setError(error.message);
  };
  useImperativeHandle(remoteRef, () => ({
    focus: () => controls.current?.transport.focus(),
    closeNativeTab: () => controls.current?.transport.runKeyAction(async send => {
      for (const [symbol, code, down] of [[0xffeb, "MetaLeft", true], [119, "KeyW", true], [119, "KeyW", false], [0xffeb, "MetaLeft", false]]) {
        await send(symbol, code, down);
      }
    }).catch(reportInputError),
    edit: command => controls.current?.input.focused() ? controls.current.input.edit(command) : false,
    copy: () => controls.current?.input.copy(),
    paste: () => controls.current?.input.readAndPaste(),
    pasteText: text => controls.current?.input.paste(text),
    transfer: () => onFallback("", "可在此使用本机复制、粘贴。"),
  }));

  useEffect(() => {
    if (!connected || !inputTarget || !state?.input || !surface.current || !pane.current || (state.persistent && !inputChannel)) return;
    const inputRequest = async (action, fields) => {
      const target = frame.current;
      try { return await windowRequest(base, action, fields, inputChannel); }
      catch (error) {
        if (error.code === "window_changed" && target && frame.current === target &&
            fields?.geometry === target.geometry &&
            !(action === "ime" && fields.mode === "status") &&
            !(action === "pointer" && fields.phase === "move" && !fields.buttons)) {
          frame.current = null;
          refreshFrame.current?.();
        }
        throw error;
      }
    };
    const transport = createMacWindowInput({
      element: surface.current,
      frame: () => frame.current,
      active: () => Boolean(view.current.connected && view.current.input && frame.current),
      request: inputRequest,
      onError: reportInputError,
    });
    const preparePaste = () => {
      const lease = { target: transport.target(), epoch: generation.current, transport };
      return Object.assign(prepareClipboardPaste("desktop", "", { windowTarget: lease.target, valid: () => transport.valid(lease.target) }), { lease });
    };
    const input = installRemoteInput(transport, pane.current, {
      mode: () => view.current.keyboard,
      guestPlatform: () => "darwin",
      systemClipboard: () => true,
      copyText: (sendCopy, valid) => copyGuestText({ request: value => api("/clipboard/copy", value), sendCopy, valid }),
      surface: () => surface.current,
      inputLabel: "向 " + appName + " 输入",
      tabId: () => frame.current ? `${frame.current.pid}:${frame.current.window}:${frame.current.geometry}` : "",
      isActive: () => Boolean(view.current.connected && view.current.input && frame.current),
      onStatus: text => view.current.onStatus(text),
      onFallback: (...values) => view.current.onFallback(...values),
      insertText: text => transport.send("text", { text }),
      caretRect: session => inputRequest("ime", { ...frame.current, mode: "status", session }).catch(error => {
        if (error.code === "unknown_action") setError(error.message);
        return { available: false };
      }),
      insertComposition: (_, value) => transport.send("ime", value).then(result => {
        if (value.mode !== "cancel" && !result.available) throw new Error("输入位置已变化，请重新输入");
        return result;
      }),
      preparePaste,
      pasteContent: (content, prepared) => receive(content, prepared?.lease || { target: transport.target(), epoch: generation.current, transport }, prepared),
    });
    controls.current = { transport, input };
    return () => {
      input.dispose(); transport.dispose();
      if (controls.current?.transport === transport) controls.current = null;
    };
  }, [connected, state?.input, state?.persistent, inputChannel, inputTarget]);

  function receive(content, lease, prepared) {
    const target = lease?.target || frame.current;
    const epoch = lease?.epoch ?? generation.current;
    const transport = lease?.transport || controls.current?.transport;
    const valid = () => target && epoch === generation.current && transport?.valid(target);
    const ready = Promise.resolve(content).then(value => ({ value }), error => ({ error }));
    if (!transport) { prepared?.dispose(); return Promise.reject(new Error("窗口尚未连接，请稍后重试")); }
    return transport.run(async () => {
      const { value, error } = await ready;
      if (error) throw error;
      if (!valid()) throw new Error("窗口已切换，请回到原窗口重新粘贴");
      setFileError(""); setTransferring(true);
      if (value.files || value.fileSelection) {
        await sendFiles(value, "desktop", "", { valid, windowTarget: target, onProgress: (loaded, total) => onStatus(total ? `正在传递附件 · ${Math.floor(loaded * 100 / total)}%` : "正在传递附件…") });
      } else if (value.png) {
        await sendImage(value, "desktop", "", { prepared });
      } else if (value.text) {
        await request("text", { ...target, text: value.text });
      }
      if (valid()) onStatus(value.text ? "已粘贴" : `附件已交给 ${appName}，请在应用中确认上传结果`);
    }).finally(() => { prepared?.dispose(); setTransferring(false); });
  }
  const fileFailure = error => { setFileError(error.message); onStatus(error.message); };
  async function chooseFiles() {
    const lease = { target: frame.current, epoch: generation.current, transport: controls.current?.transport };
    if (!nativeAvailable()) { pickerTarget.current = lease; picker.current?.click(); return; }
    try {
      const selected = await native("chooseUploadFiles", { multiple: true });
      if (!selected.canceled) await receive({ fileSelection: selected.fileSelection, kind: "paste" }, lease);
    } catch (error) { fileFailure(error); }
  }
  if (!visible) return null;
  return <section className="claude-desktop-view mac-claude" aria-label={"虚拟 Mac 中的 " + appName}>
    {ready && (browser || !nativeAvailable()) && <BrowserUploads requests={uploads} onStatus={onStatus} />}
    <div inert={!ready}>
      <DesktopToolbar title={appName} desktop={state} platform="macOS" zoom={zoom} setZoom={setZoom} onConfirm={onConfirm}
        onStop={browser ? undefined : () => action("stop")}
        refresh={state?.running ? () => action("launch") : () => refresh(n => n+1)}
        refreshLabel={state?.running ? "重新连接 " + appName : undefined} sidebarCollapsed={sidebarCollapsed} onExpand={onExpand}>
        {browser && <BrowserFavorites ready={Boolean(state?.installed)} onOpen={onOpenPage} />}
        <MacOSLogin />
        {!browser && <button disabled={!state?.input || !inputTarget || transferring} onClick={() => void chooseFiles()}>添加附件</button>}
        {!browser && <input ref={picker} type="file" multiple hidden aria-label="选择 Claude 附件" onChange={event => { const files = Array.from(event.target.files || []); event.target.value = ""; if (files.length) void receive({ files, kind: "paste" }, pickerTarget.current || undefined).catch(fileFailure); }} />}
        {!state?.running && <button onClick={() => action("launch")} disabled={!state?.installed}>打开应用</button>}
        {state?.installed && !state.screen && <button onClick={() => action("request-screen")}>授权窗口画面</button>}
        {state?.installed && !state.input && <button onClick={() => action("request-input")}>授权键鼠</button>}
      </DesktopToolbar>
    </div>
    {state && !state.installed && <p>此虚拟 Mac 尚未安装 {appName}。</p>}
    {state?.running && !state.screen && <p>{messages.screen_recording_permission}</p>}
    {state?.running && state.screen && !state.input && <p>{messages.accessibility_permission}</p>}
    <div ref={pane} className="mac-claude-frame" style={{ "--desktop-zoom": zoom }}>
      <canvas ref={surface} width={0} height={0} role="img" aria-label={appName + " 客机窗口"} />
    </div>
  </section>;
}
