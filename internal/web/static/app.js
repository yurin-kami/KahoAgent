/* KahoAgent 控制台前端逻辑
 *
 * 零依赖：只用 fetch + ReadableStream 读 SSE。
 * 不引 EventSource 的原因：EventSource 只支持 GET，而 /api/chat 是 POST。
 */

(() => {
    "use strict";

    const el = {
        messages: document.getElementById("messages"),
        empty: document.getElementById("empty"),
        input: document.getElementById("input"),
        composer: document.getElementById("composer"),
        send: document.getElementById("send"),
        reset: document.getElementById("reset"),
        status: document.getElementById("status"),
        sources: document.getElementById("sources"),
        trace: document.getElementById("trace"),
        ttft: document.getElementById("m-ttft"),
        total: document.getElementById("m-total"),
        chars: document.getElementById("m-chars"),
        events: document.getElementById("m-events"),
        tokens: document.getElementById("m-tokens"),
        cache: document.getElementById("m-cache"),
    };

    const state = {
        sessionId: newSessionId(),
        controller: null,
        startedAt: 0,
        firstTokenAt: 0,
        chars: 0,
        events: 0,
    };

    function newSessionId() {
        if (globalThis.crypto?.randomUUID) return crypto.randomUUID();
        return "s-" + Math.random().toString(36).slice(2) + Date.now().toString(36);
    }

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

    function resetTurn() {
        state.startedAt = performance.now();
        state.firstTokenAt = 0;
        state.chars = 0;
        state.events = 0;
        el.ttft.textContent = "—";
        el.total.textContent = "—";
        el.chars.textContent = "—";
        el.events.textContent = "—";
        el.tokens.textContent = "—";
        el.cache.textContent = "—";
        el.trace.innerHTML = '<li class="muted">尚无事件</li>';
        el.sources.innerHTML = '<li class="muted">等待检索…</li>';
    }

    function bumpEvents() {
        state.events += 1;
        el.events.textContent = String(state.events);
    }

    function addTrace(text, kind) {
        const placeholder = el.trace.querySelector(".muted");
        if (placeholder) placeholder.remove();

        const li = document.createElement("li");
        if (kind) li.className = "is-" + kind;

        const at = document.createElement("span");
        at.className = "at";
        at.textContent = "+" + Math.round(sinceStart()) + "ms";

        const what = document.createElement("span");
        what.className = "what";
        what.textContent = text;

        li.append(at, what);
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
            // ⚠️ 一律用 textContent：来源字符串来自文档内容，
            // 直接拼 innerHTML 等于把文档里的 HTML 当代码执行。
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
        scrollToBottom();
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

    function addSystem(text, isError) {
        const wrap = document.createElement("div");
        wrap.className = "msg msg-system" + (isError ? " msg-error" : "");
        const body = document.createElement("div");
        body.className = "body";
        body.textContent = text;
        wrap.append(body);
        el.messages.append(wrap);
        scrollToBottom();
    }

    function scrollToBottom() {
        el.messages.parentElement.scrollTop = el.messages.parentElement.scrollHeight;
    }

    // 把一行 SSE 数据解析成事件对象；不是 data 行或 JSON 坏了就返回 null。
    function parseEvent(line) {
        if (!line.startsWith("data:")) return null;
        try {
            return JSON.parse(line.slice(5).trim());
        } catch {
            return null;
        }
    }

    async function send(text) {
        if (state.controller) return;

        state.controller = new AbortController();
        el.send.disabled = true;
        setStatus("streaming", "检索中");
        resetTurn();
        addUser(text);

        const body = addAssistant();
        let firstByte = true;

        try {
            const resp = await fetch("/api/chat", {
                method: "POST",
                headers: { "Content-Type": "application/json" },
                body: JSON.stringify({ session_id: state.sessionId, message: text }),
                signal: state.controller.signal,
            });

            if (!resp.ok || !resp.body) {
                throw new Error(`HTTP ${resp.status}`);
            }

            const reader = resp.body.getReader();
            const decoder = new TextDecoder();
            let buffer = "";

            for (;;) {
                const { value, done } = await reader.read();
                if (done) break;
                if (firstByte) {
                    firstByte = false;
                    setStatus("streaming", "生成中");
                    addTrace("建立连接", "net");
                }

                buffer += decoder.decode(value, { stream: true });

                // SSE 事件以空行分隔。最后一段可能不完整，留在 buffer 里等下一块。
                let idx;
                while ((idx = buffer.indexOf("\n\n")) >= 0) {
                    const raw = buffer.slice(0, idx);
                    buffer = buffer.slice(idx + 2);

                    for (const line of raw.split("\n")) {
                        const ev = parseEvent(line);
                        if (ev) handleEvent(ev, body);
                    }
                }
            }

            body.classList.remove("is-streaming");
            setStatus("idle", "就绪");
            el.total.textContent = fmtMs(sinceStart());
            addTrace("完成", "done");
        } catch (err) {
            body.classList.remove("is-streaming");
            if (err.name === "AbortError") {
                addSystem("请求已取消。");
                setStatus("idle", "就绪");
            } else {
                addSystem("连接失败：" + err.message + "。请确认服务在运行。", true);
                setStatus("error", "出错");
            }
        } finally {
            state.controller = null;
            el.send.disabled = false;
            el.input.focus();
        }
    }

    function handleEvent(ev, body) {
        switch (ev.type) {
            case "delta": {
                if (!state.firstTokenAt) {
                    state.firstTokenAt = performance.now();
                    el.ttft.textContent = fmtMs(state.firstTokenAt - state.startedAt);
                    setStatus("streaming", "生成中");
                }
                state.chars += ev.text.length;
                el.chars.textContent = state.chars.toLocaleString() + " 字";
                body.textContent += ev.text;
                scrollToBottom();
                break;
            }
            case "tool": {
                addTrace("工具调用 · " + (ev.tool || "unknown"), "tool");
                bumpEvents();
                break;
            }
            case "sources": {
                renderSources(ev.sources);
                break;
            }
            case "error": {
                body.classList.remove("is-streaming");
                addSystem(ev.text || "处理失败。", true);
                setStatus("error", "出错");
                break;
            }
            case "stats": {
                // 服务端统计：token 用量与检索缓存命中。
                // 0 表示服务端没拿到 usage（部分兼容实现不返回），显示 — 而不是 0。
                const p = ev.prompt_tokens || 0;
                const c = ev.completion_tokens || 0;
                el.tokens.textContent = p + c > 0 ? `${p} + ${c}` : "—";
                el.cache.textContent = ev.cache_hit ? "命中" : "未命中";
                bumpEvents();
                break;
            }
            case "done": {
                el.total.textContent = fmtMs(sinceStart());
                break;
            }
        }
    }

    function submit() {
        const text = el.input.value.trim();
        if (!text || state.controller) return;
        el.input.value = "";
        el.input.style.height = "auto";
        send(text);
    }

    el.composer.addEventListener("submit", (e) => {
        e.preventDefault();
        submit();
    });

    el.input.addEventListener("keydown", (e) => {
        if (e.key === "Enter" && !e.shiftKey && !e.isComposing) {
            e.preventDefault();
            submit();
        }
    });

    el.input.addEventListener("input", () => {
        el.input.style.height = "auto";
        el.input.style.height = Math.min(el.input.scrollHeight, 160) + "px";
    });

    el.reset.addEventListener("click", () => {
        if (state.controller) state.controller.abort();
        state.sessionId = newSessionId();
        el.messages.innerHTML = "";
        el.empty.hidden = false;
        el.trace.innerHTML = '<li class="muted">尚无事件</li>';
        el.sources.innerHTML = '<li class="muted">等待检索…</li>';
        ["ttft", "total", "chars", "events", "tokens", "cache"].forEach(
            (k) => (el[k].textContent = "—"),
        );
        setStatus("idle", "就绪");
        el.input.focus();
    });

    document.querySelectorAll(".chip").forEach((chip) => {
        chip.addEventListener("click", () => {
            el.input.value = chip.dataset.q || chip.textContent.trim();
            submit();
        });
    });

    el.input.focus();
})();
