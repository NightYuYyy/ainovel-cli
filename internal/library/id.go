package library

import (
	"crypto/rand"
	"fmt"
)

// newTaskID 生成一个简短的唯一任务 ID。
// 使用 8 字节随机数编码为 hex 串，碰撞概率极低。
func newTaskID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// 极端情况：随机源失效，回退到时间戳
		panic(fmt.Sprintf("crypto/rand.Read failed: %v", err))
	}
	return fmt.Sprintf("%x", b[:])
}
