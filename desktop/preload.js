// preload：渲染进程与主进程之间唯一的桥。
//
// 只暴露最小能力集合，不暴露 ipcRenderer 本身 ——
// 一旦把 ipcRenderer 交给 renderer，页面里的任何脚本都能调用
// 主进程注册的**所有**通道，contextIsolation 就形同虚设。
"use strict";

const { contextBridge, ipcRenderer } = require("electron");

contextBridge.exposeInMainWorld("kaho", {
    // 发起对话；事件通过 onEvent / onEnd 回流
    chat: (payload) => ipcRenderer.send("chat:start", payload),
    cancel: () => ipcRenderer.send("chat:cancel"),

    onEvent: (cb) => {
        const handler = (_e, ev) => cb(ev);
        ipcRenderer.on("chat:event", handler);
        return () => ipcRenderer.removeListener("chat:event", handler);
    },
    onEnd: (cb) => {
        const handler = (_e, res) => cb(res);
        ipcRenderer.on("chat:end", handler);
        return () => ipcRenderer.removeListener("chat:end", handler);
    },

    pickAndUpload: () => ipcRenderer.invoke("upload:pick"),

    getConfig: () => ipcRenderer.invoke("config:get"),
    setConfig: (cfg) => ipcRenderer.invoke("config:set", cfg),

    info: () => ipcRenderer.invoke("app:info"),
    openExternal: (url) => ipcRenderer.invoke("shell:open", url),
});
