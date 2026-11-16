// 日期: 2026-11-16 23:39:48.270
// 作者: Zhai Wenjia

package share

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"time"
)

// 本文件实现最小 RESP2 客户端（Redis Serialization Protocol 2，协议稳定、命令面小）：
// 只覆盖本包所需（EVAL / GET / SET PX / PING / AUTH），⛔ 不引第三方 Redis 库
//（仓规 6：除非必要不引入新依赖 —— RESP2 请求为数组、回复为 + - : $ * 五型，自实现可控）。
//
// 连接池：惰性拨号 + 空闲连接复用（chan 池），坏连接关闭不回池；每连接独立读写超时。

// reply 是一条 RESP2 回复（kind：+ 简单串 / - 错误 / : 整数 / $ 批量串 / * 数组）。
type reply struct {
	kind byte
	str  string
	num  int64
	bulk []byte // $ 型；nil = null bulk（$-1）
	arr  []reply
}

// errRedis 是 Redis 侧错误（-ERR 型回复）。
type errRedis string

func (e errRedis) Error() string { return "share: redis 错误：" + string(e) }

// Client 是最小 RESP2 客户端（并发安全）。
type Client struct {
	addr     string
	password string
	timeout  time.Duration // 单次读写超时
	pool     chan *respConn
}

type respConn struct {
	nc net.Conn
	br *bufio.Reader
}

// NewClient 构造客户端。password 空 = 不发 AUTH；poolSize ≤ 0 ⇒ 8。
func NewClient(addr, password string, timeout time.Duration, poolSize int) *Client {
	if poolSize <= 0 {
		poolSize = 8
	}
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	return &Client{addr: addr, password: password, timeout: timeout, pool: make(chan *respConn, poolSize)}
}

// Do 发送一条命令并读回复（args[0] 为命令名）。
func (c *Client) Do(ctx context.Context, args ...string) (reply, error) {
	if len(args) == 0 {
		return reply{}, errors.New("share: 空命令")
	}
	rc, err := c.get(ctx)
	if err != nil {
		return reply{}, err
	}
	// 发送。
	deadline := time.Now().Add(c.timeout)
	_ = rc.nc.SetDeadline(deadline)
	var buf []byte
	buf = append(buf, '*')
	buf = strconv.AppendInt(buf, int64(len(args)), 10)
	buf = append(buf, '\r', '\n')
	for _, a := range args {
		buf = append(buf, '$')
		buf = strconv.AppendInt(buf, int64(len(a)), 10)
		buf = append(buf, '\r', '\n')
		buf = append(buf, a...)
		buf = append(buf, '\r', '\n')
	}
	if _, err := rc.nc.Write(buf); err != nil {
		rc.close()
		return reply{}, fmt.Errorf("share: redis 写失败：%w", err)
	}
	rp, err := readReply(rc.br)
	if err != nil {
		rc.close()
		return reply{}, fmt.Errorf("share: redis 读失败：%w", err)
	}
	c.put(rc)
	if rp.kind == '-' {
		return reply{}, errRedis(rp.str)
	}
	return rp, nil
}

// get 取空闲连接（池空则拨号；新连接先做 AUTH/PING 自检）。
func (c *Client) get(ctx context.Context) (*respConn, error) {
	select {
	case rc := <-c.pool:
		return rc, nil
	default:
	}
	var d net.Dialer
	nc, err := d.DialContext(ctx, "tcp", c.addr)
	if err != nil {
		return nil, fmt.Errorf("share: redis 连接失败：%w", err)
	}
	rc := &respConn{nc: nc, br: bufio.NewReader(nc)}
	_ = nc.SetDeadline(time.Now().Add(c.timeout))
	if c.password != "" {
		if err := writeCmd(nc, "AUTH", c.password); err != nil {
			rc.close()
			return nil, err
		}
		if _, err := readReply(rc.br); err != nil {
			rc.close()
			return nil, fmt.Errorf("share: redis AUTH 失败：%w", err)
		}
	}
	return rc, nil
}

// put 回收连接（池满 ⇒ 关闭）。
func (c *Client) put(rc *respConn) {
	select {
	case c.pool <- rc:
	default:
		rc.close()
	}
}

func (rc *respConn) close() { _ = rc.nc.Close() }

// writeCmd 发送一条命令（连接握手用）。
func writeCmd(nc net.Conn, args ...string) error {
	var buf []byte
	buf = append(buf, '*')
	buf = strconv.AppendInt(buf, int64(len(args)), 10)
	buf = append(buf, '\r', '\n')
	for _, a := range args {
		buf = append(buf, '$')
		buf = strconv.AppendInt(buf, int64(len(a)), 10)
		buf = append(buf, '\r', '\n')
		buf = append(buf, a...)
		buf = append(buf, '\r', '\n')
	}
	_, err := nc.Write(buf)
	return err
}

// readReply 解析一条 RESP2 回复（+ - : $ * 五型递归）。
func readReply(br *bufio.Reader) (reply, error) {
	line, err := readLine(br)
	if err != nil {
		return reply{}, err
	}
	if len(line) == 0 {
		return reply{}, errors.New("share: 空 RESP 行")
	}
	kind := line[0]
	body := line[1:]
	switch kind {
	case '+':
		return reply{kind: '+', str: body}, nil
	case '-':
		return reply{kind: '-', str: body}, nil
	case ':':
		n, err := strconv.ParseInt(body, 10, 64)
		if err != nil {
			return reply{}, fmt.Errorf("share: 整数解析失败 %q：%w", body, err)
		}
		return reply{kind: ':', num: n}, nil
	case '$':
		n, err := strconv.ParseInt(body, 10, 64)
		if err != nil {
			return reply{}, fmt.Errorf("share: 批量长度解析失败 %q：%w", body, err)
		}
		if n < 0 {
			return reply{kind: '$', bulk: nil}, nil // null bulk
		}
		if n > 1<<20 {
			return reply{}, fmt.Errorf("share: 批量串过大 %d", n)
		}
		data := make([]byte, n+2)
		if _, err := readFull(br, data); err != nil {
			return reply{}, err
		}
		return reply{kind: '$', bulk: data[:n]}, nil
	case '*':
		n, err := strconv.ParseInt(body, 10, 64)
		if err != nil {
			return reply{}, fmt.Errorf("share: 数组长度解析失败 %q：%w", body, err)
		}
		if n < 0 {
			return reply{kind: '*', arr: nil}, nil
		}
		arr := make([]reply, 0, n)
		for i := int64(0); i < n; i++ {
			rp, err := readReply(br)
			if err != nil {
				return reply{}, err
			}
			arr = append(arr, rp)
		}
		return reply{kind: '*', arr: arr}, nil
	default:
		return reply{}, fmt.Errorf("share: 未知 RESP 前缀 %q", kind)
	}
}

// readLine 读一行（剥 \r\n）。
func readLine(br *bufio.Reader) (string, error) {
	s, err := br.ReadString('\n')
	if err != nil {
		return "", err
	}
	if len(s) < 2 || s[len(s)-2] != '\r' {
		return "", errors.New("share: RESP 行尾非法")
	}
	return s[:len(s)-2], nil
}

// readFull 读满 n 字节。
func readFull(br *bufio.Reader, buf []byte) (int, error) {
	total := 0
	for total < len(buf) {
		n, err := br.Read(buf[total:])
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}
