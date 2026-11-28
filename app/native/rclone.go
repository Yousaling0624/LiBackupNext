package main

// 进程内 rclone：不再 dlopen librclone.so，直接链接 rclone 的 Go 实现。
// 对 Kotlin 暴露的 RC 方法和请求 / 响应形状与原来的 rclone_bridge 一致。

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	// 只注册项目用到的四个后端：和旧 librclone 一样裁掉其余后端与 mount / sync，
	// 否则 c-shared 库会白白大出几十 MB。
	_ "github.com/rclone/rclone/backend/local"
	_ "github.com/rclone/rclone/backend/sftp"
	_ "github.com/rclone/rclone/backend/smb"
	_ "github.com/rclone/rclone/backend/webdav"
	_ "github.com/rclone/rclone/cmd/serve"
	_ "github.com/rclone/rclone/cmd/serve/restic"
	"github.com/rclone/rclone/fs/config/obscure"
	_ "github.com/rclone/rclone/fs/operations" // operations/list、operations/mkdir
	librclone "github.com/rclone/rclone/librclone/librclone"
)

const serveStartTimeout = 15 * time.Second

var (
	rcloneOnce sync.Once
	rcloneUp   atomic.Bool
)

// ensureRclone 第一次用到远程能力时才初始化；重复调用是空操作。
func ensureRclone() {
	if rcloneUp.Load() {
		return
	}
	rcloneOnce.Do(func() {
		librclone.Initialize()
		rcloneUp.Store(true)
	})
}

type rpcResult struct {
	value map[string]any
	err   error
}

// rpc 调一次 RC 方法；非 200 时把 rclone 返回的 error 原样带出来。
func rpc(method string, input map[string]any) (map[string]any, error) {
	if !rcloneUp.Load() {
		return nil, errors.New("librclone 尚未加载")
	}

	payload, err := json.Marshal(input)
	if err != nil {
		return nil, err
	}

	var (
		text   string
		status int
	)
	func() {
		defer func() {
			if r := recover(); r != nil {
				text = fmt.Sprintf(`{"error":%q}`, fmt.Sprint(r))
				status = 500
			}
		}()
		text, status = librclone.RPC(method, string(payload))
	}()

	value := make(map[string]any)
	if err := json.Unmarshal([]byte(text), &value); err != nil {
		value = map[string]any{"error": strings.TrimSpace(text)}
	}
	if status != 200 {
		message, _ := value["error"].(string)
		if message == "" {
			message = "远程服务调用失败"
		}
		return nil, fmt.Errorf("%s 失败：%s", method, message)
	}
	return value, nil
}

// rpcWithTimeout 给 `serve/start` 加硬上限：远端不可达时 rclone 可能卡十几分钟。
// 超时后主线程先报错，后台线程跑完会自己停掉刚建好的 serve。
func rpcWithTimeout(method string, input map[string]any, timeout time.Duration) (map[string]any, error) {
	done := make(chan rpcResult, 1)
	var abandoned atomic.Bool

	go func() {
		value, err := rpc(method, input)
		if abandoned.Load() {
			if err == nil {
				if id, _ := value["id"].(string); id != "" {
					_, _ = rpc("serve/stop", map[string]any{"id": id})
				}
			}
			return
		}
		done <- rpcResult{value: value, err: err}
	}()

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case result := <-done:
		return result.value, result.err
	case <-timer.C:
		abandoned.Store(true)
		return nil, fmt.Errorf("启动远程服务超时（%ds）", int(timeout.Seconds()))
	}
}

// prepareFS 把 Kotlin 传来的 inline 配置转成可以直接喂给 RC 的 JSON 对象。
//
// inline 配置里的 pass 会被 rclone 当作「已混淆密码」去解密，明文必须转换一次。
// `insecure` 是本项目自定义的键，webdav 等 backend 不认得它，先剥离，
// 再作为 RC 顶层的全局参数下发。
func prepareFS(fsJSON string) (map[string]any, bool, error) {
	fs := make(map[string]any)
	if err := json.Unmarshal([]byte(fsJSON), &fs); err != nil {
		return nil, false, fmt.Errorf("远程服务配置无效：%v", err)
	}
	if pass, ok := fs["pass"].(string); ok && pass != "" {
		obscured, err := obscure.Obscure(pass)
		if err != nil {
			return nil, false, errors.New("服务密码转换失败")
		}
		fs["pass"] = obscured
	}
	insecure := false
	if value, ok := fs["insecure"]; ok {
		insecure, _ = value.(bool)
		delete(fs, "insecure")
	}
	return fs, insecure, nil
}

// ==================== restic 仓库服务 ====================

type serveState struct {
	key string
	url string
	id  string
}

var (
	serveMu     sync.Mutex
	activeServe *serveState
	serveSeq    atomic.Uint64
)

func token() string {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err == nil {
		return hex.EncodeToString(buf[:])
	}
	// 极端情况下退回时间戳 + 序号，仍然只留在内存里。
	return fmt.Sprintf("fallback%x", serveSeq.Add(1))
}

func stopServe() {
	serveMu.Lock()
	defer serveMu.Unlock()
	stopServeLocked()
}

func stopServeLocked() {
	if activeServe == nil {
		return
	}
	old := activeServe
	activeServe = nil
	_, _ = rpc("serve/stop", map[string]any{"id": old.id})
}

// remoteURL 起（或复用）一个 serve restic，返回给 RestBackend 的 URL。
func remoteURL(fs map[string]any, key string, insecure bool) (string, error) {
	serveMu.Lock()
	defer serveMu.Unlock()

	if activeServe != nil && activeServe.key == key {
		return activeServe.url, nil
	}
	stopServeLocked()

	user := token()
	pass := token()
	// 15s 硬上限：正常链路（握手 + 一次目录请求）在 1~4s 内完成。
	response, err := rpcWithTimeout("serve/start", map[string]any{
		"type":                 "restic",
		"fs":                   fs,
		"addr":                 []string{"127.0.0.1:0"},
		"user":                 user,
		"pass":                 pass,
		"no_check_certificate": insecure,
	}, serveStartTimeout)
	if err != nil {
		return "", err
	}

	addr, _ := response["addr"].(string)
	if addr == "" {
		return "", errors.New("serve/start 未返回监听地址")
	}
	id, _ := response["id"].(string)
	if id == "" {
		return "", errors.New("serve/start 未返回服务 id")
	}

	url := fmt.Sprintf("http://%s:%s@%s/", user, pass, addr)
	activeServe = &serveState{key: key, url: url, id: id}
	return url, nil
}

// ==================== 远程目录浏览 ====================

func joinRemote(path, name string) (string, error) {
	if name == "" || strings.ContainsAny(name, `/\`) || name == ".." {
		return "", errors.New("文件夹名称不合法")
	}
	base := strings.Trim(path, "/")
	if base == "" {
		return name, nil
	}
	return base + "/" + name, nil
}

// listDirs 列一层子目录；create 不为空时先在该层建一个文件夹再重新列。
func listDirs(fs map[string]any, path string, create *string, insecure bool) ([]string, error) {
	if create != nil {
		target, err := joinRemote(path, *create)
		if err != nil {
			return nil, err
		}
		if _, err := rpc("operations/mkdir", map[string]any{
			"fs":                   fs,
			"remote":               target,
			"no_check_certificate": insecure,
			"contimeout":           "5s",
			"timeout":              "10s",
			"low_level_retries":    1,
		}); err != nil {
			return nil, err
		}
	}

	response, err := rpc("operations/list", map[string]any{
		"fs":                   fs,
		"remote":               strings.Trim(path, "/"),
		"no_check_certificate": insecure,
		"contimeout":           "5s",
		"timeout":              "10s",
		"low_level_retries":    1,
	})
	if err != nil {
		return nil, err
	}

	entries, ok := response["list"].([]any)
	if !ok {
		return nil, errors.New("列目录返回格式异常")
	}
	dirs := make([]string, 0, len(entries))
	for _, entry := range entries {
		item, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		isDir, _ := item["IsDir"].(bool)
		if !isDir {
			continue
		}
		if name, ok := item["Name"].(string); ok {
			dirs = append(dirs, name)
		}
	}
	sort.Strings(dirs)
	return dirs, nil
}

// transferStats 读 rclone 的进程内累计传输统计。
func transferStats() (bytes uint64, speed float64, err error) {
	value, err := rpc("core/stats", map[string]any{})
	if err != nil {
		return 0, 0, err
	}
	if raw, ok := value["bytes"].(float64); ok && raw > 0 {
		bytes = uint64(raw)
	}
	speed, _ = value["speed"].(float64)
	return bytes, speed, nil
}
