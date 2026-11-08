// KahoAgent 桌面客户端 —— 主进程
//
// 设计原则：
//   1. 渲染进程零特权：nodeIntegration=false + contextIsolation=true，
//      所有能力通过 preload 暴露的窄接口进入。
//   2. 网络请求放在主进程：renderer 的 origin 是 file://，
//      直接 fetch 会被 CORS 拦；主进程没有这个限制，
//      顺便可以在这一层统一做超时、取消和错误翻译。
//   3. 配置只存 serverUrl 这类非敏感项，API Key 永远留在服务端。
"use strict";

const { app, BrowserWindow, ipcMain, dialog, shell } = require("electron");
const fs = require("node:fs");
const fsp = require("node:fs/promises");
const path = require("node:path");

const DEFAULT_SERVER = "http://127.0.0.1:8080";
const activeChats = new Map(); // webContents.id -> AbortController

// ---------- 配置 ----------

function configPath() {
    return path.join(app.getPath("userData"), "config.json");
}

function readConfig() {
    try {
        const raw = JSON.parse(fs.readFileSync(configPath(), "utf8"));
        return {
            serverUrl: typeof raw.serverUrl === "string" ? raw.serverUrl : DEFAULT_SERVER,
        };
    } catch {
        return { serverUrl: DEFAULT_SERVER };
    }
}

function writeConfig(next) {
    const cfg = { serverUrl: normalizeServer(next?.serverUrl) };
    fs.mkdirSync(path.dirname(configPath()), { recursive: true });
    fs.writeFileSync(configPath(), JSON.stringify(cfg, null, 2));
    return cfg;
}

// normalizeServer 只接受 http/https。
// 允许 file:// 或自定义协议会让「服务器地址」变成一个任意读文件的入口。
function normalizeServer(url) {
    const s = String(url || "").trim();
    if (!s) return DEFAULT_SERVER;
    let u;
    try {
        u = new URL(s);
    } catch {
        throw new Error("服务器地址格式不正确");
    }
    if (u.protocol !== "http:" && u.protocol !== "https:") {
        throw new Error("只支持 http/https 地址");
    }
    return u.origin + u.pathname.replace(/\/+$/, "");
}

// ---------- 窗口 ----------

function createWindow() {
    const win = new BrowserWindow({
        width: 1240,
        height: 820,
        minWidth: 920,
        minHeight: 620,
        backgroundColor: "#0c0d10",
        title: "KahoAgent",
        autoHideMenuBar: true,
        webPreferences: {
            preload: path.join(__dirname, "preload.js"),
            contextIsolation: true,
            nodeIntegration: false,
            sandbox: true,
        },
    });

    const demo = process.argv.includes("--mock");
    win.loadFile(path.join(__dirname, "renderer", "index.html"), {
        query: demo ? { demo: "1" } : {},
    });
    return win;
}

// ---------- SSE 对话 ----------

function send(win, channel, payload) {
    if (!win.isDestroyed()) win.webContents.send(channel, payload);
}

async function runChat(win, { message, sessionId }) {
    const cfg = readConfig();
    const ctrl = new AbortController();
    activeChats.set(win.webContents.id, ctrl);

    const fail = (err) => {
        const msg = err?.name === "AbortError" ? "已取消" : err?.message || String(err);
        send(win, "chat:end", { ok: false, error: msg });
    };

    try {
        const resp = await fetch(`${cfg.serverUrl}/api/chat`, {
            method: "POST",
            headers: { "Content-Type": "application/json" },
            body: JSON.stringify({ session_id: sessionId, message }),
            signal: ctrl.signal,
        });
        if (!resp.ok || !resp.body) throw new Error(`HTTP ${resp.status}`);

        const reader = resp.body.getReader();
        const decoder = new TextDecoder();
        let buffer = "";

        for (;;) {
            const { value, done } = await reader.read();
            if (done) break;
            buffer += decoder.decode(value, { stream: true });

            // SSE 以空行分隔事件；最后一段可能不完整，留在 buffer。
            let idx;
            while ((idx = buffer.indexOf("\n\n")) >= 0) {
                const raw = buffer.slice(0, idx);
                buffer = buffer.slice(idx + 2);
                for (const line of raw.split("\n")) {
                    if (!line.startsWith("data:")) continue;
                    try {
                        send(win, "chat:event", JSON.parse(line.slice(5).trim()));
                    } catch {
                        // 单个畸形事件忽略，不中断整条流
                    }
                }
            }
        }
        send(win, "chat:end", { ok: true });
    } catch (err) {
        fail(err);
    } finally {
        activeChats.delete(win.webContents.id);
    }
}

// ---------- 上传 ----------

async function uploadFile(win, filePath) {
    const cfg = readConfig();
    const buf = await fsp.readFile(filePath);
    const name = path.basename(filePath);

    const form = new FormData();
    form.append("doc_id", name);
    form.append("file", new Blob([buf]), name);

    const resp = await fetch(`${cfg.serverUrl}/api/upload`, {
        method: "POST",
        body: form,
    });
    const text = await resp.text();
    if (!resp.ok) throw new Error(text.trim() || `HTTP ${resp.status}`);
    try {
        return JSON.parse(text);
    } catch {
        return { raw: text };
    }
}

// ---------- 截图模式（用于自动化验收） ----------

async function shoot(win, outPath) {
    await new Promise((r) => win.webContents.once("did-finish-load", r));
    await new Promise((r) => setTimeout(r, 1800));
    const image = await win.webContents.capturePage();
    await fsp.writeFile(outPath, image.toPNG());
    console.log("screenshot:", outPath);
    app.quit();
}

// ---------- 生命周期 ----------

app.whenReady().then(() => {
    const win = createWindow();

    ipcMain.handle("config:get", () => readConfig());
    ipcMain.handle("config:set", (_e, cfg) => writeConfig(cfg));
    ipcMain.handle("app:info", () => ({
        version: app.getVersion(),
        electron: process.versions.electron,
        chrome: process.versions.chrome,
        node: process.versions.node,
    }));

    ipcMain.on("chat:start", (event, payload) => {
        const w = BrowserWindow.fromWebContents(event.sender);
        if (w) runChat(w, payload || {});
    });

    ipcMain.on("chat:cancel", (event) => {
        const ctrl = activeChats.get(event.sender.id);
        if (ctrl) ctrl.abort();
    });

    ipcMain.handle("upload:pick", async (event) => {
        const w = BrowserWindow.fromWebContents(event.sender);
        const { canceled, filePaths } = await dialog.showOpenDialog(w, {
            title: "选择要入库的文档",
            properties: ["openFile"],
            filters: [
                { name: "Markdown / 文本", extensions: ["md", "markdown", "txt"] },
                { name: "全部文件", extensions: ["*"] },
            ],
        });
        if (canceled || filePaths.length === 0) return { canceled: true };
        return uploadFile(w, filePaths[0]);
    });

    ipcMain.handle("shell:open", (_e, url) => {
        // 只允许打开 http/https；避免渲染进程通过 shell 打开任意协议
        const u = new URL(String(url));
        if (u.protocol === "http:" || u.protocol === "https:") return shell.openExternal(u.href);
        return null;
    });

    const shotIdx = process.argv.indexOf("--screenshot");
    if (shotIdx >= 0) {
        const out = process.argv[shotIdx + 1] || path.join(__dirname, "screenshot.png");
        shoot(win, out);
    }

    app.on("activate", () => {
        if (BrowserWindow.getAllWindows().length === 0) createWindow();
    });
});

app.on("window-all-closed", () => {
    if (process.platform !== "darwin") app.quit();
});
