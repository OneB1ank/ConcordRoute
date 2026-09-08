package tlsfingerprint

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
)

// 此摘要由 rustls 0.23.36 原始函数经 rustc 1.97.1 编译生成，
// 输入为以下 11 个扩展和全部 65536 个种子，输出按 uint16 大端编码。
// 禁止使用被测 Go 实现重新生成预期值。
func TestRustlsNativeOrderReference(t *testing.T) {
	ids := []uint16{0, 5, 10, 11, 13, 16, 23, 35, 43, 45, 51}
	digest := sha256.New()
	for seed := 0; seed < 65536; seed++ {
		for _, id := range rustlsExtensionOrder(ids, uint16(seed)) {
			var encoded [2]byte
			binary.BigEndian.PutUint16(encoded[:], id)
			_, _ = digest.Write(encoded[:])
		}
	}
	require.Equal(t, "aa96ae7f5152a59bd204e74d8149cbd85b55fc186e00d6a1fdbff07c69f21cfd",
		fmt.Sprintf("%x", digest.Sum(nil)))
	require.Equal(t, []uint16{0, 5, 10, 11, 13, 16, 23, 35, 43, 45, 51}, ids)
}

// 固定参考向量同时检查溢出、输入排列无关性和 ECH/PSK 后缀。
func TestRustlsNativeOrderConstraints(t *testing.T) {
	ids := []uint16{51, 45, 43, 35, 23, 16, 13, 11, 10, 5, 0}
	expected := []uint16{45, 0, 11, 43, 16, 51, 35, 10, 5, 23, 13}
	require.Equal(t, expected, rustlsExtensionOrder(ids, 42))
	withSuffix := append([]uint16{41, 65037, 64768}, ids...)
	require.Equal(t, append(slices.Clone(expected), 64768, 65037, 41),
		rustlsExtensionOrder(withSuffix, 42))
	require.Equal(t, []uint16{64768, 65037, 41},
		rustlsExtensionOrder([]uint16{41, 65037, 64768}, 65535))
	require.Empty(t, rustlsExtensionOrder(nil, 0))
}

// 排序策略属于连接池身份；随机种子不进入模板，也不使池键逐次抖动。
func TestRustlsNativeOrderCacheIsolation(t *testing.T) {
	fixed := &Profile{Name: "test", Extensions: []uint16{0, 5, 10, 11, 13, 16, 23, 35, 43, 45, 51}}
	native := *fixed
	native.RustlsNativeOrder = true
	fixedKey, nativeKey := CacheKey(fixed), CacheKey(&native)
	require.NotEqual(t, fixedKey, nativeKey)
	orders := map[string]bool{}
	for range 32 {
		spec := buildClientHelloSpecFromProfile(&native)
		var ids []uint16
		for _, ext := range spec.Extensions {
			// 类型编号来自现有扩展编码，不读取随机 key_share 等内容。
			data := make([]byte, ext.Len())
			_, _ = ext.Read(data)
			if len(data) >= 4 {
				ids = append(ids, binary.BigEndian.Uint16(data))
			}
		}
		orders[fmt.Sprint(ids)] = true
		require.Equal(t, nativeKey, CacheKey(&native))
		require.Equal(t, fixed.Extensions, native.Extensions)
	}
	require.Greater(t, len(orders), 1)
	require.Equal(t, fixedKey, CacheKey(fixed))
	native.RustlsNativeOrder = false
	require.Equal(t, fixedKey, CacheKey(&native))
}

// WS 和 HTTP/1.1 回退通过此公共副本接口剥离 h2，排序策略仍应保留。
func TestRustlsNativeOrderHTTP1Clone(t *testing.T) {
	profile := &Profile{RustlsNativeOrder: true, ALPNProtocols: []string{"h2", "http/1.1"}}
	cloned := HTTP1OnlyProfile(profile)
	require.True(t, cloned.RustlsNativeOrder)
	require.Equal(t, []string{"http/1.1"}, cloned.ALPNProtocols)
	require.Equal(t, []string{"h2", "http/1.1"}, profile.ALPNProtocols)
	require.NotEqual(t, CacheKey(profile), CacheKey(cloned))
}
