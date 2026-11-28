package engine

import "sync"

// 仓库内容的写访问闸门。和 Rust 实现一样，所有会改仓库的入口串行化：
// 备份、改备注、删快照、清孤儿图标、恢复、整理仓库。
//
// 这不是业务策略，而是保护仓库本身：prune 会把「还没进索引的新包」
// 当成无引用数据删掉，若此时有备份正在写包，仓库会直接损坏。
// 只读操作（列表 / 概况 / 估算）不进闸门。
var writeGate sync.Mutex

func enterWriteGate() func() {
	writeGate.Lock()
	return writeGate.Unlock
}
