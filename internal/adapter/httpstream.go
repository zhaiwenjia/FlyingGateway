// 日期: 2026-11-14 14:29:38.422
// 作者: Zhai Wenjia

package adapter

import (
	"bytes"
	"context"
	"io"
	"net/http"

	"github.com/zhaiwenjia/FlyingGateway/internal/core"
	gwerrors "github.com/zhaiwenjia/FlyingGateway/internal/errors"
	"github.com/zhaiwenjia/FlyingGateway/internal/stream"
)

// 本文件是流式取流的机械件（统一 SDD 3.7「一套流解析核心 + 每家一个帧解码器」）：
// HTTP 发起 / body 生命周期 / ctx 取消中断在此共享；「供应商帧 → 内部帧」的解码器逐家实现。

// streamRequest 按出站产物发真实 HTTP 请求（构造时注入的 *http.Client）。
// 非 2xx ⇒ DetailError（errors.FromUpstream 口径，保留上游状态码 / 请求 ID / 响应体）。
func streamRequest(ctx context.Context, hc *http.Client, p core.Provider, out *Outbound) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, out.Method, out.URL, bytes.NewReader(out.Body))
	if err != nil {
		return nil, &DetailError{Detail: gwerrors.New(core.ErrInvalidRequest, 400, "request_build_failed", err.Error())}
	}
	for k, v := range out.Headers {
		req.Header.Set(k, v)
	}
	resp, err := hc.Do(req)
	if err != nil {
		// 取消 / 超时 / 连接失败统一由 errors.FromTransportError 归类（7.1）。
		return nil, &DetailError{Detail: gwerrors.FromTransportError(p, err, "")}
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return nil, &DetailError{Detail: gwerrors.FromUpstream(p, resp.StatusCode, b, resp.Header.Get("X-Request-Id"))}
	}
	return resp, nil
}

// frameDecoder 是「每家一个」的帧解码器：SSE 事件 → 内部帧。
// finished=true 表示终态事件已到（后续帧发完即收口）。
type frameDecoder func(ev stream.SSEEvent) (frames []stream.Frame, finished bool, err error)

// endCheck 是流自然结束后的完整性检查（如 Gemini 流结束无 finishReason ⇒ 按错误处理，3.7）。
type endCheck func() error

// sseIter 是 SSE 流式帧迭代器：解析核心用 stream.SSEScanner（容忍 data: 无空格、缺 [DONE]、缺事件）。
type sseIter struct {
	ctx     context.Context
	body    io.ReadCloser
	sc      *stream.SSEScanner
	decode  frameDecoder
	endChk  endCheck
	queue   []stream.Frame
	cur     stream.Frame
	err     error
	stop    bool // 终态事件已解出（队列发完即收口）
	closed  bool
	seqAuto int64 // 无 sequence_number 事件的自增序号
}

func newSSEIter(ctx context.Context, body io.ReadCloser, decode frameDecoder, endChk endCheck) *sseIter {
	return &sseIter{ctx: ctx, body: body, sc: stream.NewSSEScanner(body), decode: decode, endChk: endChk}
}

// Next 推进到下一帧；流结束或出错返回 false（此时关闭 body）。
func (it *sseIter) Next() bool {
	for len(it.queue) == 0 {
		if it.stop {
			it.close()
			return false
		}
		if err := it.ctx.Err(); err != nil {
			// ctx 取消 ⇒ 立即中断并关闭上游连接（3.7 取消语义；cancel_grace_period 默认 0）。
			it.err = err
			it.close()
			return false
		}
		if !it.sc.Scan() {
			if e := it.sc.Err(); e != nil {
				it.err = e
			} else if it.endChk != nil {
				it.err = it.endChk()
			}
			it.close()
			return false
		}
		frames, finished, err := it.decode(it.sc.Event())
		if err != nil {
			it.err = err
			it.close()
			return false
		}
		for i := range frames {
			it.seqAuto++
			if frames[i].Seq == 0 {
				frames[i].Seq = it.seqAuto
			}
		}
		it.queue = frames
		it.stop = finished
	}
	it.cur = it.queue[0]
	it.queue = it.queue[1:]
	return true
}

// Frame 返回当前帧。
func (it *sseIter) Frame() stream.Frame { return it.cur }

// Err 返回终态错误（nil 表示正常结束）。
func (it *sseIter) Err() error { return it.err }

// close 关闭上游 body（停止时关闭 body，3.6）并归还扫描缓冲；幂等。
// Release 安全性：close 只在终态路径调用；bufio.Scanner 在 EOF/出错后再 Scan
// 不会向缓冲读写（GOROOT bufio/scan.go：s.err != nil 直接短路），⛔ 不会串流数据。
func (it *sseIter) close() {
	if !it.closed {
		it.closed = true
		_ = it.body.Close()
		it.sc.Release() // 缓冲归还池（可省略走 GC；此处调用以复用 64KB 初始缓冲）
	}
}
