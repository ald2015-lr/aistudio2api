package config

import (
	"slices"
	"testing"
)

// requireInvalidKey 断言 .env 里取值无效时照常加载（不报错），并把 key 记入无效项；返回这一项供调用方核对原值与原因
func requireInvalidKey(t *testing.T, content string, cfg Config, err error, key string) InvalidValue {
	t.Helper()
	if err != nil {
		t.Fatalf("%q：取值无效应按默认值加载，得到错误 %v", content, err)
	}
	index := slices.IndexFunc(cfg.Invalid, func(item InvalidValue) bool { return item.Key == key })
	if index < 0 {
		t.Fatalf("%q：应把 %s 记为无效项，得到 %+v", content, key, cfg.Invalid)
	}
	return cfg.Invalid[index]
}
