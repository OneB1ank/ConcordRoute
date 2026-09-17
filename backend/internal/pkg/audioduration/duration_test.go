package audioduration

import (
	"bytes"
	"context"
	"encoding/binary"
	"os/exec"
	"testing"

	"github.com/stretchr/testify/require"
)

// syntheticWAV 产生静音样本，只用于时长与容器回归测试。
func syntheticWAV(seconds int) []byte {
	b := make([]byte, 44+seconds*32000)
	copy(b, "RIFF")
	binary.LittleEndian.PutUint32(b[4:], uint32(len(b)-8))
	copy(b[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(b[16:], 16)
	binary.LittleEndian.PutUint16(b[20:], 1)
	binary.LittleEndian.PutUint16(b[22:], 1)
	binary.LittleEndian.PutUint32(b[24:], 16000)
	binary.LittleEndian.PutUint32(b[28:], 32000)
	binary.LittleEndian.PutUint16(b[32:], 2)
	binary.LittleEndian.PutUint16(b[34:], 16)
	copy(b[36:], "data")
	binary.LittleEndian.PutUint32(b[40:], uint32(seconds*32000))
	return b
}

func TestAudioDurationWAV(t *testing.T) {
	data := syntheticWAV(2)
	seconds, err := Measure(context.Background(), data)
	require.NoError(t, err)
	require.Equal(t, float64(2), seconds)
	binary.LittleEndian.PutUint32(data[28:], 1)
	_, ok := ParseWAV(data)
	require.False(t, ok, "伪造 byteRate 不参与计费")
	_, ok = ParseWAV(syntheticWAV(1)[:100])
	require.False(t, ok, "截断文件不信任声明时长")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = Measure(ctx, syntheticWAV(1))
	require.ErrorIs(t, err, context.Canceled)
	_, err = Measure(context.Background(), syntheticWAV(601))
	require.ErrorIs(t, err, ErrInvalid)
}

func TestAudioDurationCompressedLocal(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("压缩容器集成测试需要本机 ffmpeg")
	}
	for _, format := range []struct{ name, codec string }{{"webm", "libopus"}, {"mp3", "libmp3lame"}, {"ogg", "libopus"}} {
		t.Run(format.name, func(t *testing.T) {
			cmd := exec.Command(ffmpeg, "-nostdin", "-v", "error", "-f", "wav", "-i", "pipe:0",
				"-c:a", format.codec, "-f", format.name, "pipe:1")
			cmd.Stdin = bytes.NewReader(syntheticWAV(2))
			data, err := cmd.Output()
			require.NoError(t, err)
			seconds, err := Measure(context.Background(), data)
			require.NoError(t, err)
			require.InDelta(t, 2, seconds, 0.12)
			// 相同录音长度的压缩文件大小显著不同，测量不能按字节数估算。
			require.Less(t, len(data), len(syntheticWAV(2)))
		})
	}
	_, err = Measure(context.Background(), []byte("https://example.invalid/audio.wav"))
	require.ErrorIs(t, err, ErrInvalid)
}

func TestAudioDurationMissingDecoderAndBusy(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	_, err := Measure(context.Background(), []byte("not wav"))
	require.ErrorIs(t, err, ErrUnavailable)
	decodeSlots <- struct{}{}
	decodeSlots <- struct{}{}
	defer func() { <-decodeSlots; <-decodeSlots }()
	_, err = Measure(context.Background(), []byte("not wav"))
	require.ErrorIs(t, err, ErrBusy)
}
