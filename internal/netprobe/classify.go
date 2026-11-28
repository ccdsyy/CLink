package netprobe

import (
	"sync"
	"time"
)

// NAT 映射行为分类（ROADMAP #14/15）。
// 现代 STUN 服务器普遍不支持 classic-STUN 的 CHANGE-REQUEST（#16 过滤行为
// 测试依赖它），故线上实现以「映射行为」分类为准：
//   EIM    端点无关映射（锥形，好打）
//   nonEIM 依赖地址映射（对称型家族，难打，策略上直奔中继/端口映射）
//   unknown 探测失败（STUN 全不可达等）
// 过滤行为（EIF/ADF/APDF）留待 #14 完整版（双服务器模拟 Test2/Test3）。

var (
	classMu      sync.Mutex
	classCache   string    // 最近分类结果
	classCacheAt time.Time // 缓存时间（10 分钟）
)

// MappingClass 返回本机 NAT 映射类别（带 10 分钟缓存）。
// 实现：同一本地 socket 向两个不同 STUN 服务器发 BindingRequest，
// 比较映射端口——不变 = EIM；变化 = 依赖地址（nonEIM）。
func MappingClass() string {
	classMu.Lock()
	if classCache != "" && time.Since(classCacheAt) < 10*time.Minute {
		v := classCache
		classMu.Unlock()
		return v
	}
	classMu.Unlock()

	res := mappingClassUncached()
	classMu.Lock()
	classCache, classCacheAt = res, time.Now()
	classMu.Unlock()
	return res
}

func mappingClassUncached() string {
	type hit struct {
		port int
		rtt  time.Duration
	}
	var hits []hit
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, node := range PublicSTUN[:2] { // 两个节点足够对比
		wg.Add(1)
		go func(node string) {
			defer wg.Done()
			start := time.Now()
			a, err := stunMapped(node)
			if err != nil {
				return
			}
			mu.Lock()
			hits = append(hits, hit{port: a.Port, rtt: time.Since(start)})
			mu.Unlock()
		}(node)
	}
	wg.Wait()

	switch {
	case len(hits) >= 2 && hits[0].port == hits[1].port:
		return "EIM"
	case len(hits) >= 2:
		return "nonEIM"
	case len(hits) == 1:
		return "unknown" // 仅一个节点可达，无法对比
	default:
		return "unknown"
	}
}

// PublicV4 经公共 STUN 探测公网 IPv4（端口为映射端口，仅 IP 有意义）。
func PublicV4() (string, error) {
	a, err := stunMapped(PublicSTUN[0])
	if err != nil {
		return "", err
	}
	if a.IP.To4() == nil {
		return "", errNoPublicV4
	}
	return a.IP.String(), nil
}

type errNatV4 struct{}

func (errNatV4) Error() string { return "公网 IPv4 不可得" }

var errNoPublicV4 = errNatV4{}
