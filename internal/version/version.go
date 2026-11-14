// 日期: 2026-11-14 08:02:31.727
// 作者: Zhai Wenjia

// Package version 承载运行时版本号。
//
// 版本的唯一真值是 git tag（v主.大.小），⛔ 不在源码硬编码版本号。
// 构建时用 -ldflags 把 tag 注入 Version 变量：
//
//	go build -ldflags "-X github.com/zhaiwenjia/FlyingGateway/internal/version.Version=$(git describe --tags --always)" ./cmd/gateway
//
// 未注入时 Version 为 "dev"（本地直跑 / 测试），调用方不得把 "dev" 当发布版本。
package version

// Version 由 ldflags 注入；未注入时为 "dev"。
var Version = "dev"
