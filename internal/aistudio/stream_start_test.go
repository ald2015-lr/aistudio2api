package aistudio

import (
	"context"
	"testing"
	"time"
)

// TestStreamStartHold 写出响应头前的等待延长记录：开始与结束成对计数，延长按每次结束给出的时长累加（不大于 0 的不计），
// 状态变化时通知等待方（不阻塞写入方）；nil 上的方法都安全，context 里没有时为 nil
func TestStreamStartHold(t *testing.T) {
	var none *StreamStartHold
	none.BeginColdStart(time.Second)
	none.EndColdStart(time.Second)
	if extension, starting := none.State(); extension != 0 || starting || none.Changed() != nil || none.Extension() != 0 {
		t.Fatal("nil 上的方法应不做任何事")
	}
	if StreamStartHoldFromContext(context.Background()) != nil || WithStreamStartHold(context.Background(), nil) != context.Background() {
		t.Fatal("没有等待延长记录时 context 应原样")
	}
	hold := NewStreamStartHold()
	if StreamStartHoldFromContext(WithStreamStartHold(context.Background(), hold)) != hold {
		t.Fatal("应从 context 取回同一份记录")
	}
	hold.BeginColdStart(time.Second)
	hold.BeginColdStart(400 * time.Millisecond)
	if _, starting := hold.State(); !starting {
		t.Fatal("开始后应为进行中")
	}
	hold.EndColdStart(300 * time.Millisecond)
	if extension, starting := hold.State(); !starting || extension != 300*time.Millisecond {
		t.Fatalf("还有一次启动没结束：延长 %s 进行中=%v", extension, starting)
	}
	hold.EndColdStart(-time.Second)
	hold.EndColdStart(0)
	if extension, starting := hold.State(); starting || extension != 300*time.Millisecond {
		t.Fatalf("延长 %s 进行中=%v，期望 300ms、不在进行中（多余的结束不让计数变负）", extension, starting)
	}
	if hold.Extension() != 300*time.Millisecond {
		t.Fatalf("全部结束后要延长的等待 = %s，期望只剩实测的 300ms", hold.Extension())
	}
	select {
	case <-hold.Changed():
	default:
		t.Fatal("状态变化后应有通知")
	}
	select {
	case <-hold.Changed():
		t.Fatal("通知只保留一个，不应阻塞写入方也不应堆积")
	default:
	}
}

// TestStreamStartHoldExtensionLimit 进行中的现场冷启动最多只把写出响应头前的等待延长开始时给出的上限（这次启动还能抵扣的时长），
// 不随启动时长一直增加；结束后改为实测时长。原先启动进行中一直推迟写出响应头，抵扣用完后再现场冷启动一次，响应头就被整段启动压着，
// 多等的时间换不来以 HTTP 状态返回的 429
func TestStreamStartHoldExtensionLimit(t *testing.T) {
	hold := NewStreamStartHold()
	hold.BeginColdStart(700 * time.Millisecond)
	if got := hold.Extension(); got != 700*time.Millisecond {
		t.Fatalf("第一次启动进行中要延长 %s，期望上限 700ms", got)
	}
	hold.EndColdStart(600 * time.Millisecond)
	if got := hold.Extension(); got != 600*time.Millisecond {
		t.Fatalf("第一次启动结束后要延长 %s，期望实测 600ms", got)
	}
	// 第二次启动只剩 100ms 可抵扣：进行中最多延长到 700ms，不因启动更久而继续推迟
	hold.BeginColdStart(100 * time.Millisecond)
	if got := hold.Extension(); got != 700*time.Millisecond {
		t.Fatalf("第二次启动进行中要延长 %s，期望 600ms + 剩余的 100ms", got)
	}
	hold.EndColdStart(100 * time.Millisecond)
	// 抵扣用完：开始时上限为 0，进行中不再延长
	hold.BeginColdStart(0)
	if got := hold.Extension(); got != 700*time.Millisecond {
		t.Fatalf("抵扣用完后的启动进行中要延长 %s，期望保持 700ms", got)
	}
	hold.EndColdStart(0)
	if extension, starting := hold.State(); starting || extension != 700*time.Millisecond {
		t.Fatalf("延长 %s 进行中=%v，期望 700ms、不在进行中", extension, starting)
	}
}
