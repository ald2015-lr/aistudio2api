package proxyrelay

import (
	"io"
	"net"
	"sync"
	"time"
)

// pipe 双向对拷；一个方向读到 EOF 后对另一侧 CloseWrite，两个方向都结束（或兜底到期）后关闭两侧。
// 隧道阶段不设空闲超时：流式生成可能长时间没有字节，死连接靠 TCP keepalive 发现
func pipe(client, upstream net.Conn, grace time.Duration) {
	var closeOnce sync.Once
	closeBoth := func() {
		closeOnce.Do(func() {
			_ = client.Close()
			_ = upstream.Close()
		})
	}
	var timerMu sync.Mutex
	var timer *time.Timer
	finished := 0
	// halfDone 在一个方向结束时调用：第一个方向结束后启动兜底计时，防止对端不发 FIN 导致连接泄漏
	halfDone := func() {
		timerMu.Lock()
		defer timerMu.Unlock()
		finished++
		if finished == 1 {
			timer = time.AfterFunc(grace, closeBoth)
			return
		}
		timer.Stop()
	}
	copyHalf := func(destination, source net.Conn) {
		if _, err := io.Copy(destination, source); err != nil {
			closeBoth()
		} else if err := closeWrite(destination); err != nil {
			closeBoth()
		}
		halfDone()
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		copyHalf(upstream, client)
	}()
	copyHalf(client, upstream)
	<-done
	closeBoth()
}

// closeWrite 半关闭写方向：*net.TCPConn 与 *tls.Conn 都支持，类型不支持时直接关闭
func closeWrite(connection net.Conn) error {
	if writer, ok := connection.(interface{ CloseWrite() error }); ok {
		return writer.CloseWrite()
	}
	return connection.Close()
}
