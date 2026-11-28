package engine

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/restic/restic/internal/backend"
	"github.com/restic/restic/internal/backend/cache"
	"github.com/restic/restic/internal/backend/local"
	"github.com/restic/restic/internal/backend/rest"
	"github.com/restic/restic/internal/repository"
)

// RepoConfig 是 nativeSetRepo 注入的仓库凭据。代数每次注入递增，
// 三个仓库缓存拿它当 key：换仓库 / 改密码后即使缓存没清也不会错误复用。
type RepoConfig struct {
	Path       string
	Password   string
	Options    map[string]string
	Generation uint64
}

var (
	repoMu         sync.Mutex
	repoCfg        *RepoConfig
	repoGeneration atomic.Uint64

	cacheDirMu  sync.Mutex
	cacheDirVal string
)

func SetRepo(path, password string, options map[string]string) {
	gen := repoGeneration.Add(1)
	repoMu.Lock()
	if path == "" || password == "" {
		repoCfg = nil
	} else {
		repoCfg = &RepoConfig{Path: path, Password: password, Options: options, Generation: gen}
	}
	repoMu.Unlock()
	invalidateAll()
}

func repoConfig() (*RepoConfig, error) {
	repoMu.Lock()
	defer repoMu.Unlock()
	if repoCfg == nil {
		return nil, appFailure("仓库尚未配置，请先初始化仓库")
	}
	copy := *repoCfg
	return &copy, nil
}

func SetCacheDir(path string) {
	cacheDirMu.Lock()
	cacheDirVal = path
	cacheDirMu.Unlock()
}

func cacheDir() string {
	cacheDirMu.Lock()
	defer cacheDirMu.Unlock()
	return cacheDirVal
}

// repoKey 里不放密码：代数已经能区分每次注入。
func repoKey(cfg *RepoConfig) string {
	return cfg.Path + "\x00" + formatUint(cfg.Generation)
}

func formatUint(v uint64) string {
	// 避免为了一个十进制转换引入 fmt 的格式化开销
	if v == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}

// localRoot 返回本地仓库目录；远程逻辑标识返回空。
func localRoot(location string) (string, bool) {
	if strings.Contains(location, "://") ||
		strings.HasPrefix(location, "rest:") ||
		strings.HasPrefix(location, "opendal:") ||
		strings.HasPrefix(location, "remote:") ||
		strings.HasPrefix(location, "rclone:") {
		return "", false
	}
	return strings.TrimPrefix(location, "local:"), true
}

const (
	backendDialTimeout    = 5 * time.Second
	backendResponseHeader = 20 * time.Second
	backendIdleTimeout    = 20 * time.Second
)

// idleTimeoutConn 把每次 Read / Write 的空闲超时变成连接 deadline：
// 慢但持续的传输不会被总时长误杀，真正卡住才会断。
type idleTimeoutConn struct {
	net.Conn
}

func (c *idleTimeoutConn) Read(b []byte) (int, error) {
	_ = c.Conn.SetReadDeadline(time.Now().Add(backendIdleTimeout))
	return c.Conn.Read(b)
}

func (c *idleTimeoutConn) Write(b []byte) (int, error) {
	_ = c.Conn.SetWriteDeadline(time.Now().Add(backendIdleTimeout))
	return c.Conn.Write(b)
}

func restTransport() *http.Transport {
	dialer := &net.Dialer{Timeout: backendDialTimeout}
	transport := &http.Transport{
		Proxy:                 nil,
		DialContext:           dialer.DialContext,
		ResponseHeaderTimeout: backendResponseHeader,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConns:          20,
	}
	transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		conn, err := dialer.DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		return &idleTimeoutConn{Conn: conn}, nil
	}
	return transport
}

// makeBackend 把项目里的仓库地址翻译成 restic backend。
// 远程只支持 rest，由主进程起的 rclone serve restic 提供 rest_url。
func makeBackend(ctx context.Context, location string, options map[string]string, create bool) (backend.Backend, bool, error) {
	if strings.HasPrefix(location, "remote:") {
		restURL := options["rest_url"]
		if restURL == "" {
			return nil, true, appFailure("缺少远程服务地址")
		}
		be, err := openRestBackend(ctx, restURL, create)
		if err != nil {
			return nil, true, wrapFailure("", err)
		}
		return &errorReportingBackend{Backend: be}, true, nil
	}

	if strings.HasPrefix(location, "rest:") {
		be, err := openRestBackend(ctx, strings.TrimPrefix(location, "rest:"), create)
		if err != nil {
			return nil, true, wrapFailure("", err)
		}
		return &errorReportingBackend{Backend: be}, true, nil
	}

	if root, ok := localRoot(location); ok {
		if create {
			be, err := local.Create(ctx, local.Config{Path: root, Connections: 2})
			if err != nil {
				return nil, false, wrapFailure("", err)
			}
			return be, false, nil
		}
		be, err := local.Open(ctx, local.Config{Path: root, Connections: 2})
		if err != nil {
			return nil, false, wrapFailure("", err)
		}
		return be, false, nil
	}

	return nil, false, appFailuref("不支持的后端：%s", location)
}

func openRestBackend(ctx context.Context, raw string, create bool) (backend.Backend, error) {
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	if u.Path == "" {
		u.Path = "/"
	}
	cfg := rest.NewConfig()
	cfg.URL = u
	transport := restTransport()
	if create {
		return rest.Create(ctx, cfg, transport)
	}
	return rest.Open(ctx, cfg, transport)
}

// errorReportingBackend 在传输层错误出现时先写 interrupted 帧，再短路后续操作，
// 让 archiver / restore 尽快收尾，不用等源字节读满。
type errorReportingBackend struct {
	backend.Backend
	cancel atomic.Bool
}

func (b *errorReportingBackend) cancelled() bool {
	return b.cancel.Load()
}

func (b *errorReportingBackend) observe(err error) error {
	if err == nil {
		return nil
	}
	f := DescribeError(err)
	if f.Reason.isTransport() {
		interruptActive(f.Reason)
		b.cancel.Store(true)
	}
	return err
}

func (b *errorReportingBackend) Save(ctx context.Context, h backend.Handle, rd backend.RewindReader) error {
	if b.cancelled() {
		return errCancelled
	}
	return b.observe(b.Backend.Save(ctx, h, rd))
}

func (b *errorReportingBackend) Load(ctx context.Context, h backend.Handle, length int, offset int64, fn func(rd io.Reader) error) error {
	if b.cancelled() {
		return errCancelled
	}
	return b.observe(b.Backend.Load(ctx, h, length, offset, fn))
}

func (b *errorReportingBackend) Stat(ctx context.Context, h backend.Handle) (backend.FileInfo, error) {
	if b.cancelled() {
		return backend.FileInfo{}, errCancelled
	}
	fi, err := b.Backend.Stat(ctx, h)
	return fi, b.observe(err)
}

func (b *errorReportingBackend) List(ctx context.Context, t backend.FileType, fn func(backend.FileInfo) error) error {
	if b.cancelled() {
		return errCancelled
	}
	return b.observe(b.Backend.List(ctx, t, fn))
}

func (b *errorReportingBackend) Remove(ctx context.Context, h backend.Handle) error {
	if b.cancelled() {
		return errCancelled
	}
	return b.observe(b.Backend.Remove(ctx, h))
}

func (b *errorReportingBackend) Delete(ctx context.Context) error {
	if b.cancelled() {
		return errCancelled
	}
	return b.observe(b.Backend.Delete(ctx))
}

// openRepository 打开仓库并派生密钥。indexed 为 true 时顺带把索引读进内存。
func openRepository(ctx context.Context, cfg *RepoConfig, indexed bool) (*repository.Repository, error) {
	be, _, err := makeBackend(ctx, cfg.Path, cfg.Options, false)
	if err != nil {
		return nil, err
	}

	repo, err := repository.New(be, repository.Options{})
	if err != nil {
		return nil, wrapFailure("", err)
	}
	if err := repo.SearchKey(ctx, cfg.Password, 0, ""); err != nil {
		_ = repo.Close()
		return nil, wrapFailure("", err)
	}

	if dir := cacheDir(); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err == nil {
			if c, err := cache.New(repo.Config().ID, dir); err == nil {
				repo.UseCache(c)
			}
		}
	}

	if indexed {
		if err := repo.LoadIndex(ctx, nil); err != nil {
			_ = repo.Close()
			return nil, wrapFailure("", err)
		}
	}
	return repo, nil
}

// repoLease 给仓库缓存加引用计数。缓存失效只是「退休」：还在用的调用
// 各自持有一份引用，等最后一个 release 才真正 Close。这对应 Rust 版的
// Arc<Repository> —— 否则并发查询可能拿到刚被备份收尾关掉的 backend。
type repoLease struct {
	*repository.Repository
	refs      atomic.Int32
	retired   atomic.Bool
	closeOnce sync.Once
}

func (l *repoLease) acquire() *repoLease {
	l.refs.Add(1)
	return l
}

func (l *repoLease) release() {
	if l.refs.Add(-1) == 0 && l.retired.Load() {
		l.closeOnce.Do(func() { _ = l.Repository.Close() })
	}
}

func (l *repoLease) retire() {
	l.retired.Store(true)
	if l.refs.Load() == 0 {
		l.closeOnce.Do(func() { _ = l.Repository.Close() })
	}
}

type cachedRepository struct {
	key      string
	repo     *repoLease
	lastUsed time.Time
}

var (
	openCacheMu sync.Mutex
	openCache   *cachedRepository

	indexedCacheMu sync.Mutex
	indexedCache   *cachedRepository

	backupCacheMu sync.Mutex
	backupCache   *cachedRepository
)

const repoCacheIdle = 10 * time.Minute

func openCached(ctx context.Context, indexed bool) (*repoLease, error) {
	cfg, err := repoConfig()
	if err != nil {
		return nil, err
	}
	key := repoKey(cfg)

	cacheMu := &openCacheMu
	var slot **cachedRepository
	if indexed {
		cacheMu = &indexedCacheMu
		slot = &indexedCache
	} else {
		slot = &openCache
	}

	cacheMu.Lock()
	if cached := *slot; cached != nil {
		if cached.key == key && time.Since(cached.lastUsed) < repoCacheIdle {
			cached.lastUsed = time.Now()
			lease := cached.repo.acquire()
			cacheMu.Unlock()
			return lease, nil
		}
		cached.repo.retire()
		*slot = nil
	}
	cacheMu.Unlock()

	// 打开仓库可能要几秒（远程仓库还要 scrypt + 联网），不能一直占着缓存锁：
	// 旧实现也是先放开锁再打开，换仓库的 setRepo 不会被慢查询卡住。
	repo, err := openRepository(ctx, cfg, indexed)
	if err != nil {
		return nil, err
	}

	cacheMu.Lock()
	if cached := *slot; cached != nil {
		cached.repo.retire()
	}
	lease := &repoLease{Repository: repo}
	*slot = &cachedRepository{key: key, repo: lease, lastUsed: time.Now()}
	// 必须在锁内拿引用：否则 invalidate 可能先退休这个零引用句柄。
	lease.acquire()
	cacheMu.Unlock()
	return lease, nil
}

func openRepo() (*repoLease, error) {
	return openCached(context.Background(), false)
}

func openIndexedRepo() (*repoLease, error) {
	return openCached(context.Background(), true)
}

// openBackupRepo 按批次缓存已建索引的仓库。同一批备份里打开仓库（scrypt
// 派生 + 读索引）只付一次；没有批次标识就用一次性实例，不跨调用缓存。
//
// 不能复用 indexedCache：备份写进去的新 pack 只有这个实例的内存索引知道，
// 而列表查询用的缓存每轮都会失效，两者语义不同。
func openBackupRepo(batch *string) (*repoLease, error) {
	cfg, err := repoConfig()
	if err != nil {
		return nil, err
	}

	name := ""
	if batch != nil {
		name = strings.TrimSpace(*batch)
	}
	if name == "" {
		repo, err := openRepository(context.Background(), cfg, true)
		if err != nil {
			return nil, err
		}
		// 一次性句柄：没有缓存持有者，release 时直接关闭。
		lease := (&repoLease{Repository: repo}).acquire()
		lease.retire()
		return lease, nil
	}

	key := repoKey(cfg) + "\x00batch\x00" + name

	backupCacheMu.Lock()
	if cached := backupCache; cached != nil {
		if cached.key == key && time.Since(cached.lastUsed) < repoCacheIdle {
			cached.lastUsed = time.Now()
			lease := cached.repo.acquire()
			backupCacheMu.Unlock()
			return lease, nil
		}
		cached.repo.retire()
		backupCache = nil
	}
	backupCacheMu.Unlock()

	repo, err := openRepository(context.Background(), cfg, true)
	if err != nil {
		return nil, err
	}

	backupCacheMu.Lock()
	if cached := backupCache; cached != nil {
		cached.repo.retire()
	}
	lease := &repoLease{Repository: repo}
	backupCache = &cachedRepository{key: key, repo: lease, lastUsed: time.Now()}
	lease.acquire()
	backupCacheMu.Unlock()
	return lease, nil
}

func invalidateAll() {
	openCacheMu.Lock()
	if openCache != nil {
		openCache.repo.retire()
		openCache = nil
	}
	openCacheMu.Unlock()

	indexedCacheMu.Lock()
	if indexedCache != nil {
		indexedCache.repo.retire()
		indexedCache = nil
	}
	indexedCacheMu.Unlock()

	backupCacheMu.Lock()
	if backupCache != nil {
		backupCache.repo.retire()
		backupCache = nil
	}
	backupCacheMu.Unlock()
}

// invalidateIndexed 只在仓库内容变化后调用：备份 / 删除快照 / 整理仓库。
func invalidateIndexed() {
	indexedCacheMu.Lock()
	if indexedCache != nil {
		indexedCache.repo.retire()
		indexedCache = nil
	}
	indexedCacheMu.Unlock()
}

// invalidateBackup 丢掉批次缓存：整理仓库或这批备份失败后索引可能已不可信。
func invalidateBackup() {
	backupCacheMu.Lock()
	if backupCache != nil {
		backupCache.repo.retire()
		backupCache = nil
	}
	backupCacheMu.Unlock()
}

// closeEverything 给进程退出用（JNI 没有显式关闭入口，留作测试清理）。
func closeEverything() { invalidateAll() }
