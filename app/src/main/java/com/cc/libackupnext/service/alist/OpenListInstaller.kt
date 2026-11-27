package com.cc.libackupnext.service.alist

import android.content.Context
import android.os.Build
import com.cc.libackupnext.BuildConfig
import com.cc.libackupnext.service.gateway.AlistInstallState
import com.cc.libackupnext.service.root.RootManager
import com.cc.libackupnext.util.AppLog
import com.topjohnwu.superuser.Shell
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.currentCoroutineContext
import kotlinx.coroutines.ensureActive
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.sync.Mutex
import kotlinx.coroutines.sync.withLock
import kotlinx.coroutines.withContext
import java.io.BufferedInputStream
import java.io.BufferedOutputStream
import java.io.ByteArrayOutputStream
import java.io.File
import java.io.FileInputStream
import java.io.FileOutputStream
import java.io.IOException
import java.io.InputStream
import java.io.OutputStream
import java.net.HttpURLConnection
import java.net.URI
import java.security.MessageDigest
import java.util.zip.GZIPInputStream

/**
 * 按需下载并安装 OpenList 内核。
 *
 * APK 里不再打包 AList 二进制：第一次启用时从上游 release 下载 tar.gz
 * （arm64 约 55MB，解压后约 165MB），校验 MD5 后解出可执行文件。
 *
 * Android 10+ 的 W^X 限制不允许 app 在可写目录里 execve；但 `apk_data_file`
 * 类型（APK 的 nativeLibraryDir 用的标签）是可执行的。所以下载完成后用 root
 * 执行一次 `chcon u:object_r:apk_data_file:s0`，进程仍然以应用 uid 运行，
 * 不需要把整个 AList 提权。
 *
 * 标签在应用更新 / 某些 ROM 的 restorecon 后可能被重置，所以**每次启动前都会重打**。
 */
internal class OpenListInstaller(private val context: Context) {

    private companion object {
        const val TAG = "AlistInstall"

        /** 安装目录下的文件名。 */
        const val DOWNLOAD_NAME = "openlist.tar.gz"
        const val BINARY_NAME = "libopenlist.so"
        const val VERSION_FILE = "kernel.version"

        /** tar.gz + 解压后的二进制（Go 静态链接，体积不小），留足余量。 */
        const val MIN_FREE_BYTES = 300L * 1024 * 1024
    }

    /**
     * 当前 ABI 对应的上游资产。
     *
     * goArch 是 OpenList release 里的架构名；md5 来自同版本的 md5-android.txt；
     * bytes 是 release 资产大小，用来在 Content-Length 缺失时也能显示进度。
     */
    private data class Asset(val goArch: String, val md5: String, val bytes: Long)

    private fun assetForCurrentDevice(): Asset? = when {
        Build.SUPPORTED_ABIS.contains("arm64-v8a") ->
            Asset("arm64", "12986ca60a8b561ea26b3f53cac738ed", 57_910_896L)

        Build.SUPPORTED_ABIS.contains("x86_64") ->
            Asset("amd64", "1e50e78407d8d2789f3aae3f93bb99d9", 60_544_414L)

        else -> null
    }

    private val root: File get() = File(context.noBackupFilesDir, "openlist")

    /** 最终的可执行文件；OpenListServer 直接按这个路径启动。 */
    val binary: File get() = File(root, "bin/$BINARY_NAME")

    private val versionFile: File get() = File(root, VERSION_FILE)
    private val downloadFile: File get() = File(root, DOWNLOAD_NAME)
    private val stagedBinary: File get() = File(binary.absolutePath + ".part")

    private val _state = MutableStateFlow<AlistInstallState>(
        if (isInstalled()) {
            AlistInstallState.Ready(BuildConfig.OPENLIST_VERSION)
        } else {
            AlistInstallState.Missing
        }
    )
    val state: StateFlow<AlistInstallState> = _state.asStateFlow()

    /** 同一时间只有一条下载 / 打标签的协程。 */
    private val mutex = Mutex()

    fun isInstalled(): Boolean {
        if (!binary.isFile) return false
        val installed = runCatching { versionFile.readText().trim() }.getOrNull()
        return installed == BuildConfig.OPENLIST_VERSION
    }

    /**
     * 确保内核可执行。
     *
     * 已安装时只重新打 SELinux 标签（很便宜）；否则先下载、校验、解压。
     */
    suspend fun ensureInstalled(): Result<File> = withContext(Dispatchers.IO) {
        mutex.withLock {
            try {
                if (!isInstalled()) install()
                applyExecLabel(binary)
                _state.value = AlistInstallState.Ready(BuildConfig.OPENLIST_VERSION)
                Result.success(binary)
            } catch (e: CancellationException) {
                downloadFile.delete()
                stagedBinary.delete()
                _state.value = if (isInstalled()) {
                    AlistInstallState.Ready(BuildConfig.OPENLIST_VERSION)
                } else {
                    AlistInstallState.Missing
                }
                throw e
            } catch (t: Throwable) {
                downloadFile.delete()
                stagedBinary.delete()
                val message = t.message.orEmpty().ifBlank { "下载 AList 内核失败" }
                _state.value = AlistInstallState.Failed(message)
                AppLog.e(TAG, message, t)
                Result.failure(IllegalStateException(message, t))
            }
        }
    }

    private suspend fun install() {
        val asset = assetForCurrentDevice()
            ?: error("当前 CPU 架构不受支持（内置 AList 只提供 arm64-v8a / x86_64）")

        val dir = root
        if (!dir.isDirectory && !dir.mkdirs()) error("无法创建 AList 数据目录")
        val free = dir.usableSpace
        if (free in 1 until MIN_FREE_BYTES) {
            error("存储空间不足：安装 AList 内核至少需要 300MB，当前可用 ${free / 1024 / 1024}MB")
        }

        val url = "https://github.com/OpenListTeam/OpenList/releases/download/" +
                "v${BuildConfig.OPENLIST_VERSION}/openlist-android-${asset.goArch}.tar.gz"
        _state.value = AlistInstallState.Downloading(0, asset.bytes)
        val actualMd5 = download(url, asset.bytes)
        if (!actualMd5.equals(asset.md5, ignoreCase = true)) {
            downloadFile.delete()
            error("内核校验失败（MD5 不匹配），请重试")
        }

        _state.value = AlistInstallState.Extracting
        binary.parentFile?.mkdirs()
        try {
            GZIPInputStream(BufferedInputStream(FileInputStream(downloadFile))).use { gzip ->
                if (!extract(gzip, "openlist", stagedBinary)) {
                    error("压缩包里找不到 openlist 可执行文件")
                }
            }
            if (!stagedBinary.setExecutable(true, true) || !stagedBinary.setReadable(true, true)) {
                // 个别文件系统不认 Java 的 setExecutable，用 chmod 兜底
                Shell.cmd("chmod 700 ${shellQuote(stagedBinary.absolutePath)}").exec()
            }
            if (binary.exists() && !binary.delete()) error("无法覆盖旧的内核文件")
            if (!stagedBinary.renameTo(binary)) error("无法写入内核文件")
        } finally {
            stagedBinary.delete()
            downloadFile.delete()
        }

        // 先写版本标记：即使后面的 chcon 失败，下次也只重试打标签，不重复下载
        versionFile.writeText(BuildConfig.OPENLIST_VERSION)
        AppLog.i(TAG, "OpenList 内核已下载：${binary.length() / 1024 / 1024}MB")
    }

    /**
     * 把文件标成 `apk_data_file`，应用 uid 才能 execve。
     *
     * AList 进程本身仍以应用 uid 运行，root 只用来改这一个文件的 SELinux 标签。
     */
    private fun applyExecLabel(target: File) {
        if (!target.isFile) error("AList 内核文件不存在，请重新下载")

        if (!RootManager.request()) {
            error("需要 root 权限才能运行内置 AList（要给下载的内核设置可执行标签）")
        }

        val quoted = shellQuote(target.absolutePath)
        val label = Shell.cmd("chcon u:object_r:apk_data_file:s0 $quoted").exec()
        if (!label.isSuccess) {
            val detail = (label.err + label.out).joinToString(" ").trim()
            val hint = if (detail.contains("not found", ignoreCase = true) ||
                detail.contains("inaccessible", ignoreCase = true)
            ) {
                "系统缺少 chcon 命令"
            } else {
                detail.ifBlank { "chcon 执行失败" }
            }
            error("无法设置内核的执行标签：$hint")
        }

        // 校验一次标签；有的 toybox 不支持 ls -Z，这种设备跳过校验，交给实际启动去判断
        val check = Shell.cmd("ls -Z $quoted").exec()
        if (check.isSuccess) {
            val text = (check.out + check.err).joinToString(" ")
            if (text.isNotBlank() && !text.contains("apk_data_file")) {
                error("内核的执行标签没有生效：$text")
            }
        }
    }

    /** 下载到固定临时文件，边下边算 MD5，返回十六进制摘要。 */
    private suspend fun download(url: String, expectedBytes: Long): String {
        val connection = (URI.create(url).toURL().openConnection() as HttpURLConnection).apply {
            instanceFollowRedirects = true
            connectTimeout = 30_000
            readTimeout = 180_000
            setRequestProperty("User-Agent", "LiBackupNext/${BuildConfig.APP_VERSION}")
            setRequestProperty("Accept", "application/octet-stream")
        }
        try {
            val http = connection.responseCode
            if (http != HttpURLConnection.HTTP_OK) error("下载 AList 内核失败：HTTP $http")

            val total = connection.contentLengthLong.takeIf { it > 0 } ?: expectedBytes
            val digest = MessageDigest.getInstance("MD5")
            var downloaded = 0L
            var lastPercent = -1
            val buffer = ByteArray(64 * 1024)

            BufferedInputStream(connection.inputStream).use { input ->
                BufferedOutputStream(FileOutputStream(downloadFile)).use { output ->
                    while (true) {
                        currentCoroutineContext().ensureActive()
                        val read = input.read(buffer)
                        if (read == -1) break
                        output.write(buffer, 0, read)
                        digest.update(buffer, 0, read)
                        downloaded += read
                        val percent = if (total > 0) (downloaded * 100 / total).toInt() else -1
                        if (percent != lastPercent) {
                            lastPercent = percent
                            _state.value = AlistInstallState.Downloading(downloaded, total)
                        }
                    }
                }
            }
            return digest.digest().joinToString("") { "%02x".format(it) }
        } catch (e: IOException) {
            throw IOException("无法下载 AList 内核，请检查网络（GitHub 可能需要代理）", e)
        } finally {
            connection.disconnect()
        }
    }

    /**
     * 极简 tar 读取器：只需要从 gzip 流里取出一个普通文件。
     * 支持 GNU longname（'L'），其它类型跳过。
     */
    private fun extract(input: InputStream, wanted: String, output: File): Boolean {
        val header = ByteArray(512)
        var longName: String? = null

        while (true) {
            if (!readFully(input, header)) return false
            if (isZeroBlock(header)) return false

            val name = readString(header, 0, 100)
            val type = (header[156].toInt() and 0xff).toChar()
            val size = readOctal(header, 124, 12)

            if (type == 'L') {
                longName = trimNul(readEntryData(input, size))
                continue
            }

            val effectiveName = longName ?: name
            longName = null
            val isFile = type == '0' || type == '\u0000'
            if (isFile && (effectiveName == wanted || effectiveName.endsWith("/$wanted"))) {
                BufferedOutputStream(FileOutputStream(output)).use { out ->
                    copyExactly(input, out, size)
                }
                return true
            }
            skipExactly(input, size)
        }
    }

    private fun readEntryData(input: InputStream, size: Long): String {
        if (size > 64 * 1024) error("tar 头信息异常：条目过大 $size")
        val out = ByteArrayOutputStream(size.toInt())
        copyExactly(input, out, size)
        return out.toString(Charsets.UTF_8.name())
    }

    private fun readFully(input: InputStream, buffer: ByteArray): Boolean {
        var offset = 0
        while (offset < buffer.size) {
            val read = input.read(buffer, offset, buffer.size - offset)
            if (read == -1) {
                if (offset == 0) return false
                throw IOException("tar 头不完整")
            }
            offset += read
        }
        return true
    }

    private fun isZeroBlock(block: ByteArray): Boolean = block.all { it == 0.toByte() }

    private fun readString(block: ByteArray, offset: Int, length: Int): String {
        var end = offset
        while (end < offset + length && block[end] != 0.toByte()) end++
        return String(block, offset, end - offset, Charsets.UTF_8)
    }

    private fun readOctal(block: ByteArray, offset: Int, length: Int): Long {
        val value = readString(block, offset, length).trim()
        if (value.isEmpty()) return 0
        return value.toLongOrNull(8) ?: error("tar 头里的文件大小不是八进制：$value")
    }

    private fun trimNul(value: String): String {
        val end = value.indexOf('\u0000')
        return if (end >= 0) value.substring(0, end) else value
    }

    private fun skipExactly(input: InputStream, size: Long) {
        var remaining = size + padding(size)
        val buffer = ByteArray(16 * 1024)
        while (remaining > 0) {
            val read = input.read(buffer, 0, minOf(buffer.size.toLong(), remaining).toInt())
            if (read == -1) throw IOException("tar 数据提前结束")
            remaining -= read
        }
    }

    private fun copyExactly(input: InputStream, output: OutputStream, size: Long) {
        val buffer = ByteArray(64 * 1024)
        var remaining = size
        while (remaining > 0) {
            val read = input.read(buffer, 0, minOf(buffer.size.toLong(), remaining).toInt())
            if (read == -1) throw IOException("tar 数据提前结束")
            output.write(buffer, 0, read)
            remaining -= read
        }
        var skip = padding(size)
        while (skip > 0) {
            val skipped = input.skip(skip)
            if (skipped <= 0) {
                if (input.read() == -1) throw IOException("tar 数据提前结束")
                skip -= 1
            } else {
                skip -= skipped
            }
        }
    }

    private fun padding(size: Long): Long = (512 - (size % 512)) % 512

    /** 路径里没有引号；仍然按 shell 规则包一层，避免以后路径变化出问题。 */
    private fun shellQuote(value: String): String = "'" + value.replace("'", "'\\''") + "'"
}
