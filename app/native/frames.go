package main

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"unsafe"
)

// Kotlin 侧的共享内存布局（RusticFrames.kt）：
//
//	[0, 512KB)      请求 / 响应帧
//	[512KB, 576KB)  进度环形缓冲
//
// 帧格式统一为「大端 i32 长度 + UTF-8 JSON」。
const (
	responseArea = 512 * 1024
	progressArea = 64 * 1024

	progressOffset      = responseArea
	progressCounterSize = 4
	progressSlotSize    = 256
	progressSlotCount   = (progressArea - progressCounterSize) / progressSlotSize
)

// sharedBuffer 是 Java DirectByteBuffer 的一块视图，只在对应的 JNI 调用
// 返回前有效。Go 这边只保存裸地址和容量，读写都直接落在 Java 的映射内存上。
type sharedBuffer struct {
	ptr      unsafe.Pointer
	capacity int
}

func (b sharedBuffer) valid() bool {
	return b.ptr != nil && b.capacity > 4
}

func (b sharedBuffer) bytes() []byte {
	return unsafe.Slice((*byte)(b.ptr), b.capacity)
}

// readRequest 读取 offset 0 处的请求帧。
func (b sharedBuffer) readRequest() ([]byte, error) {
	if b.ptr == nil || b.capacity < 4 {
		return nil, fmt.Errorf("共享内存太小")
	}
	header := b.bytes()[:4]
	length := int(int32(binary.BigEndian.Uint32(header)))
	if length < 0 {
		return nil, fmt.Errorf("数据长度非法：%d", length)
	}
	if 4+length > b.capacity {
		return nil, fmt.Errorf("数据超出共享内存")
	}
	out := make([]byte, length)
	copy(out, b.bytes()[4:4+length])
	return out, nil
}

// writeFrameAt 在指定偏移处写一帧。装不下时写一条错误响应，
// 和 Rust 的 write_frame_at 一样，避免客户端拿到半截 JSON。
func (b sharedBuffer) writeFrameAt(offset int, payload []byte) {
	if b.ptr == nil || offset+4 > b.capacity {
		return
	}
	available := b.capacity - offset - 4
	if len(payload) > available {
		message := fmt.Sprintf(`{"ok":false,"error":"响应过大：%d 字节，缓冲区只有 %d 字节"}`,
			len(payload), available)
		payload = []byte(message)
		if len(payload) > available {
			payload = payload[:available]
		}
	}
	data := b.bytes()
	binary.BigEndian.PutUint32(data[offset:offset+4], uint32(len(payload)))
	copy(data[offset+4:offset+4+len(payload)], payload)
}

// writeResponse 把对象序列化后写到响应帧区。
func (b sharedBuffer) writeResponse(value any) bool {
	data, err := json.Marshal(value)
	if err != nil {
		return false
	}
	b.writeFrameAt(0, data)
	return true
}

// progressRing 是进度区：4 字节计数器 + 255 个固定大小槽位。
// 每条事件写一格，写完槽位再推进计数器；读端不会看到半截数据。
type progressRing struct {
	buf     sharedBuffer
	counter atomic.Uint32
	mu      sync.Mutex
}

func newProgressRing(buf sharedBuffer) *progressRing {
	return &progressRing{buf: buf}
}

// write 把一条进度帧写进环形缓冲。所有事件（步骤 / 字节 / interrupted）
// 都走这一条路径。
func (r *progressRing) write(frame string) {
	if r == nil || r.buf.ptr == nil || r.buf.capacity <= progressOffset+8 {
		return
	}

	payload := []byte(frame)
	r.mu.Lock()
	defer r.mu.Unlock()

	index := r.counter.Add(1)
	slot := int((index - 1) % uint32(progressSlotCount))
	offset := progressOffset + progressCounterSize + slot*progressSlotSize
	r.buf.writeFrameAt(offset, payload)

	// 槽位写好之后再推进计数。
	counter := r.buf.bytes()[progressOffset : progressOffset+progressCounterSize]
	binary.BigEndian.PutUint32(counter, index)
}
