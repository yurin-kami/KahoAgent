package chunker

import "time"

// timeoutAfterSeconds 返回一个在指定秒数后触发的通道。
// 用于给可能死循环的测试加超时保护。
func timeoutAfterSeconds(n int) <-chan time.Time {
	return time.After(time.Duration(n) * time.Second)
}
