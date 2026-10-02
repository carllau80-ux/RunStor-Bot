package agent

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain 在所有用例结束后检查有没有 goroutine 泄漏。
//
// 并发代码光靠"测试通过"是不够的: 漏掉的 goroutine 会一直攥着连接、定时器或内存,
// 症状是"跑几天之后内存越来越高", 而单测每次都是绿的。goleak 把这件事变成一条断言。
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
