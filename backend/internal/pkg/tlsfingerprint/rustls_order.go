package tlsfingerprint

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"slices"
	"sort"
)

// rustlsExtensionOrder 对齐 rustls 0.23.36 的扩展排序，而不是通用 Shuffle。
// 来源：rustls/src/msgs/handshake.rs 的 order_insensitive_extensions_in_random_order
// 和 low_quality_integer_hash（rustls，Apache-2.0 / ISC / MIT）。
// 当前模板没有真实 ECH 的 contiguous_extensions，故仅处理普通组及特殊后缀。
// 返回副本，不修改共享模板；同一握手只在生成首个 ClientHello 时选择一次种子。
func rustlsExtensionOrder(ids []uint16, seed uint16) []uint16 {
	order := make([]uint16, 0, len(ids))
	for _, id := range ids {
		if id != 41 && id != 65037 && id != 64768 {
			order = append(order, id)
		}
	}
	sort.SliceStable(order, func(i, j int) bool {
		return rustlsOrderKey(seed, order[i]) < rustlsOrderKey(seed, order[j])
	})
	// rustls 的编码后缀依次为 outer_extensions、ECH、PSK；PSK 必须最后。
	for _, id := range []uint16{64768, 65037, 41} {
		if slices.Contains(ids, id) {
			order = append(order, id)
		}
	}
	return order
}

// rustlsOrderKey 是非密码学排序混合函数，uint32 运算保留 Rust wrapping_add 语义。
// 种子只来源于建连时的安全随机数，与账号、UA、业务会话标识无关。
func rustlsOrderKey(seed, id uint16) uint32 {
	x := uint32(seed)<<16 | uint32(id)
	x = x + 0x7ed55d16 + (x << 12)
	x = (x ^ 0xc761c23c) ^ (x >> 19)
	x = x + 0x165667b1 + (x << 5)
	x = (x + 0xd3a2646c) ^ (x << 9)
	x = x + 0xfd7046c5 + (x << 3)
	return (x ^ 0xb55a4f09) ^ (x >> 16)
}

func newRustlsOrderSeed() uint16 {
	var seed [2]byte
	// Go 1.26 的 crypto/rand.Read 总是填满缓冲区；熵源异常会终止进程，不降级为固定种子。
	_, _ = rand.Read(seed[:])
	return binary.BigEndian.Uint16(seed[:])
}

// ValidateRustlsNativeOrder 阻止将尚未支持的模板数据误当作原生握手能力。
// 排序不负责生成 PSK binder、真实 ECH 压缩块或首包 Cookie。
func (p *Profile) ValidateRustlsNativeOrder() error {
	if p == nil || !p.RustlsNativeOrder {
		return nil
	}
	if p.EnableGREASE {
		return fmt.Errorf("rustls 原生排序请关闭 GREASE，避免自动插入扩展改变排序语义")
	}
	for _, id := range p.Extensions {
		if isGREASEValue(id) {
			return fmt.Errorf("rustls 原生排序请移除 GREASE 扩展 0x%04x", id)
		}
		switch id {
		case 41, 44, 64768:
			return fmt.Errorf("rustls 排序模板暂不支持手动声明扩展 %d（需要握手状态数据）", id)
		}
	}
	return nil
}
