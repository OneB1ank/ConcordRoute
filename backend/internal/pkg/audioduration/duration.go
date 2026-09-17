// Package audioduration 在内存中测量上传录音，避免用文件大小猜测计费时长。
package audioduration

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"os/exec"
	"time"
)

const MaxSeconds = 600

var (
	ErrInvalid     = errors.New("invalid audio or duration exceeds 10 minutes")
	ErrUnavailable = errors.New("audio decoder unavailable; upload PCM WAV or install ffmpeg")
	ErrBusy        = errors.New("audio decoder is busy")
	decodeSlots    = make(chan struct{}, 2)
)

// Measure 优先检查 PCM WAV；压缩音频通过受限解码计数，既不保存原音频也不相信客户端时长。
func Measure(ctx context.Context, data []byte) (float64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if seconds, ok := ParseWAV(data); ok {
		if seconds > MaxSeconds {
			return 0, ErrInvalid
		}
		return seconds, nil
	}
	select {
	case decodeSlots <- struct{}{}:
		defer func() { <-decodeSlots }()
	default:
		return 0, ErrBusy
	}
	path, err := exec.LookPath("ffmpeg")
	if err != nil {
		return 0, ErrUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	// 只允许管道协议和常见音频容器；禁用网络、字幕与视频输出，最多解码十分钟。
	cmd := exec.CommandContext(ctx, path, "-nostdin", "-hide_banner", "-loglevel", "error",
		"-max_alloc", "33554432", "-threads", "1", "-protocol_whitelist", "pipe",
		"-format_whitelist", "wav,matroska,webm,ogg,mp3,mov,aac,flac",
		"-i", "pipe:0", "-map", "0:a:0", "-vn", "-sn", "-dn",
		"-threads", "1", "-ac", "1", "-ar", "16000", "-f", "s16le", "pipe:1")
	cmd.Stdin = bytes.NewReader(data)
	sink := &pcmCounter{limit: MaxSeconds * 32000}
	cmd.Stdout = sink
	// 不采集解码器 stderr，避免容器元数据进入诊断日志。
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		return 0, ErrInvalid
	}
	if sink.n <= 0 {
		return 0, ErrInvalid
	}
	return float64(sink.n) / 32000, nil
}

type pcmCounter struct{ n, limit int }

func (c *pcmCounter) Write(p []byte) (int, error) {
	if len(p) > c.limit-c.n {
		return 0, ErrInvalid
	}
	c.n += len(p)
	return len(p), nil
}

// ParseWAV 只接受完整且速率自洽的 PCM 文件；不把伪造 byteRate 或截断文件当成有效计费长度。
func ParseWAV(data []byte) (float64, bool) {
	if len(data) < 44 || string(data[:4]) != "RIFF" || string(data[8:12]) != "WAVE" {
		return 0, false
	}
	end := uint64(binary.LittleEndian.Uint32(data[4:8])) + 8
	if end != uint64(len(data)) {
		return 0, false
	}
	var rate, audioBytes uint64
	var fmtSeen, dataSeen bool
	for off, chunks := uint64(12), 0; off+8 <= end && chunks < 128; chunks++ {
		size := uint64(binary.LittleEndian.Uint32(data[off+4 : off+8]))
		start := off + 8
		if start+size > end {
			return 0, false
		}
		switch string(data[off : off+4]) {
		case "fmt ":
			if fmtSeen || size < 16 {
				return 0, false
			}
			f := data[start : start+16]
			if binary.LittleEndian.Uint16(f[:2]) != 1 {
				return 0, false
			}
			channels := uint64(binary.LittleEndian.Uint16(f[2:4]))
			sampleRate := uint64(binary.LittleEndian.Uint32(f[4:8]))
			bits := uint64(binary.LittleEndian.Uint16(f[14:16]))
			if channels == 0 || channels > 8 || sampleRate < 8000 || sampleRate > 192000 ||
				(bits != 8 && bits != 16 && bits != 24 && bits != 32) {
				return 0, false
			}
			align := channels * bits / 8
			rate = sampleRate * align
			if rate != uint64(binary.LittleEndian.Uint32(f[8:12])) ||
				align != uint64(binary.LittleEndian.Uint16(f[12:14])) {
				return 0, false
			}
			fmtSeen = true
		case "data":
			if dataSeen {
				return 0, false
			}
			audioBytes, dataSeen = size, true
		}
		off = start + size + size%2
	}
	if !fmtSeen || !dataSeen || audioBytes == 0 || rate == 0 {
		return 0, false
	}
	return float64(audioBytes) / float64(rate), true
}
