package statsviz_serve

import (
	"net/http"

	"github.com/arl/statsviz"
)

// Serve 性能监控 HTTP 服务
//
// 提供两种性能可视化：
//   - /debug/pprof: Go 原生 pprof（CPU / 内存 / goroutine 等 profile 数据）
//   - /debug/statsviz: 实时图表（GC 频率 / 堆内存 / goroutine 数等）
//
// standalone 模式默认监听 0.0.0.0:4567（cmd/standalone/main.go 启动）
// 集群模式各服务独立 pprof 端口
//
// 性能调优用：开发期看 statsviz 图表 排查问题用 pprof 抓 profile
func Serve(addr string) error {
	err := statsviz.RegisterDefault()
	if err != nil {
		return err
	}
	err = http.ListenAndServe(addr, nil)
	if err != nil {
		return err
	}
	return nil
}
