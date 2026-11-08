/* KahoAgent Desktop — 渲染进程
 *
 * 所有网络能力都通过 window.kaho（preload 暴露）走主进程，
 * 这里不直接 fetch：renderer 的 origin 是 file://，直连会被 CORS 拦。
 * 这也让「渲染进程无特权」这条边界保持完整。
 */
(() => {
    "use strict";

    const el = {
        messages: document.getElementById("messages"),
        empty: document.getElementById("empty"),
        input: document.getElementById("input"),
        composer: document.getElementById("composer"),
        send: document.getElementById("send"),
        status: document.getElementById("status"),
        sources: document.getElementById("sources"),
        trace: document.getElementById("trace"),
        ttft: document.getElementById("m-ttft"),
        total: document.getElementById("m-total"),
        chars: document.getElementById("m-chars"),
        events: document.getElementById("m-events"),
        tokens: document.getElementById("m-tokens"),
        cache: document.getElementById("m-cache"),
        serverLabel: document.getElementById("server-label"),
        serverInput: document.getElementById("server-input"),
        settings: document.getElementById("settings"),
        toast: document.getElementById("toast"),
        version: document.getElementById("version"),
    };

    const state = {
        sessionId: newSessionId(),
        busy: false,
        startedAt: 0,
        firstTokenAt: 0,
        chars: 0,
        events: 0,
    };

    const demo = new URLSearchParams(location.search).get("demo") === "1";

    // 在 Electron 里用 preload 暴露的桥；在普通浏览器里退化成 mock。
    // 这样同一个渲染层既能被 Electron 加载，也能用 Chrome 直接打开做视觉验收 ——
    // 不需要为了截图单独维护一份界面。
    const bridge = window.kaho || createMockBridge();

    function createMockBridge() {
        const evs = new Set();
        const ends = new Set();
        let timer = null;
        const emit = (o) => evs.forEach((f) => f(o));
        const finish = (o) => ends.forEach((f) => f(o));
        const sample =
            "（浏览器预览）缓存穿透是指查询一个数据库里不存在的数据，缓存与数据库都不会命中。\n" +
            "常见解法：缓存空值、布隆过滤器、接口层参数校验。";

        return {
            chat() {
                let i = 0;
                emit({ type: "tool", tool: "rag_query" });
                setTimeout(
                    () =>
                        emit({
                            type: "sources",
                            sources: ["Redis 缓存实战 > 缓存穿透（redis.md）"],
                        }),
                    200,
                );
                timer = setInterval(() => {
                    if (i >= sample.length) {
                        clearInterval(timer);
                        emit({
                            type: "stats",
                            prompt_tokens: 812,
                            completion_tokens: 167,
                            cache_hit: false,
                        });
                        emit({ type: "done" });
                        finish({ ok: true });
                        return;
                    }
                    emit({ type: "delta", text: sample[i++] });
                }, 16);
            },
            cancel() {
                clearInterval(timer);
                finish({ ok: false, error: "已取消" });
            },
            onEvent(cb) {
                evs.add(cb);
                return () => evs.delete(cb);
            },
            onEnd(cb) {
                ends.add(cb);
                return () => ends.delete(cb);
            },
            async pickAndUpload() {
                return { canceled: true };
            },
            async getConfig() {
                return { serverUrl: "http://127.0.0.1:8080" };
            },
            async setConfig(c) {
                return c;
            },
            async info() {
                return { version: "0.1.0", electron: "browser-mock", chrome: navigator.userAgent, node: "-" };
            },
            async openExternal() {
                return null;
            },
        };
    }

    function newSessionId() {
        if (globalThis.crypto?.randomUUID) return crypto.randomUUID();
        return "d-" + Math.random().toString(36).slice(2) + Date.now().toString(36);
    }

    // ---------- 基础 UI ----------

    function setStatus(kind, label) {
        el.status.dataset.state = kind;
        el.status.textContent = label;
    }

    function fmtMs(ms) {
        if (!ms) return "—";
        return ms < 1000 ? Math.round(ms) + " ms" : (ms / 1000).toFixed(2) + " s";
    }

    function sinceStart() {
        return state.startedAt ? performance.now() - state.startedAt : 0;
    }

    function toast(text, isError) {
        el.toast.textContent = text;
        el.toast.classList.toggle("is-error", Boolean(isError));
        el.toast.classList.add("is-show");
        clearTimeout(toast._t);
        toast._t = setTimeout(() => el.toast.classList.remove("is-show"), 2600);
    }

    function resetTurn() {
        state.startedAt = performance.now();
        state.firstTokenAt = 0;
        state.chars = 0;
        state.events = 0;
        for (const k of ["ttft", "total", "chars", "events", "tokens", "cache"]) {
            el[k].textContent = "—";
        }
        el.trace.innerHTML = '<li class="muted">尚无事件</li>';
        el.sources.innerHTML = '<li class="muted">等待检索…</li>';
    }

    function bumpEvents() {
        state.events += 1;
        el.events.textContent = String(state.events);
    }

    function addTrace(text, kind, at) {
        const placeholder = el.trace.querySelector(".muted");
        if (placeholder) placeholder.remove();

        const li = document.createElement("li");
        if (kind) li.className = "is-" + kind;

        const ts = document.createElement("span");
        ts.className = "at";
        ts.textContent = "+" + Math.round(at ?? sinceStart()) + "ms";

        const what = document.createElement("span");
        what.className = "what";
        what.textContent = text;

        li.append(ts, what);
        el.trace.append(li);
        el.trace.parentElement.scrollTop = el.trace.parentElement.scrollHeight;
    }

    function renderSources(list) {
        if (!list || !list.length) return;
        el.sources.innerHTML = "";
        list.forEach((src, i) => {
            const li = document.createElement("li");
            const idx = document.createElement("span");
            idx.className = "idx";
            idx.textContent = String(i + 1).padStart(2, "0");
            const s = document.createElement("span");
            s.className = "src";
            // 一律 textContent：来源来自文档内容，拼 innerHTML 等于执行文档里的脚本
            s.textContent = src;
            li.append(idx, s);
            el.sources.append(li);
        });
        addTrace(`来源 ${list.length} 条`, "source");
        bumpEvents();
    }

    function hideEmpty() {
        el.empty.hidden = true;
    }

    function addUser(text) {
        hideEmpty();
        const wrap = document.createElement("div");
        wrap.className = "msg msg-user";
        const bubble = document.createElement("div");
        bubble.className = "bubble";
        bubble.textContent = text;
        wrap.append(bubble);
        el.messages.append(wrap);
        scrollBottom();
    }

    function addAssistant() {
        const wrap = document.createElement("div");
        wrap.className = "msg msg-assistant";
        const who = document.createElement("div");
        who.className = "who";
        who.textContent = "kahoagent";
        const body = document.createElement("div");
        body.className = "body is-streaming";
        wrap.append(who, body);
        el.messages.append(wrap);
        return body;
    }

    function addError(text) {
        const wrap = document.createElement("div");
        wrap.className = "msg msg-assistant msg-error";
        const body = document.createElement("div");
        body.className = "body";
        body.textContent = text;
        wrap.append(body);
        el.messages.append(wrap);
        scrollBottom();
    }

    function scrollBottom() {
        el.messages.scrollTop = el.messages.scrollHeight;
    }

    // ---------- 事件处理 ----------

    function handleEvent(ev, body) {
        switch (ev.type) {
            case "delta": {
                if (!state.firstTokenAt) {
                    state.firstTokenAt = performance.now();
                    el.ttft.textContent = fmtMs(state.firstTokenAt - state.startedAt);
                }
                state.chars += (ev.text || "").length;
                el.chars.textContent = state.chars.toLocaleString() + " 字";
                body.textContent += ev.text || "";
                scrollBottom();
                break;
            }
            case "tool":
                addTrace("工具调用 · " + (ev.tool || "unknown"), "tool");
                bumpEvents();
                break;
            case "sources":
                renderSources(ev.sources);
                break;
            case "stats": {
                const p = ev.prompt_tokens || 0;
                const c = ev.completion_tokens || 0;
                el.tokens.textContent = p + c > 0 ? `${p} + ${c}` : "—";
                el.cache.textContent = ev.cache_hit ? "命中" : "未命中";
                bumpEvents();
                break;
            }
            case "error":
                body.classList.remove("is-streaming");
                addError(ev.text || "处理失败。");
                setStatus("error", "出错");
                break;
            case "done":
                el.total.textContent = fmtMs(sinceStart());
                break;
        }
    }

    // ---------- 发送 ----------

    function send(text) {
        if (state.busy) return;
        state.busy = true;
        el.send.disabled = true;
        hideEmpty();
        resetTurn();
        setStatus("streaming", "检索中");
        addUser(text);

        const body = addAssistant();
        bridge.chat({ message: text, sessionId: state.sessionId });
        body._el = body; // 供 onEnd 找到当前气泡
        window.__kahoCurrentBody = body;
    }

    bridge.onEvent((ev) => {
        const body = window.__kahoCurrentBody;
        if (!body) return;
        handleEvent(ev, body);
    });

    bridge.onEnd((res) => {
        const body = window.__kahoCurrentBody;
        if (body) body.classList.remove("is-streaming");
        window.__kahoCurrentBody = null;
        state.busy = false;
        el.send.disabled = false;

        if (res?.ok) {
            el.total.textContent = fmtMs(sinceStart());
            addTrace("完成", "done");
            setStatus("idle", "就绪");
        } else {
            setStatus("error", "出错");
            addError("请求失败：" + (res?.error || "未知错误"));
        }
        el.input.focus();
    });

    // ---------- 上传 / 设置 ----------

    async function upload() {
        try {
            const res = await bridge.pickAndUpload();
            if (!res || res.canceled) return;
            toast(`已受理：${res.doc_id || "文档"}（${res.bytes ?? "?"} 字节）`);
            addTrace(`上传 ${res.doc_id}`, "tool");
        } catch (err) {
            toast("上传失败：" + (err.message || err), true);
        }
    }

    function openSettings() {
        el.serverInput.value = el.serverLabel.textContent;
        el.settings.showModal();
    }

    async function saveSettings() {
        try {
            const cfg = await bridge.setConfig({ serverUrl: el.serverInput.value });
            el.serverLabel.textContent = cfg.serverUrl;
            el.settings.close();
            toast("已保存");
        } catch (err) {
            toast(err.message || String(err), true);
        }
    }

    // ---------- 演示数据（离线截图用） ----------

    function renderDemo() {
        setStatus("idle", "演示数据");
        addUser("缓存穿透和缓存击穿有什么区别？");
        const body = addAssistant();
        body.classList.remove("is-streaming");
        body.textContent =
            "缓存穿透是指查询一个数据库里根本不存在的数据，缓存和数据库都不会命中；\n" +
            "缓存击穿则是一个热点 key 过期瞬间，大量请求直接压到数据库。\n\n" +
            "常见解法：\n" +
            "1. 穿透：缓存空值 + 布隆过滤器；\n" +
            "2. 击穿：互斥锁重建 + 逻辑过期；\n" +
            "3. 两者都要配合接口层参数校验。";

        state.startedAt = performance.now() - 3180;
        el.ttft.textContent = "922 ms";
        el.total.textContent = "3.18 s";
        el.chars.textContent = "167 字";
        el.events.textContent = "4";
        el.tokens.textContent = "812 + 167";
        el.cache.textContent = "未命中";

        el.trace.innerHTML = "";
        addTrace("建立连接", "net", 412);
        addTrace("工具调用 · rag_query", "tool", 418);
        addTrace("来源 3 条", "source", 906);
        addTrace("完成", "done", 3180);
        state.events = 4;

        el.sources.innerHTML = "";
        [
            "Redis 缓存实战 > 缓存击穿（redis.md）",
            "Redis 缓存实战 > 布隆过滤器（redis.md）",
            "缓存一致性 > 空值缓存（cache.md）",
        ].forEach((src, i) => {
            const li = document.createElement("li");
            const idx = document.createElement("span");
            idx.className = "idx";
            idx.textContent = String(i + 1).padStart(2, "0");
            const s = document.createElement("span");
            s.className = "src";
            s.textContent = src;
            li.append(idx, s);
            el.sources.append(li);
        });
    }

    // ---------- 初始化 ----------

    el.composer.addEventListener("submit", (e) => {
        e.preventDefault();
        const text = el.input.value.trim();
        if (!text || state.busy) return;
        el.input.value = "";
        el.input.style.height = "auto";
        send(text);
    });

    el.input.addEventListener("keydown", (e) => {
        if (e.key === "Enter" && !e.shiftKey && !e.isComposing) {
            e.preventDefault();
            el.composer.requestSubmit();
        }
    });

    el.input.addEventListener("input", () => {
        el.input.style.height = "auto";
        el.input.style.height = Math.min(el.input.scrollHeight, 150) + "px";
    });

    document.getElementById("btn-new").addEventListener("click", () => {
        if (state.busy) bridge.cancel();
        state.sessionId = newSessionId();
        el.messages.innerHTML = "";
        el.empty.hidden = false;
        el.trace.innerHTML = '<li class="muted">尚无事件</li>';
        el.sources.innerHTML = '<li class="muted">等待检索…</li>';
        resetTurn();
        for (const k of ["ttft", "total", "chars", "events", "tokens", "cache"]) {
            el[k].textContent = "—";
        }
        setStatus("idle", "就绪");
        el.input.focus();
    });

    document.getElementById("btn-upload").addEventListener("click", upload);
    document.getElementById("btn-settings").addEventListener("click", openSettings);
    document.getElementById("btn-cancel").addEventListener("click", () => el.settings.close());
    document.getElementById("btn-save").addEventListener("click", saveSettings);

    document.querySelectorAll(".chip").forEach((chip) => {
        chip.addEventListener("click", () => {
            el.input.value = chip.dataset.q || chip.textContent.trim();
            el.composer.requestSubmit();
        });
    });

    document.getElementById("link-web").addEventListener("click", async (e) => {
        e.preventDefault();
        const url = el.serverLabel.textContent;
        if (url && url !== "—") await bridge.openExternal(url);
    });

    (async () => {
        const info = await bridge.info();
        el.version.textContent = `v${info.version} · Electron ${info.electron}`;
        const cfg = await bridge.getConfig();
        el.serverLabel.textContent = cfg.serverUrl;
        el.serverInput.value = cfg.serverUrl;
        if (demo) renderDemo();
        el.input.focus();
    })();
})();
