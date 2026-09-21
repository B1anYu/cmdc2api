package server

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/B1anYu/cmdc2api/internal/config"
)

// ===========================================================================
// 上游空闲超时：body.go 的 idleReader + pipeline.go 的两条消费路径。
//
// 这条链（上游停滞 → 本地主动断开 → 出站错误出口）在本次补齐前是整仓零覆盖：
// ErrIdleTimeout / idleReader 在全部 *_test.go 中零命中，即「上游挂住」从未被执行过。
// 档位按出站形态分开（IdleStream 默认 30s / IdleBuffer 默认 90s），
// 下面全部用毫秒级档位驱动，不真的等半分钟。
// ===========================================================================

// stallBody 可控延迟的上游响应体替身：Read 在无数据时阻塞，Close 时唤醒。
// 用来复现「上游连上了但不再发数据」——读一半停住，正是空闲超时唯一能救场的形态。
type stallBody struct {
	mu      sync.Mutex
	cond    *sync.Cond
	pending []byte
	closed  bool
	closes  int
}

func newStallBody() *stallBody {
	b := &stallBody{}
	b.cond = sync.NewCond(&b.mu)
	return b
}

// push 追加一段可读数据并唤醒阻塞中的 Read。
func (b *stallBody) push(s string) {
	b.mu.Lock()
	b.pending = append(b.pending, s...)
	b.mu.Unlock()
	b.cond.Broadcast()
}

func (b *stallBody) Read(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for len(b.pending) == 0 && !b.closed {
		b.cond.Wait()
	}
	if len(b.pending) == 0 {
		// 被关闭后读到 EOF：idleReader 必须把这个「因超时关闭而解开的读」改写成 ErrIdleTimeout，
		// 否则上层只会看到一次干净的流结束（读了半截却当成功）。
		return 0, io.EOF
	}
	n := copy(p, b.pending)
	b.pending = b.pending[n:]
	return n, nil
}

func (b *stallBody) Close() error {
	b.mu.Lock()
	b.closed = true
	b.closes++
	b.mu.Unlock()
	b.cond.Broadcast()
	return nil
}

func (b *stallBody) closeCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.closes
}

// TestIdleReader 单元级：直接驱动 idleReader，不需要 HTTP 栈。
func TestIdleReader(t *testing.T) {
	// 未超时：数据在空闲窗口内到达 → 原样读出，不得误报超时。
	t.Run("正常读取不受空闲计时影响", func(t *testing.T) {
		body := newStallBody()
		body.push("hello ")
		ir := newIdleReader(body, time.Second)
		defer func() { _ = ir.Close() }()

		buf := make([]byte, 64)
		n, err := ir.Read(buf)
		if err != nil {
			t.Fatalf("首个读返回错误 = %v, want nil", err)
		}
		if got := string(buf[:n]); got != "hello " {
			t.Errorf("读到 %q, want %q", got, "hello ")
		}
		if body.closeCount() != 0 {
			t.Error("未超时时不得关闭上游连接")
		}
	})

	// 每次成功读取都要重置空闲计时：否则「细水长流」的合法长流会被误判为停滞。
	// 这里让累计耗时（1.2s）超过 idle（1s），但每段间隔（0.6s）都小于 idle——
	// 若 Read 不重置计时器，第三次读会落在最初的 1s 期限之后而误报超时。
	t.Run("每次成功读取都重置空闲计时", func(t *testing.T) {
		const (
			idle = time.Second
			gap  = 600 * time.Millisecond
		)
		body := newStallBody()
		ir := newIdleReader(body, idle)
		defer func() { _ = ir.Close() }()

		buf := make([]byte, 64)
		for i := 0; i < 3; i++ {
			if i > 0 {
				time.Sleep(gap) // 累计 1.2s > idle，单段 0.6s < idle
			}
			body.push(fmt.Sprintf("chunk%d;", i))
			n, err := ir.Read(buf)
			if err != nil {
				t.Fatalf("第 %d 次读返回错误 = %v（空闲计时未按每次读取重置会导致这里误报超时）", i+1, err)
			}
			if want := fmt.Sprintf("chunk%d;", i); string(buf[:n]) != want {
				t.Fatalf("第 %d 次读到 %q, want %q", i+1, string(buf[:n]), want)
			}
		}
		if body.closeCount() != 0 {
			t.Error("读取都在空闲窗口内，不得关闭上游连接")
		}
	})

	// 停滞：上游既不发送也不关闭 → 本地必须主动断开并给出专用错误。
	t.Run("停滞超时返回 ErrIdleTimeout 并断开上游", func(t *testing.T) {
		const idle = 50 * time.Millisecond
		body := newStallBody()
		ir := newIdleReader(body, idle)
		defer func() { _ = ir.Close() }()

		buf := make([]byte, 64)
		start := time.Now()
		n, err := ir.Read(buf) // 上游不发数据 → 阻塞到计时器开火
		elapsed := time.Since(start)

		if n != 0 {
			t.Errorf("超时读返回 n = %d, want 0", n)
		}
		if !errors.Is(err, ErrIdleTimeout) {
			t.Fatalf("停滞读返回错误 = %v, want ErrIdleTimeout（底层是 io.EOF，必须被改写成超时）", err)
		}
		if errors.Is(err, io.EOF) {
			t.Error("不得把「因超时断开」误报成正常结束（io.EOF）")
		}
		if elapsed < idle {
			t.Errorf("在空闲期限（%v）前就返回了错误：%v", idle, elapsed)
		}
		if elapsed > 5*time.Second {
			t.Errorf("远超空闲期限才返回：%v", elapsed)
		}
		if got := body.closeCount(); got != 1 {
			t.Errorf("上游连接关闭次数 = %d, want 1（超时必须主动断开上游连接）", got)
		}

		// 超时已置位：后续读不得再去碰上游（上游已经不该被复用）。
		start = time.Now()
		if _, err := ir.Read(buf); !errors.Is(err, ErrIdleTimeout) {
			t.Errorf("超时后的读返回 %v, want ErrIdleTimeout", err)
		}
		if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
			t.Errorf("超时后的读阻塞了 %v，应立刻返回", elapsed)
		}
		if got := body.closeCount(); got != 1 {
			t.Errorf("后续读不得再次关闭上游：关闭次数 = %d", got)
		}
	})

	// 显式 Close 必须停掉计时器，否则连接会被重复关闭。
	t.Run("Close 停止计时不再重复关闭上游", func(t *testing.T) {
		const idle = 60 * time.Millisecond
		body := newStallBody()
		ir := newIdleReader(body, idle)
		if err := ir.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		time.Sleep(4 * idle) // 睡过空闲期限：计时器若没停，会再关一次
		if got := body.closeCount(); got != 1 {
			t.Errorf("关闭次数 = %d, want 1（计时器必须在 Close 时停止）", got)
		}
	})
}

// ---------- 管线级：停滞的上游 ----------

// stallingProxy 上游写完 lines 后挂起：连接保持打开，既不发送也不关闭。
// fakeCC 只能写完整响应、truncatedProxy 是直接掐断，两者都表达不了「停滞」——
// 而停滞正是空闲超时唯一能救场的形态（掐断会走 read error 分支，出口不同）。
//
// 挂起的 handler 在代理侧断开（超时触发上游连接关闭）后立即返回，
// 因此 httptest.Server.Close() 不会等待超时。
func stallingProxy(t *testing.T, f *fakeCC, lines []string, mutate func(*config.Config)) *httptest.Server {
	t.Helper()
	return newChatProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/alpha/generate" {
			f.serve(w, r) // 指纹/生命周期/models 等前置请求照常
			return
		}
		// 先排空请求体：之后对连接的 Read 才能只在「对端关闭」时返回。
		_, _ = io.Copy(io.Discard, r.Body)
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Error("upstream: hijack unsupported")
			return
		}
		conn, buf, err := hj.Hijack()
		if err != nil {
			t.Errorf("upstream: hijack failed: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()
		_, _ = buf.WriteString("HTTP/1.1 200 OK\r\nContent-Type: application/x-ndjson\r\nTransfer-Encoding: chunked\r\n\r\n")
		for _, line := range lines {
			payload := line + "\n"
			_, _ = fmt.Fprintf(buf, "%x\r\n%s\r\n", len(payload), payload)
		}
		_ = buf.Flush()

		// 停滞：不写终止块、不关闭连接，直到代理侧主动断开（兜底超时防止用例挂死）。
		disconnected := make(chan struct{})
		go func() {
			var b [256]byte
			for {
				if _, err := buf.Read(b[:]); err != nil {
					break
				}
			}
			close(disconnected)
		}()
		select {
		case <-disconnected:
		case <-time.After(20 * time.Second):
		}
	}), mutate)
}

// TestIdleTimeout_StreamContentPathEmitsErrorFrame 流式路径（内容已开流）遇上游停滞：
// SSE 的 200 头已经发出，改不了状态码，只能以流内错误帧收场。
// 断言同时钉住「确实是等到空闲超时才报」——错误必须在 idle 之后才出现。
func TestIdleTimeout_StreamContentPathEmitsErrorFrame(t *testing.T) {
	const idle = 150 * time.Millisecond
	f := newFakeCC(t)
	proxy := stallingProxy(t, f, []string{
		`{"type":"start"}`,
		`{"type":"text-delta","text":"half"}`,
	}, func(c *config.Config) {
		// 只把流式档位压到毫秒级：若实现误用 IdleBuffer（10s），下面的耗时断言会拦下它。
		c.IdleStream, c.IdleBuffer = idle, 10*time.Second
	})

	start := time.Now()
	resp := postMessages(t, proxy, anthropicReq, map[string]string{"x-api-key": "user_test123"})
	sse := readAll(t, resp)
	elapsed := time.Since(start)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200（内容已开流，状态码改不了）\nbody:\n%s", resp.StatusCode, sse)
	}
	if !strings.Contains(sse, "event: message_start") {
		t.Fatalf("流未正常开启:\n%s", sse)
	}
	// 停滞前已产出的内容必须在线，错误帧必须排在它之后。
	assertOrdered(t, "sse", sse, []string{`"text":"half"`, "event: error"})
	if !strings.Contains(sse, `"type":"rate_limit_error"`) || !strings.Contains(sse, "Response timeout") {
		t.Errorf("流内错误帧不是空闲超时出口:\n%s", sse)
	}
	// 停滞不是正常结束：不得补发收尾帧（message_start 里的 "stop_reason":null 不算收尾）。
	assertAbsent(t, "sse", sse, []string{"event: message_delta", "event: message_stop"})
	if elapsed < idle {
		t.Errorf("在空闲期限（%v）前就返回了：%v（说明走的不是空闲超时分支）", idle, elapsed)
	}
	if elapsed > 5*time.Second {
		t.Errorf("耗时 %v：流式档位应为 IdleStream（%v），不是 IdleBuffer", elapsed, idle)
	}
}

// TestIdleTimeout_StreamBeforeContentFallsBackToJSON 流式请求遇停滞但尚无任何内容：
// 200 头还没发（首帧以内容为准），此时必须回退为标准 HTTP JSON 错误响应并带上
// Retry-After，客户端才能按状态码退避重试。错误形状按端点协议（Chat → OpenAI 形状）。
func TestIdleTimeout_StreamBeforeContentFallsBackToJSON(t *testing.T) {
	const idle = 150 * time.Millisecond
	f := newFakeCC(t)
	proxy := stallingProxy(t, f, []string{`{"type":"start"}`}, func(c *config.Config) {
		c.IdleStream, c.IdleBuffer = idle, 10*time.Second
	})

	start := time.Now()
	resp := postChat(t, proxy, chatRequestBody(true), chatAuth)
	body := readAll(t, resp)
	elapsed := time.Since(start)

	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429\nbody:\n%s", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("content-type = %q, want application/json（未开流时必须回退 JSON）", ct)
	}
	if strings.Contains(body, "data: ") {
		t.Errorf("未开流时不得发 SSE 帧:\n%s", body)
	}
	if ra := resp.Header.Get("Retry-After"); ra != "5" {
		t.Errorf("Retry-After = %q, want 5（空闲超时出口）", ra)
	}
	assertOpenAIError(t, decodeJSON(t, body), "rate_limit_error", "rate_limit_exceeded", "Response timeout")
	if elapsed < idle {
		t.Errorf("在空闲期限（%v）前就返回了：%v", idle, elapsed)
	}
}

// TestIdleTimeout_BufferPathReturns429JSON 非流式（缓冲聚合）路径遇上游停滞：
// 用 IdleBuffer 档位（非流式默认 90s，比流式更宽），出口是 429 JSON 而非 502——
// 502 是「上游读错误」的出口，429 才代表「本地判定为超时、可退避重试」。
func TestIdleTimeout_BufferPathReturns429JSON(t *testing.T) {
	const idle = 150 * time.Millisecond
	f := newFakeCC(t)
	proxy := stallingProxy(t, f, []string{
		`{"type":"start"}`,
		`{"type":"text-delta","text":"half"}`,
	}, func(c *config.Config) {
		// 只压非流式档位：若实现误用 IdleStream（10s），耗时断言会拦下它。
		c.IdleStream, c.IdleBuffer = 10*time.Second, idle
	})

	start := time.Now()
	resp := postMessages(t, proxy, strings.Replace(anthropicReq, `"stream": true`, `"stream": false`, 1),
		map[string]string{"x-api-key": "user_test123"})
	body := readAll(t, resp)
	elapsed := time.Since(start)

	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429（缓冲路径超时出口）\nbody:\n%s", resp.StatusCode, body)
	}
	if ra := resp.Header.Get("Retry-After"); ra != "5" {
		t.Errorf("Retry-After = %q, want 5（空闲超时出口）", ra)
	}
	// 已产出内容但仍然超时：必须报超时而非零输出（零输出出口的 Retry-After 是 10）
	assertAnthropicError(t, decodeJSON(t, body), "rate_limit_error", "Response timeout")
	if elapsed < idle {
		t.Errorf("在空闲期限（%v）前就返回了：%v（说明走的不是空闲超时分支）", idle, elapsed)
	}
	if elapsed > 5*time.Second {
		t.Errorf("耗时 %v：缓冲路径应使用 IdleBuffer（%v），不是 IdleStream", elapsed, idle)
	}
}
