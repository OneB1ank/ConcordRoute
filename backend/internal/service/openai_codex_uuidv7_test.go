package service

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// 逐位读取 RFC 可用区域，避免复制被测编码公式而形成自证测试。
func decodeUUIDv7CounterForTest(value uuid.UUID) uint64 {
	var counter uint64
	for bit := 52; bit < 96; bit++ {
		if bit == 64 || bit == 65 {
			continue
		}
		counter = (counter << 1) | uint64((value[bit/8]>>uint(7-bit%8))&1)
	}
	return counter
}

// 每个计数位都须独立保留，不能被 version 或 variant 掩码覆盖。
func TestEncodeCodexUUIDv7_CounterRoundTrip(t *testing.T) {
	for bit := 0; bit < 42; bit++ {
		t.Run(fmt.Sprintf("bit_%02d", bit), func(t *testing.T) {
			counter := uint64(1) << bit
			got := encodeCodexUUIDv7(0x0123456789ab, counter, [16]byte{})
			require.Equal(t, counter, decodeUUIDv7CounterForTest(got))
			require.Equal(t, uuid.Version(7), got.Version())
			require.Equal(t, uuid.RFC4122, got.Variant())
		})
	}
	for _, counter := range []uint64{0, 0x2a3456789ab, codexUUIDv7MaxCounter} {
		got := encodeCodexUUIDv7(0x0123456789ab, counter, [16]byte{})
		require.Equal(t, counter, decodeUUIDv7CounterForTest(got))
	}
}

// 即使前一个随机尾部取最大值、后一个为零，递增计数器仍须严格排序。
func TestEncodeCodexUUIDv7_AllCounterBoundaries(t *testing.T) {
	var maxRandom [16]byte
	for i := range maxRandom {
		maxRandom[i] = 0xff
	}
	for bit := 0; bit < 42; bit++ {
		t.Run(fmt.Sprintf("carry_%02d", bit), func(t *testing.T) {
			boundary := uint64(1) << bit
			before := encodeCodexUUIDv7(0x0123456789ab, boundary-1, maxRandom)
			after := encodeCodexUUIDv7(0x0123456789ab, boundary, [16]byte{})
			require.Less(t, bytes.Compare(before[:], after[:]), 0)
		})
	}
}

// 32 个随机位逐位注入；它们只影响尾部，不覆盖时间和计数器。
func TestEncodeCodexUUIDv7_RandomTailIsolation(t *testing.T) {
	const timestamp = uint64(0x0123456789ab)
	const counter = uint64(0x2a3456789ab)
	baseline := encodeCodexUUIDv7(timestamp, counter, [16]byte{})
	for bit := 0; bit < 32; bit++ {
		var randomBytes [16]byte
		randomBytes[6+bit/8] = byte(1 << uint(7-bit%8))
		got := encodeCodexUUIDv7(timestamp, counter, randomBytes)
		require.Equal(t, baseline[:12], got[:12], "random bit %d", bit)
		require.Equal(t, randomBytes[6:10], got[12:], "random bit %d", bit)
		require.Equal(t, counter, decodeUUIDv7CounterForTest(got))
	}
	// 尾部之外的随机输入不应混入计数区域。
	randomBytes := [16]byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff}
	got := encodeCodexUUIDv7(timestamp, counter, randomBytes)
	require.Equal(t, baseline, got)
}

// 时间独立于计数器和随机尾部，保留完整 48 位 Unix 毫秒字段。
func TestEncodeCodexUUIDv7_TimestampRoundTrip(t *testing.T) {
	for _, timestamp := range []uint64{0, 1, 0x0123456789ab, (1 << 48) - 1} {
		got := encodeCodexUUIDv7(timestamp, codexUUIDv7MaxCounter, [16]byte{})
		var decoded uint64
		for _, b := range got[:6] {
			decoded = decoded<<8 | uint64(b)
		}
		require.Equal(t, timestamp, decoded)
		require.Equal(t, uuid.Version(7), got.Version())
		require.Equal(t, uuid.RFC4122, got.Variant())
	}
}

// 固定时间而不修改全局时钟或共享生成器，验证停顿、回退、进位及溢出。
func TestCodexUUIDv7Context_ClockAndOverflow(t *testing.T) {
	c := codexUUIDv7Context{
		initialized: true, timestampMS: 1_000_100, lastSeedMS: 1_000_100,
		counter: (1 << 26) - 2,
	}
	var previous uuid.UUID
	for index, now := range []uint64{1_000_100, 999_900, 1_000_099, 1_000_100} {
		timestamp, counter := c.next(now)
		require.EqualValues(t, 1_000_100, timestamp)
		require.EqualValues(t, (1<<26)-1+index, counter)
		value := encodeCodexUUIDv7(timestamp, counter, [16]byte{})
		if index > 0 {
			require.Less(t, bytes.Compare(previous[:], value[:]), 0)
		}
		previous = value
	}
	timestamp, counter := c.next(1_000_101)
	require.EqualValues(t, 1_000_101, timestamp)
	require.Less(t, counter, uint64(1)<<41)

	c.counter = codexUUIDv7MaxCounter
	before := encodeCodexUUIDv7(timestamp, c.counter, [16]byte{})
	timestamp, counter = c.next(1_000_101)
	require.EqualValues(t, 1_000_102, timestamp)
	require.Less(t, counter, uint64(1)<<41)
	after := encodeCodexUUIDv7(timestamp, counter, [16]byte{})
	require.Less(t, bytes.Compare(before[:], after[:]), 0)
	heldTimestamp, incremented := c.next(1_000_101)
	require.Equal(t, timestamp, heldTimestamp)
	require.Equal(t, counter+1, incremented)

	var fresh codexUUIDv7Context
	timestamp, counter = fresh.next(1_000_100)
	require.EqualValues(t, 1_000_100, timestamp)
	require.Less(t, counter, uint64(1)<<41)
}

// 固定毫秒并跨越旧实现丢位的边界，唯一性不能依赖随机尾部碰巧不同。
func TestCodexUUIDv7Context_ConcurrentCounterUniqueness(t *testing.T) {
	const workers, perWorker = 16, 256
	c := codexUUIDv7Context{
		initialized: true, timestampMS: 1234, lastSeedMS: 1234,
		counter: (1 << 26) - 2048,
	}
	results := make(chan uuid.UUID, workers*perWorker)
	var workersDone sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		workersDone.Add(1)
		go func() {
			defer workersDone.Done()
			for i := 0; i < perWorker; i++ {
				timestamp, counter := c.next(1234)
				results <- encodeCodexUUIDv7(timestamp, counter, [16]byte{})
			}
		}()
	}
	workersDone.Wait()
	close(results)
	seen := make(map[uuid.UUID]struct{}, workers*perWorker)
	for value := range results {
		_, exists := seen[value]
		require.False(t, exists, "共享上下文不应重复分配同一计数器")
		seen[value] = struct{}{}
	}
	require.Len(t, seen, workers*perWorker)
}

// 新编码不重写仍在有效期内的旧完整 UUID；两类持久存储都覆盖冷缓存恢复。
func TestCodexUUIDv7_ExistingBindingsUnchanged(t *testing.T) {
	for _, store := range codexUUIDv7BindingStores {
		t.Run(store.extraKey, func(t *testing.T) {
			account := newTestOAuthAccount(9990000+codexSnapshotTestAccountID.Add(1), nil)
			t.Cleanup(func() { auditDropIdentityHotState(account.ID) })
			const seed = "uuid-codec-upgrade-fixture"
			const legacy = "01234567-89ab-78d0-9678-9ab506070809"
			storeCodexUUIDv7Binding(account, seed, store.extraKey, legacy, store.idleTTL, store.maxEntries, time.Now().UnixMilli())
			serialized, err := json.Marshal(account.Extra)
			require.NoError(t, err)
			auditDropIdentityHotState(account.ID)
			var restored map[string]any
			require.NoError(t, json.Unmarshal(serialized, &restored))
			account.Extra = restored
			got := deriveStableUUIDv7ForAccountStore(account, seed, store.extraKey, store.idleTTL, store.maxEntries)
			require.Equal(t, legacy, got)
		})
	}
}
