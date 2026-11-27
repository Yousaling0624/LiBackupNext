package com.cc.libackupnext.service.gateway

import com.cc.libackupnext.data.RemoteRepo
import kotlinx.coroutines.flow.StateFlow

/**
 * 内置 AList（OpenList）服务的运行态。
 *
 * [Failed] 只表示「这次没起来」：用户意图仍然是启用（见 AlistPrefs.enabled），
 * 页面上给一个重试入口，而不是偷偷把开关关掉。
 */
sealed interface AlistState {

    /** 没有运行；用户没启用，或手动停掉了。 */
    data object Stopped : AlistState

    /** 进程已拉起，正在等 /ping 就绪。 */
    data object Starting : AlistState

    /** 已经能提供服务。 */
    data class Running(val port: Int) : AlistState

    /** 启动失败或中途退出，message 是可直接展示的中文原因。 */
    data class Failed(val message: String) : AlistState
}

/**
 * 内置 AList 内核（OpenList 可执行文件）的安装状态。
 *
 * 内核不再打进 APK：第一次启用时从 OpenList 官方 release 下载，
 * 校验 MD5 后解压到应用私有目录，再用 root 把文件标记成可执行（apk_data_file）。
 */
sealed interface AlistInstallState {

    /** 还没下载。 */
    data object Missing : AlistInstallState

    /** 正在下载；total 未知时为 0。 */
    data class Downloading(val downloaded: Long, val total: Long) : AlistInstallState

    /** 下载完成，正在解压。 */
    data object Extracting : AlistInstallState

    /** 已就绪，可以直接启动。 */
    data class Ready(val version: String) : AlistInstallState

    /** 下载 / 校验 / 设置执行标签失败。 */
    data class Failed(val message: String) : AlistInstallState
}

/**
 * 内置 AList 服务：一个跑在应用进程里的 OpenList 子进程。
 *
 * 只监听 127.0.0.1，由本机上的 rclone（:root 服务）通过 WebDAV 访问，
 * 因此完全不需要把端口暴露到局域网。
 *
 * 界面只依赖这个接口：启停、观察状态、拿 WebDAV 地址 / 账号构建仓库配置；
 * 具体怎么起进程、数据放哪、密码怎么生成都在实现里。
 */
interface AlistGateway {

    val state: StateFlow<AlistState>

    /** 内核的下载 / 解压 / 就绪状态。 */
    val installState: StateFlow<AlistInstallState>

    /** 用户是否希望它运行（跨重启保留的意图，不等于当前进程是否活着）。 */
    val enabled: Boolean

    val port: Int

    /** AList 的 admin 密码，同时用于网页登录和 WebDAV。 */
    val password: String

    /** 管理页面地址，带末尾斜杠。 */
    val webUiUrl: String

    /** WebDAV 服务地址（`…/dav`）。 */
    val webDavUrl: String

    /** WebDAV 用户名（AList 的 admin 用户名固定为 admin）。 */
    val webDavUser: String

    /** 开关：打开就开始启动进程并记住，关闭就停掉。 */
    fun setEnabled(enabled: Boolean)

    /** 应用启动时调用：用户上次开着就自动拉起。 */
    fun startIfEnabled()

    /** 确保服务可用（需要时先下载内核，已经运行则直接成功）。 */
    suspend fun ensureStarted(): Result<Unit>

    /** 只确保内核已下载并打好执行标签（不启动服务）。 */
    suspend fun ensureInstalled(): Result<Unit>

    /**
     * 用 WebView 里拿到的夸克 Cookie 添加 / 更新一个 Quark 存储。
     *
     * AList 没起来会先启动；Cookie 会先拿去夸克接口验证。
     *
     * @return 实际使用的挂载路径（如 `/quark`）。
     */
    suspend fun addQuarkStorage(cookie: String): Result<String>

    suspend fun stop()

    /** 用当前地址 / 账号生成一份 WebDAV 仓库表单，供「新建 WebDAV 仓库」一键预填。 */
    fun webDavRepo(name: String = "AList"): RemoteRepo
}
