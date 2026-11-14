#!/usr/bin/env python3
# 日期: 2026-11-14 18:35:00.000
# 作者: Zhai Wenjia
"""本地隔离用假上游：固定 300ms 延迟的 chat 形两拍 usage 响应（SSE 或 JSON，按 stream 分派）。

用法：/tmp/bench/bin/python scripts/fake_upstream.py --port 9099 --latency 0.3
"""
import argparse, asyncio, json, time

CHUNKS = [
    {"id": "f", "object": "chat.completion.chunk", "model": "mimo-v2.6-flash",
     "choices": [{"index": 0, "delta": {"content": "好"}, "finish_reason": None}]},
    {"id": "f", "object": "chat.completion.chunk", "model": "mimo-v2.6-flash",
     "choices": [{"index": 0, "delta": {}, "finish_reason": "stop"}], "usage": None},
    {"id": "f", "object": "chat.completion.chunk", "model": "mimo-v2.6-flash",
     "choices": [], "usage": {"prompt_tokens": 3, "completion_tokens": 1, "total_tokens": 4}},
]
NONSTREAM = {"id": "f", "object": "chat.completion", "model": "mimo-v2.6-flash",
             "choices": [{"index": 0, "message": {"role": "assistant", "content": "好"}, "finish_reason": "stop"}],
             "usage": {"prompt_tokens": 3, "completion_tokens": 1, "total_tokens": 4}}

async def handle(request):
    body = await request.json()
    await asyncio.sleep(LATENCY)
    if body.get("stream"):
        async def gen():
            for c in CHUNKS:
                yield "data: " + json.dumps(c, ensure_ascii=False) + "\n\n"
            yield "data: [DONE]\n\n"
        from aiohttp import web
        return web.StreamResponse() if False else web.Response(text="".join(["data: " + json.dumps(c, ensure_ascii=False) + "\n\n" for c in CHUNKS]) + "data: [DONE]\n\n", headers={"Content-Type": "text/event-stream"})
    from aiohttp import web
    return web.json_response(NONSTREAM)

LATENCY = 0.3

def main():
    global LATENCY
    ap = argparse.ArgumentParser()
    ap.add_argument("--port", type=int, default=9099)
    ap.add_argument("--latency", type=float, default=0.3)
    args = ap.parse_args()
    LATENCY = args.latency
    from aiohttp import web
    app = web.Application()
    app.router.add_post("/v1/chat/completions", handle)
    app.router.add_post("/chat/completions", handle)
    web.run_app(app, host="127.0.0.1", port=args.port, print=None)

if __name__ == "__main__":
    main()
