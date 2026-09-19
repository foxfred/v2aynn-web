//go:build race

package v2ray

// raceEnabled 表示当前测试二进制由 -race 构建，数据竞争检测器已启用。
//
// 放在 _test.go 里是为了不影响生产产物 —— 正式编译时这个文件根本不参与。
//
// 为什么需要它：并发缺陷（如 watch() 曾无锁读 m.running）只有在 -race 下才会暴露，
// 普通 go test 会静默通过。若不加此判断，这个测试就成了"永远通过的测试"，
// 比没有测试更糟 —— 它会给出虚假的安全感。
//
// 注意：-race 需要 C 编译器（cgo）。Windows 上若无 gcc，只能到有 gcc 的机器
// （或 Linux/ARM64 构建环境）上运行，例如：
//
//	go test -race ./internal/v2ray/
const raceEnabled = true
