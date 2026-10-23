//go:build unix

package proxyrelay

import (
	"errors"
	"testing"
)

func TestDefaultTotalLimit(t *testing.T) {
	original := getrlimitNoFile
	defer func() { getrlimitNoFile = original }()
	cases := []struct {
		soft     uint64
		err      error
		expected int
	}{
		{soft: 1024, expected: 1024},
		{soft: 8192, expected: 2048},
		{soft: 100000, expected: 25000},
		{soft: 1 << 20, expected: 65536},
		{soft: ^uint64(0), expected: 65536},
		{err: errors.New("getrlimit 失败"), expected: 1024},
	}
	for _, each := range cases {
		getrlimitNoFile = func() (uint64, error) { return each.soft, each.err }
		if got := defaultTotalConnections(); got != each.expected {
			t.Fatalf("软限制 %d（%v）时上限 = %d，期望 %d", each.soft, each.err, got, each.expected)
		}
		registry := NewRegistry(Options{})
		_ = registry.Close()
		if got := registry.options.MaxTotalConnections; got != each.expected {
			t.Fatalf("注册表默认上限 = %d，期望 %d", got, each.expected)
		}
	}
	registry := NewRegistry(Options{MaxTotalConnections: 7})
	_ = registry.Close()
	if got := registry.options.MaxTotalConnections; got != 7 {
		t.Fatalf("显式上限被覆盖：%d", got)
	}
}
