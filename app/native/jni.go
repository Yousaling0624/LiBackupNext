package main

/*
#include <jni.h>
#include <stdio.h>
#include <stdlib.h>

#cgo android LDFLAGS: -llog

#ifdef __ANDROID__
#include <android/log.h>
#endif

static void* libackup_buffer_address(JNIEnv* env, jobject buffer) {
	return (*env)->GetDirectBufferAddress(env, buffer);
}

static jlong libackup_buffer_capacity(JNIEnv* env, jobject buffer) {
	return (*env)->GetDirectBufferCapacity(env, buffer);
}

static const jchar* libackup_string_chars(JNIEnv* env, jstring value) {
	return (*env)->GetStringChars(env, value, NULL);
}

static jsize libackup_string_length(JNIEnv* env, jstring value) {
	return (*env)->GetStringLength(env, value);
}

static void libackup_release_string(JNIEnv* env, jstring value, const jchar* chars) {
	(*env)->ReleaseStringChars(env, value, chars);
}

// 真机走 liblog，出错信息进 logcat；桌面构建只需要能看到这句话。
static void libackup_log(const char* text) {
#ifdef __ANDROID__
	__android_log_print(6, "LiBackupGo", "%s", text);
#else
	fputs(text, stderr);
	fputc('\n', stderr);
#endif
}
*/
import "C"

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime/debug"
	"sync"
	"time"
	"unicode/utf16"
	"unsafe"

	"github.com/restic/restic/libackupnext/engine"
)

var (
	logDirMu sync.Mutex
	logDir   string
)

func setLogDir(dir string) {
	logDirMu.Lock()
	logDir = dir
	logDirMu.Unlock()
}

func nativeLog(text string) {
	cs := C.CString(text)
	defer C.free(unsafe.Pointer(cs))
	C.libackup_log(cs)
}

// appendPanicFile 在进程崩溃之前把信息落盘；logcat 被冲掉也还能查。
func appendPanicFile(text string) {
	logDirMu.Lock()
	dir := logDir
	logDirMu.Unlock()
	if dir == "" {
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	file := filepath.Join(dir, "rust-panic.txt")
	out, err := os.OpenFile(file, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer out.Close()
	_, _ = fmt.Fprintf(out, "===== %d =====\n%s\n", time.Now().UnixMilli(), text)
	_ = out.Sync()
}

// guard 是所有 JNI 入口的兜底：Go panic 不能穿过 cgo 边界。
func guard(ret *C.jint, where string) {
	r := recover()
	if r == nil {
		return
	}
	text := fmt.Sprintf(
		"[libackup-go] PANIC\ntime(ms): %d\nlocation: %s\npayload: %v\nstack:\n%s",
		time.Now().UnixMilli(), where, r, debug.Stack(),
	)
	nativeLog(text)
	appendPanicFile(text)
	*ret = 1
}

func jstringGo(env *C.JNIEnv, value C.jstring) string {
	length := C.libackup_string_length(env, value)
	chars := C.libackup_string_chars(env, value)
	if chars == nil {
		return ""
	}
	defer C.libackup_release_string(env, value, chars)

	units := unsafe.Slice((*uint16)(unsafe.Pointer(chars)), int(length))
	return string(utf16.Decode(units))
}

// getBuffer 取 Java DirectByteBuffer 的地址和容量。
// 第二个返回值非 0 时表示共享内存不可用，直接把它当 JNI 返回值。
func getBuffer(env *C.JNIEnv, buffer C.jobject, minimum int) (sharedBuffer, C.jint) {
	ptr := C.libackup_buffer_address(env, buffer)
	capacity := int(C.libackup_buffer_capacity(env, buffer))
	shared := sharedBuffer{ptr: ptr, capacity: capacity}
	if shared.ptr == nil || shared.capacity <= minimum {
		return shared, 2
	}
	return shared, 0
}

func failureOf(err error) *engine.Failure {
	if err == nil {
		return nil
	}
	var failure *engine.Failure
	if errors.As(err, &failure) {
		return failure
	}
	return engine.DescribeError(err)
}

func writeSimple(shared sharedBuffer, err error) C.jint {
	if err != nil {
		shared.writeResponse(engine.FailureResponse(failureOf(err)))
		return 1
	}
	shared.writeResponse(engine.OKResponse())
	return 0
}

func decodeRequest[T any](shared sharedBuffer) (*T, *engine.Failure) {
	data, err := shared.readRequest()
	if err != nil {
		return nil, &engine.Failure{Message: err.Error()}
	}
	var request T
	if err := json.Unmarshal(data, &request); err != nil {
		return nil, &engine.Failure{Message: err.Error()}
	}
	return &request, nil
}

// ==================== RusticServer ====================

//export Java_com_cc_libackupnext_service_root_RusticServer_nativeSetLogDir
func Java_com_cc_libackupnext_service_root_RusticServer_nativeSetLogDir(env *C.JNIEnv, _ C.jobject, path C.jstring) (ret C.jint) {
	defer guard(&ret, "nativeSetLogDir")
	setLogDir(jstringGo(env, path))
	return 0
}

//export Java_com_cc_libackupnext_service_root_RusticServer_nativeSetCacheDir
func Java_com_cc_libackupnext_service_root_RusticServer_nativeSetCacheDir(env *C.JNIEnv, _ C.jobject, path C.jstring) (ret C.jint) {
	defer guard(&ret, "nativeSetCacheDir")
	engine.SetCacheDir(jstringGo(env, path))
	return 0
}

//export Java_com_cc_libackupnext_service_root_RusticServer_nativeSetRepo
func Java_com_cc_libackupnext_service_root_RusticServer_nativeSetRepo(env *C.JNIEnv, _ C.jobject, path C.jstring, password C.jstring, options C.jstring) (ret C.jint) {
	defer guard(&ret, "nativeSetRepo")
	var opts map[string]string
	_ = json.Unmarshal([]byte(jstringGo(env, options)), &opts)
	engine.SetRepo(jstringGo(env, path), jstringGo(env, password), opts)
	// 这里刻意不停 rclone serve：凭据注入前 RusticSession 已经用 ensureServe
	// 取回了 rest_url，随即停掉会让刚注入的地址立刻失效。旧 serve 由
	// RcloneClient.stopServe / 下一笔 ensureServe 按 key 处理。
	return 0
}

//export Java_com_cc_libackupnext_service_root_RusticServer_nativeRepoLayout
func Java_com_cc_libackupnext_service_root_RusticServer_nativeRepoLayout(env *C.JNIEnv, _ C.jobject, buffer C.jobject) (ret C.jint) {
	defer guard(&ret, "nativeRepoLayout")
	shared, bad := getBuffer(env, buffer, 4)
	if bad != 0 {
		return bad
	}
	request, failure := decodeRequest[engine.RepoLayoutRequest](shared)
	if failure != nil {
		shared.writeResponse(engine.LayoutFailure(failure))
		return 1
	}
	layout, err := engine.RepoLayout(request.Path, request.Options)
	if err != nil {
		shared.writeResponse(engine.LayoutFailure(failureOf(err)))
		return 1
	}
	shared.writeResponse(engine.NewLayoutResponse(*layout))
	return 0
}

//export Java_com_cc_libackupnext_service_root_RusticServer_nativeInitRepo
func Java_com_cc_libackupnext_service_root_RusticServer_nativeInitRepo(env *C.JNIEnv, _ C.jobject, buffer C.jobject) (ret C.jint) {
	defer guard(&ret, "nativeInitRepo")
	shared, bad := getBuffer(env, buffer, 4)
	if bad != 0 {
		return bad
	}
	request, failure := decodeRequest[engine.InitRequest](shared)
	if failure != nil {
		shared.writeResponse(engine.FailureResponse(failure))
		return 1
	}
	return writeSimple(shared, engine.InitRepo(request))
}

//export Java_com_cc_libackupnext_service_root_RusticServer_nativeBackup
func Java_com_cc_libackupnext_service_root_RusticServer_nativeBackup(env *C.JNIEnv, _ C.jobject, buffer C.jobject) (ret C.jint) {
	defer guard(&ret, "nativeBackup")
	shared, bad := getBuffer(env, buffer, progressOffset+8)
	if bad != 0 {
		return bad
	}
	request, failure := decodeRequest[engine.BackupRequest](shared)
	if failure != nil {
		shared.writeResponse(engine.FailureResponse(failure))
		return 1
	}

	ring := newProgressRing(shared)
	progress := engine.NewProgress(ring.write)
	progress.SetStage("")
	engine.SetActiveProgress(progress)
	err := engine.Backup(context.Background(), request, progress)
	engine.ClearActiveProgress()

	progress.Finish()
	return writeSimple(shared, err)
}

//export Java_com_cc_libackupnext_service_root_RusticServer_nativeListSnapshots
func Java_com_cc_libackupnext_service_root_RusticServer_nativeListSnapshots(env *C.JNIEnv, _ C.jobject, buffer C.jobject) (ret C.jint) {
	defer guard(&ret, "nativeListSnapshots")
	shared, bad := getBuffer(env, buffer, 4)
	if bad != 0 {
		return bad
	}
	request, failure := decodeRequest[engine.ListRequest](shared)
	if failure != nil {
		shared.writeResponse(engine.ListFailure(failure))
		return 1
	}

	snapshots, err := engine.ListSnapshots(request.PackageName)
	if err != nil {
		shared.writeResponse(engine.ListFailure(failureOf(err)))
		return 1
	}

	// 按实际容量裁剪：描述可能很长，500 条不一定塞得下。
	for {
		data, err := json.Marshal(engine.NewListResponse(snapshots))
		if err != nil {
			shared.writeResponse(engine.ListFailure(&engine.Failure{Message: "序列化快照失败"}))
			return 0
		}
		if len(data)+4 <= shared.capacity {
			shared.writeFrameAt(0, data)
			return 0
		}
		if len(snapshots) == 0 {
			shared.writeResponse(engine.NewListResponse(nil))
			return 0
		}
		snapshots = snapshots[:len(snapshots)-1]
	}
}

//export Java_com_cc_libackupnext_service_root_RusticServer_nativeOverview
func Java_com_cc_libackupnext_service_root_RusticServer_nativeOverview(env *C.JNIEnv, _ C.jobject, buffer C.jobject) (ret C.jint) {
	defer guard(&ret, "nativeOverview")
	shared, bad := getBuffer(env, buffer, 4)
	if bad != 0 {
		return bad
	}
	packages, err := engine.Overview()
	if err != nil {
		shared.writeResponse(engine.OverviewFailure(failureOf(err)))
		return 1
	}
	shared.writeResponse(engine.NewOverviewResponse(packages))
	return 0
}

//export Java_com_cc_libackupnext_service_root_RusticServer_nativeCleanOrphanIcons
func Java_com_cc_libackupnext_service_root_RusticServer_nativeCleanOrphanIcons(env *C.JNIEnv, _ C.jobject, buffer C.jobject) (ret C.jint) {
	defer guard(&ret, "nativeCleanOrphanIcons")
	shared, bad := getBuffer(env, buffer, 4)
	if bad != 0 {
		return bad
	}
	removed, err := engine.CleanOrphanIcons()
	if err != nil {
		shared.writeResponse(engine.CleanupFailure(failureOf(err)))
		return 1
	}
	shared.writeResponse(engine.NewCleanupResponse(removed))
	return 0
}

//export Java_com_cc_libackupnext_service_root_RusticServer_nativeUpdateDescription
func Java_com_cc_libackupnext_service_root_RusticServer_nativeUpdateDescription(env *C.JNIEnv, _ C.jobject, buffer C.jobject) (ret C.jint) {
	defer guard(&ret, "nativeUpdateDescription")
	shared, bad := getBuffer(env, buffer, 4)
	if bad != 0 {
		return bad
	}
	request, failure := decodeRequest[engine.UpdateDescriptionRequest](shared)
	if failure != nil {
		shared.writeResponse(engine.FailureResponse(failure))
		return 1
	}
	return writeSimple(shared, engine.UpdateDescriptions(request.IDs, request.Description))
}

//export Java_com_cc_libackupnext_service_root_RusticServer_nativeDeleteSnapshots
func Java_com_cc_libackupnext_service_root_RusticServer_nativeDeleteSnapshots(env *C.JNIEnv, _ C.jobject, buffer C.jobject) (ret C.jint) {
	defer guard(&ret, "nativeDeleteSnapshots")
	shared, bad := getBuffer(env, buffer, 4)
	if bad != 0 {
		return bad
	}
	request, failure := decodeRequest[engine.DeleteRequest](shared)
	if failure != nil {
		shared.writeResponse(engine.FailureResponse(failure))
		return 1
	}
	return writeSimple(shared, engine.DeleteSnapshots(request.IDs))
}

//export Java_com_cc_libackupnext_service_root_RusticServer_nativeRestore
func Java_com_cc_libackupnext_service_root_RusticServer_nativeRestore(env *C.JNIEnv, _ C.jobject, buffer C.jobject) (ret C.jint) {
	defer guard(&ret, "nativeRestore")
	shared, bad := getBuffer(env, buffer, 4)
	if bad != 0 {
		return bad
	}
	request, failure := decodeRequest[engine.RestoreRequest](shared)
	if failure != nil {
		shared.writeResponse(engine.FailureResponse(failure))
		return 1
	}

	ring := newProgressRing(shared)
	progress := engine.NewProgress(ring.write)
	progress.SetStage("")
	engine.SetActiveProgress(progress)
	err := engine.Restore(context.Background(), request, progress)
	engine.ClearActiveProgress()

	// 恢复和 Rust 实现一样只写字节进度，不写 finished。
	return writeSimple(shared, err)
}

//export Java_com_cc_libackupnext_service_root_RusticServer_nativePrune
func Java_com_cc_libackupnext_service_root_RusticServer_nativePrune(env *C.JNIEnv, _ C.jobject, buffer C.jobject) (ret C.jint) {
	defer guard(&ret, "nativePrune")
	shared, bad := getBuffer(env, buffer, 4)
	if bad != 0 {
		return bad
	}
	return writeSimple(shared, engine.Prune())
}

//export Java_com_cc_libackupnext_service_root_RusticServer_nativePruneEstimate
func Java_com_cc_libackupnext_service_root_RusticServer_nativePruneEstimate(env *C.JNIEnv, _ C.jobject, buffer C.jobject) (ret C.jint) {
	defer guard(&ret, "nativePruneEstimate")
	shared, bad := getBuffer(env, buffer, 4)
	if bad != 0 {
		return bad
	}
	bytes, err := engine.PruneEstimate()
	if err != nil {
		shared.writeResponse(engine.EstimateFailure(failureOf(err)))
		return 1
	}
	shared.writeResponse(engine.NewEstimateResponse(bytes))
	return 0
}

// ==================== RcloneServer ====================

func writeRcloneError(shared sharedBuffer, err error) C.jint {
	shared.writeResponse(map[string]any{"ok": false, "error": err.Error()})
	return 1
}

//export Java_com_cc_libackupnext_service_root_RcloneServer_nativeListDirs
func Java_com_cc_libackupnext_service_root_RcloneServer_nativeListDirs(env *C.JNIEnv, _ C.jobject, buffer C.jobject) (ret C.jint) {
	defer guard(&ret, "nativeListDirs")
	shared, bad := getBuffer(env, buffer, 4)
	if bad != 0 {
		return bad
	}
	request, failure := decodeRequest[engine.ListDirsRequest](shared)
	if failure != nil {
		return writeRcloneError(shared, errors.New(failure.Message))
	}

	ensureRclone()
	fs, insecure, err := prepareFS(request.Fs)
	if err != nil {
		return writeRcloneError(shared, err)
	}
	dirs, err := listDirs(fs, request.Path, request.Create, insecure)
	if err != nil {
		return writeRcloneError(shared, err)
	}
	shared.writeResponse(map[string]any{"ok": true, "dirs": dirs})
	return 0
}

//export Java_com_cc_libackupnext_service_root_RcloneServer_nativeEnsureServe
func Java_com_cc_libackupnext_service_root_RcloneServer_nativeEnsureServe(env *C.JNIEnv, _ C.jobject, buffer C.jobject) (ret C.jint) {
	defer guard(&ret, "nativeEnsureServe")
	shared, bad := getBuffer(env, buffer, 4)
	if bad != 0 {
		return bad
	}
	request, failure := decodeRequest[engine.EnsureServeRequest](shared)
	if failure != nil {
		return writeRcloneError(shared, errors.New(failure.Message))
	}

	ensureRclone()
	fs, insecure, err := prepareFS(request.Fs)
	if err != nil {
		return writeRcloneError(shared, err)
	}
	url, err := remoteURL(fs, request.Key, insecure)
	if err != nil {
		return writeRcloneError(shared, err)
	}
	shared.writeResponse(map[string]any{"ok": true, "url": url})
	return 0
}

//export Java_com_cc_libackupnext_service_root_RcloneServer_nativeStopServe
func Java_com_cc_libackupnext_service_root_RcloneServer_nativeStopServe(env *C.JNIEnv, _ C.jobject, buffer C.jobject) (ret C.jint) {
	defer guard(&ret, "nativeStopServe")
	shared, bad := getBuffer(env, buffer, 4)
	if bad != 0 {
		return bad
	}
	stopServe()
	shared.writeResponse(map[string]any{"ok": true})
	return 0
}

//export Java_com_cc_libackupnext_service_root_RcloneServer_nativeTransferStats
func Java_com_cc_libackupnext_service_root_RcloneServer_nativeTransferStats(env *C.JNIEnv, _ C.jobject, buffer C.jobject) (ret C.jint) {
	defer guard(&ret, "nativeTransferStats")
	shared, bad := getBuffer(env, buffer, 4)
	if bad != 0 {
		return bad
	}
	bytes, speed, err := transferStats()
	if err != nil {
		return writeRcloneError(shared, err)
	}
	if math.IsNaN(speed) || math.IsInf(speed, 0) {
		speed = 0
	}
	shared.writeResponse(map[string]any{"ok": true, "bytes": bytes, "speed": speed})
	return 0
}

//export Java_com_cc_libackupnext_service_root_RcloneServer_nativeSetLogDir
func Java_com_cc_libackupnext_service_root_RcloneServer_nativeSetLogDir(env *C.JNIEnv, _ C.jobject, path C.jstring) (ret C.jint) {
	defer guard(&ret, "nativeSetLogDir")
	setLogDir(jstringGo(env, path))
	return 0
}
