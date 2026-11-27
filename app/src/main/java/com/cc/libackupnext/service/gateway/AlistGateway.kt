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

    /** 确保服务可用（已经运行则直接成功）。 */
    suspend fun ensureStarted(): Result<Unit>

    suspend fun stop()

    /** 用当前地址 / 账号生成一份 WebDAV 仓库表单，供「新建 WebDAV 仓库」一键预填。 */
    fun webDavRepo(name: String = "AList"): RemoteRepo
}
