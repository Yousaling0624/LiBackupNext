package engine

import (
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Progress 把 native 侧的步骤事件和字节进度编码成主进程认识的 JSON 帧。
// 帧的字段顺序、缺省规则、节流间隔都沿用原来的 Rust 实现。
type Progress struct {
	mu    sync.Mutex
	write func(string)
	stage string
	start time.Time
}

var (
	activeProgress atomic.Pointer[Progress]
	progressEpoch  = time.Now()
)

func NewProgress(write func(string)) *Progress {
	return &Progress{write: write, start: progressEpoch}
}

func (p *Progress) SetStage(stage string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.stage = stage
	p.mu.Unlock()
}

func (p *Progress) currentStage() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.stage
}

// Running 记录当前步骤并写一条 running 事件。
func (p *Progress) Running(stage string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.stage = stage
	p.writeLocked(eventJSON(stage, "running", nil, 0, "", false))
	p.mu.Unlock()
}

// Done 写完成事件。files 只在已知时带上；errors 为 0 时省略。
func (p *Progress) Done(stage string, files *uint64, errors uint64) {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.writeLocked(eventJSON(stage, "done", files, errors, "", errors > 0))
	p.mu.Unlock()
}

func (p *Progress) Skipped(stage, reason string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.writeLocked(eventJSON(stage, "skipped", nil, 0, reason, false))
	p.mu.Unlock()
}

func (p *Progress) Interrupted(stage string, reason Reason) {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.writeLocked(eventJSON(stage, "interrupted", nil, 0, string(reason), false))
	p.mu.Unlock()
}

func (p *Progress) Finish() {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.writeLocked(`{"stage":"","state":"finished"}`)
	p.mu.Unlock()
}

func (p *Progress) writeLocked(frame string) {
	if p.write != nil {
		p.write(frame)
	}
}

// SetActiveProgress 让传输层包装能拿到当前这次调用的进度端。备份 / 恢复完成后
// 必须 ClearActiveProgress，否则下一次列目录时的网络错误会被当成进度中断。
func SetActiveProgress(p *Progress) { activeProgress.Store(p) }

func ClearActiveProgress() { activeProgress.Store(nil) }

// interruptActive 在传输层错误发生时写 interrupted 帧。没有活动进度端
// （列目录、改备注等）就静默跳过。
func interruptActive(reason Reason) {
	p := activeProgress.Load()
	if p == nil {
		return
	}
	p.Interrupted(p.currentStage(), reason)
}

// BytesProgress 对应一个快照的字节进度。total 是扫描得到的源字节数，
// current 由 restic 的 CompleteBlob 回调累加。
type BytesProgress struct {
	parent    *Progress
	total     atomic.Uint64
	current   atomic.Uint64
	lastEmit  atomic.Int64
	startTime time.Time
}

func (p *Progress) NewBytes() *BytesProgress {
	if p == nil {
		return nil
	}
	return &BytesProgress{parent: p, startTime: progressEpoch}
}

func (b *BytesProgress) SetTotal(total uint64) {
	if b == nil {
		return
	}
	b.total.Store(total)
	b.emit(true)
}

func (b *BytesProgress) Add(n uint64) {
	if b == nil {
		return
	}
	b.current.Add(n)
	b.emit(false)
}

func (b *BytesProgress) Finish() {
	if b == nil {
		return
	}
	b.emit(true)
}

// ProgressThrottle 沿用 Rust 的 200ms：读端 60ms 轮询一次，写太密只会挤掉步骤事件。
const ProgressThrottle = 200 * time.Millisecond

func (b *BytesProgress) emit(force bool) {
	now := time.Since(b.startTime).Milliseconds()
	if !force {
		last := b.lastEmit.Load()
		if now-last < ProgressThrottle.Milliseconds() {
			return
		}
	}
	b.lastEmit.Store(now)

	p := b.parent
	p.mu.Lock()
	defer p.mu.Unlock()
	frame := `{"stage":"` + escapeJSON(p.stage) + `","state":"progress","total":` +
		strconv.FormatUint(b.total.Load(), 10) + `,"current":` +
		strconv.FormatUint(b.current.Load(), 10) + `}`
	p.writeLocked(frame)
}

func eventJSON(stage, state string, files *uint64, errors uint64, reason string, includeErrors bool) string {
	var b strings.Builder
	b.WriteString(`{"stage":"`)
	b.WriteString(escapeJSON(stage))
	b.WriteString(`","state":"`)
	b.WriteString(state)
	b.WriteString(`"`)
	if files != nil {
		b.WriteString(`,"files":`)
		b.WriteString(strconv.FormatUint(*files, 10))
	}
	if includeErrors {
		b.WriteString(`,"errors":`)
		b.WriteString(strconv.FormatUint(errors, 10))
	}
	if reason != "" {
		b.WriteString(`,"reason":"`)
		b.WriteString(escapeJSON(reason))
		b.WriteString(`"`)
	}
	b.WriteByte('}')
	return b.String()
}

func escapeJSON(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, `"`, `\"`)
	value = strings.ReplaceAll(value, "\n", " ")
	return value
}
