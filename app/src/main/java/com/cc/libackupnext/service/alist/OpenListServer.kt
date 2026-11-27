package com.cc.libackupnext.service.alist

import android.content.Context
import com.cc.libackupnext.data.RemoteRepo
import com.cc.libackupnext.data.prefs.AlistPrefs
import com.cc.libackupnext.data.protocol.FIELD_HOST
import com.cc.libackupnext.data.protocol.WebDavProtocol
import com.cc.libackupnext.service.gateway.AlistGateway
import com.cc.libackupnext.service.gateway.AlistInstallState
import com.cc.libackupnext.service.gateway.AlistState
import com.cc.libackupnext.util.AppLog
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.Job
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.delay
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.launch
import kotlinx.coroutines.sync.Mutex
import kotlinx.coroutines.sync.withLock
import kotlinx.coroutines.withContext
import java.io.File
import java.io.FileOutputStream
import java.net.HttpURLConnection
import java.net.URL
import java.util.concurrent.TimeUnit

/**
 * [AlistGateway] 的默认实现：把按需下载的 OpenList 可执行文件拉起来，管它的生命周期。
 *
 * 进程本身跑在**应用进程的普通 uid** 下，不是 root：
 * AList 只负责把网盘翻译成 WebDAV，不需要系统权限；需要 root 的备份 / 恢复仍然在
 * `:root` 服务里，两边通过 127.0.0.1 通信。
 *
 * 数据（sqlite、config、日志）放在 [Context.getNoBackupFilesDir]：
 * 里面会有网盘 token，不能进 Android 自动备份 / 换机迁移。
 */
internal class OpenListServer(
    private val context: Context,
    private val prefs: AlistPrefs,
    private val installer: OpenListInstaller
) : AlistGateway {

    private companion object {
        const val TAG = "Alist"

        /** 固定端口：WebDAV 仓库配置里存的就是它，换了会让旧配置失效。 */
        const val PORT = 5244

        /** 首次启动要建库、初始化默认项，慢设备留足余量。 */
        const val READY_TIMEOUT_MS = 45_000L

        /** 单个日志文件上限；超了就不再多写，避免长期运行把存储吃掉。 */
        const val LOG_MAX_BYTES = 4L * 1024 * 1024

        /** 内存里保留的末尾行数，用来给启动失败拼原因。 */
        const val TAIL_LINES = 80
    }

    private val scope = CoroutineScope(SupervisorJob() + Dispatchers.IO)
    private val mutex = Mutex()

    private val _state = MutableStateFlow<AlistState>(AlistState.Stopped)
    override val state: StateFlow<AlistState> = _state.asStateFlow()

    override val installState: StateFlow<AlistInstallState> get() = installer.state

    /** 由开关触发的启动协程；关掉开关时要能取消还在下载内核的那一条。 */
    private var startJob: Job? = null

    private var process: Process? = null
    private var logSink: LogSink? = null
    private val logTail = ArrayDeque<String>()
    private val passwordLock = Any()

    /** 正在被 stop() 叫停：awaitReady 看到就提前收工，不用等满 45 秒。 */
    @Volatile
    private var stopRequested = false

    override val port: Int = PORT

    override val enabled: Boolean get() = prefs.enabled

    override val password: String
        get() = synchronized(passwordLock) {
            prefs.password.ifBlank {
                OpenListConfig.randomSecret().also { prefs.password = it }
            }
        }

    override val webUiUrl: String get() = "http://${OpenListConfig.ADDRESS}:$PORT/"

    override val webDavUrl: String get() = "http://${OpenListConfig.ADDRESS}:$PORT/dav"

    override val webDavUser: String get() = "admin"

    private val dataDir: File
        get() = File(context.noBackupFilesDir, "openlist")

    override fun setEnabled(enabled: Boolean) {
        prefs.enabled = enabled
        if (enabled) {
            launchManagedStart()
        } else {
            // 正在下载 / 解压内核也要能立刻停下，不能等下载完
            startJob?.cancel()
            startJob = null
            scope.launch { stop() }
        }
    }

    override fun startIfEnabled() {
        if (prefs.enabled) launchManagedStart()
    }

    /**
     * 按「用户开关」启动：start / stop 是两条独立协程，快速开关时 stop 可能先执行完，
     * 再轮到这条 start。排队拿到锁后再看一次用户意图，已经关了就不启动。
     */
    private fun launchManagedStart() {
        // 已经在启动 / 下载了就不重复发起（重复下载 60MB 没有意义）
        if (startJob?.isActive == true) return
        startJob = scope.launch { start(requireEnabled = true) }
    }

    override suspend fun ensureStarted(): Result<Unit> = start()

    override suspend fun ensureInstalled(): Result<Unit> {
        val result = installer.ensureInstalled().map { }
        // 用户本来就开着服务（开关 = 用户意图）：下载完顺手拉起来，不用再拨一次开关
        if (result.isSuccess && prefs.enabled) launchManagedStart()
        return result
    }

    override suspend fun addQuarkStorage(cookie: String): Result<String> {
        ensureStarted().getOrElse { return Result.failure(it) }
        return OpenListAdminApi(webUiUrl, password).upsertQuarkStorage(cookie)
    }

    /**
     * 启动（幂等）：已在运行直接成功，否则等 /ping 就绪后返回。
     *
     * @param requireEnabled 只有用户开关触发的启动才传 true：排队期间用户可能已经关掉，
     * 这时不该把进程拉起来；页面上的「打开管理页面」是显式操作，不受开关限制。
     */
    suspend fun start(requireEnabled: Boolean = false): Result<Unit> = withContext(Dispatchers.IO) {
        if (requireEnabled && !prefs.enabled) {
            return@withContext Result.success(Unit)
        }

        // 已有一个在跑：可能还停在 Starting（上一个 start 还没走完），直接按运行中处理
        process?.takeIf { it.isAlive }?.let {
            if (_state.value !is AlistState.Running) _state.value = AlistState.Running(PORT)
            return@withContext Result.success(Unit)
        }

        // 下载 / 校验内核可能要几分钟，不能占着 mutex：用户点关闭时要能立刻取消
        val installed = installer.ensureInstalled()
        if (installed.isFailure) {
            val t = installed.exceptionOrNull() ?: IllegalStateException("AList 内核不可用")
            if (t is CancellationException) throw t
            _state.value = AlistState.Failed(t.message ?: "AList 内核不可用")
            return@withContext Result.failure(t)
        }
        val binary = installed.getOrThrow()
        if (requireEnabled && !prefs.enabled) {
            return@withContext Result.success(Unit)
        }

        mutex.withLock {
            if (requireEnabled && !prefs.enabled) {
                return@withLock Result.success(Unit)
            }

            process?.takeIf { it.isAlive }?.let {
                // 下载内核期间可能已经有另一条 start 把它拉起来了
                if (_state.value !is AlistState.Running) _state.value = AlistState.Running(PORT)
                return@withLock Result.success(Unit)
            }

            // 走到这里说明没有活着的进程。上一个进程刚死、watchExit 还没拿到锁时，
            // 它的日志句柄还挂在字段上，这里先收掉，避免反复启停泄漏文件描述符。
            process = null
            logSink?.close()
            logSink = null

            if (!binary.isFile) {
                val message = "AList 内核文件不存在，请重新下载"
                _state.value = AlistState.Failed(message)
                return@withLock Result.failure(IllegalStateException(message))
            }

            stopRequested = false
            synchronized(logTail) { logTail.clear() }
            _state.value = AlistState.Starting

            try {
                val dir = dataDir
                if (!dir.isDirectory && !dir.mkdirs()) {
                    error("无法创建 AList 数据目录")
                }
                writeConfig(dir)

                val sink = LogSink(File(dir, "openlist.log")) { line ->
                    synchronized(logTail) {
                        logTail.addLast(line)
                        while (logTail.size > TAIL_LINES) logTail.removeFirst()
                    }
                }
                logSink = sink

                val started = ProcessBuilder(
                    binary.absolutePath,
                    "server",
                    "--data", dir.absolutePath,
                    "--log-std"
                )
                    .directory(dir)
                    .redirectErrorStream(true)
                    .apply {
                        // 第一次启动 OpenList 不会自己建管理员（v4 改为网页安装向导），
                        // 这里用环境变量预置成我们生成的密码，页面就能直接登录
                        environment()["OPENLIST_ADMIN_PASSWORD"] = password
                        // 应用进程写不了 /tmp：HOME / TMPDIR 都指到自己的数据目录
                        environment()["HOME"] = dir.absolutePath
                        environment()["TMPDIR"] =
                            File(dir, "temp").apply { if (!isDirectory) mkdirs() }.absolutePath
                    }
                    .start()
                process = started
                drainLogs(started, sink)

                if (!awaitReady(started)) {
                    val detail = OpenListConfig.classify(snapshotTail(), PORT)
                        ?: exitDetail(started)
                        ?: "AList 未能在 ${READY_TIMEOUT_MS / 1000} 秒内启动"
                    throw IllegalStateException(detail)
                }

                watchExit(started, sink)
                _state.value = AlistState.Running(PORT)
                AppLog.i(TAG, "内置 AList 已启动：$webUiUrl")
                Result.success(Unit)
            } catch (t: Throwable) {
                val detail = t.message.orEmpty().ifBlank { "AList 启动失败" }
                destroyLocked()
                // 用户在启动途中点关闭：以「已停止」为准，别停在失败态
                _state.value = if (stopRequested) AlistState.Stopped else AlistState.Failed(detail)
                if (!stopRequested) AppLog.e(TAG, "启动内置 AList 失败：$detail", t)
                // 页面离开导致的协程取消：清理已经做了，但必须继续往上抛，
                // 不能吞掉 CancellationException
                if (t is CancellationException) throw t
                Result.failure(t)
            }
        }
    }

    override suspend fun stop() = withContext(Dispatchers.IO) {
        // 内核还在下载 / 解压时也要能停：取消驱动它的 startJob（installer 会清理临时文件）
        startJob?.cancel()
        startJob = null
        // 先置位再排队：start 持有 mutex 等 45 秒就绪时，这里不用干等
        stopRequested = true
        mutex.withLock {
            // 排队期间用户又打开了：这次的「关」已经被新意图覆盖，别把进程杀掉
            if (prefs.enabled) {
                stopRequested = false
                return@withLock
            }
            destroyLocked()
            _state.value = AlistState.Stopped
        }
        stopRequested = false
    }

    override fun webDavRepo(name: String): RemoteRepo = RemoteRepo(
        name = name,
        protocolKey = WebDavProtocol.key,
        values = mapOf(
            FIELD_HOST to webDavUrl,
            WebDavProtocol.FIELD_USER to webDavUser,
            WebDavProtocol.FIELD_PASS to password
        )
    )

    /** config.json 每次启动都重写：地址 / 端口是我们必须掌控的，其它字段原样保留。 */
    private fun writeConfig(dir: File) {
        val config = File(dir, "config.json")
        val existing = if (config.isFile) config.readText() else null
        config.writeText(
            OpenListConfig.prepare(
                existing = existing,
                port = PORT,
                jwtSecret = OpenListConfig.randomSecret(24)
            )
        )
    }

    /** 启动日志转存到有界文件；stdout / stderr 已经合并，一个线程就够。 */
    private fun drainLogs(process: Process, sink: LogSink) {
        Thread({
            runCatching {
                process.inputStream.bufferedReader(Charsets.UTF_8).useLines { lines ->
                    lines.forEach { sink.write(it) }
                }
            }
        }, "openlist-log").apply {
            isDaemon = true
            start()
        }
    }

    /** 轮询 /ping，直到就绪 / 进程退出 / 日志出现致命错误 / 超时。 */
    private suspend fun awaitReady(process: Process): Boolean {
        val deadline = System.currentTimeMillis() + READY_TIMEOUT_MS
        while (System.currentTimeMillis() < deadline) {
            if (stopRequested) return false
            if (!process.isAlive) {
                // 进程没了：给日志线程留一拍，把管道里最后几行读出来再判定原因
                delay(100)
                return false
            }
            if (OpenListConfig.classify(snapshotTail(), PORT) != null) return false
            if (ping()) return true
            delay(250)
        }
        return false
    }

    /** 进程已经退出时给一句带退出码的原因；还活着或拿不到退出码就返回 null。 */
    private fun exitDetail(process: Process): String? {
        if (process.isAlive) return null
        val code = runCatching { process.exitValue() }.getOrNull() ?: return null
        return "AList 启动后立即退出（代码 $code）"
    }

    /**
     * 端口冲突这类错误 OpenList 只报日志、进程本身会继续挂着等信号，
     * 所以除了日志判定，一定要在就绪后盯住进程退出。
     */
    private fun watchExit(target: Process, sink: LogSink) {
        scope.launch {
            val code = runCatching { target.waitFor() }.getOrDefault(-1)
            mutex.withLock {
                // 已经被 stop() / 重启用新进程替换：退出事件不是这次要处理的。
                // 只能关这次启动的 sink，不能碰 logSink 字段 —— 它可能已经指向新进程
                if (process !== target) return@withLock
                process = null
                logSink = null
                sink.close()
                val detail = OpenListConfig.classify(snapshotTail(), PORT)
                    ?: "AList 服务已退出（代码 $code）"
                _state.value = AlistState.Failed(detail)
                AppLog.w(TAG, "内置 AList 意外退出：$detail")
            }
        }
    }

    private fun destroyLocked() {
        val target = process ?: run {
            logSink?.close()
            logSink = null
            return
        }
        process = null
        if (target.isAlive) {
            target.destroy()
            if (!target.waitFor(3, TimeUnit.SECONDS)) target.destroyForcibly()
        }
        logSink?.close()
        logSink = null
    }

    private fun ping(): Boolean = runCatching {
        val connection = URL("${webUiUrl}ping").openConnection() as HttpURLConnection
        connection.connectTimeout = 1_000
        connection.readTimeout = 1_000
        connection.requestMethod = "GET"
        try {
            if (connection.responseCode != HttpURLConnection.HTTP_OK) return@runCatching false
            // 必须是 AList 自己的 /ping 应答；端口被别的程序占着时通常不会返回 pong
            val body = connection.inputStream.bufferedReader(Charsets.UTF_8).use { it.readText() }
            body.trim().equals("pong", ignoreCase = true)
        } finally {
            connection.disconnect()
        }
    }.getOrDefault(false)

    private fun snapshotTail(): String = synchronized(logTail) {
        logTail.joinToString("\n")
    }

    /**
     * 有界日志文件：只保留最近一次运行的输出，超过上限就不再写。
     *
     * 写入发生在日志线程，关闭发生在 start/stop 线程，方法都做了同步。
     */
    private class LogSink(
        file: File,
        private val onLine: (String) -> Unit
    ) {
        private val out: FileOutputStream
        private var written: Long
        private var closed = false

        init {
            if (file.length() > LOG_MAX_BYTES) file.delete()
            out = FileOutputStream(file, true)
            written = file.length()
        }

        fun write(raw: String) {
            val line = OpenListConfig.redact(raw)
            onLine(line)
            synchronized(this) {
                if (closed || written >= LOG_MAX_BYTES) return
                val bytes = (line + "\n").toByteArray(Charsets.UTF_8)
                try {
                    out.write(bytes)
                    out.flush()
                    written += bytes.size
                } catch (_: java.io.IOException) {
                    // 磁盘满等：日志写不下去也不能让日志线程退出，
                    // 否则 stdout 管道会堵死，AList 进程跟着卡住
                    closed = true
                    runCatching { out.close() }
                }
            }
        }

        @Synchronized
        fun close() {
            if (closed) return
            closed = true
            runCatching { out.close() }
        }
    }
}
