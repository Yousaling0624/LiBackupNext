package com.cc.libackupnext.ui.screen

import android.content.ClipData
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.verticalScroll
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.automirrored.filled.ArrowBack
import androidx.compose.material.icons.filled.ContentCopy
import androidx.compose.material.icons.filled.Visibility
import androidx.compose.material.icons.filled.VisibilityOff
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Scaffold
import androidx.compose.material3.Surface
import androidx.compose.material3.Switch
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.collectAsState
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.runtime.setValue
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.Shape
import androidx.compose.ui.platform.ClipEntry
import androidx.compose.ui.platform.LocalClipboard
import androidx.compose.ui.unit.dp
import com.cc.libackupnext.BuildConfig
import com.cc.libackupnext.data.RemoteRepo
import com.cc.libackupnext.service.gateway.AlistInstallState
import com.cc.libackupnext.service.gateway.AlistState
import com.cc.libackupnext.ui.AppTopBar
import com.cc.libackupnext.ui.component.CardGroup
import com.cc.libackupnext.ui.component.SettingsCard
import com.cc.libackupnext.ui.di.LocalAlist
import com.cc.libackupnext.util.AppLog
import kotlinx.coroutines.launch

/** 服务状态给列表 / 设置页显示的一句话。 */
internal fun AlistState.describe(port: Int): String = when (this) {
    AlistState.Stopped -> "未启用"
    AlistState.Starting -> "正在启动…"
    is AlistState.Running -> "运行中 · 127.0.0.1:$port"
    is AlistState.Failed -> "启动失败：$message"
}

/** 内核安装状态的一句话。 */
private fun AlistInstallState.describeKernel(): String = when (this) {
    AlistInstallState.Missing -> "未下载 · 约 60MB"
    is AlistInstallState.Downloading -> {
        if (total > 0) {
            val percent = (downloaded * 100 / total).toInt()
            "正在下载 $percent%（${downloaded / 1024 / 1024}MB / ${total / 1024 / 1024}MB）"
        } else {
            "正在下载…（${downloaded / 1024 / 1024}MB）"
        }
    }

    AlistInstallState.Extracting -> "正在解压内核…"
    is AlistInstallState.Ready -> "已就绪 · OpenList $version"
    is AlistInstallState.Failed -> "不可用：$message"
}

/**
 * 内置 AList 管理页：启停服务、查看 WebDAV 凭据、打开 AList 网页配置网盘、
 * 一键用当前凭据预填一个 WebDAV 仓库。
 *
 * 页面本身不碰进程和密码的存储：都走 [com.cc.libackupnext.service.gateway.AlistGateway]。
 * OpenList 内核不再随 APK 提供，第一次启用时按需下载。
 */
@Composable
fun AlistScreen(
    onBack: () -> Unit,
    /** 打开内嵌网页（AList 管理界面）。 */
    onOpenWeb: () -> Unit,
    /** 打开夸克网盘登录页（登录后自动添加存储）。 */
    onOpenQuarkLogin: () -> Unit,
    /** 用户点了「新建 WebDAV 仓库」，带着预填好地址 / 账号的表单跳去仓库页。 */
    onCreateRepo: (RemoteRepo) -> Unit,
    modifier: Modifier = Modifier
) {
    val scope = rememberCoroutineScope()
    val clipboard = LocalClipboard.current
    val alist = LocalAlist.current
    val state by alist.state.collectAsState()
    val installState by alist.installState.collectAsState()

    // 开关的本地态：服务层只暴露「用户意图」，setEnabled 是异步的，
    // 界面不能等状态流回来才动 Switch，否则点一下会回弹
    var enabled by remember { mutableStateOf(alist.enabled) }
    var showPassword by remember { mutableStateOf(false) }
    var busy by remember { mutableStateOf(false) }
    var actionError by remember { mutableStateOf<String?>(null) }
    var showDownloadDialog by remember { mutableStateOf(false) }

    val pageBackground = MaterialTheme.colorScheme.surfaceContainer
    val searchBarBackground = MaterialTheme.colorScheme.surfaceContainerHighest

    fun copy(label: String, value: String) {
        scope.launch {
            clipboard.setClipEntry(ClipEntry(ClipData.newPlainText(label, value)))
            AppLog.toastOnMain("已复制$label")
        }
    }

    /** 打开网页 / 建仓库之前先确保服务在跑；失败原因留在页面上，不用弹窗来回切。 */
    fun withServer(action: () -> Unit) {
        if (busy) return
        // 同步置位：连点两下时第二下会在协程启动前就被挡掉
        busy = true
        actionError = null
        scope.launch {
            val result = alist.ensureStarted()
            busy = false
            result
                .onSuccess {
                    // 手动启动 = 用户希望它继续运行，顺手把开关打开，下次启动自动拉起
                    enabled = true
                    alist.setEnabled(true)
                    action()
                }
                .onFailure { actionError = it.message ?: "启动失败" }
        }
    }

    /** 内核卡片：没下载 / 下载失败时点一下重试；下载中的进度在副标题里。 */
    fun installKernel() {
        if (installState is AlistInstallState.Downloading ||
            installState is AlistInstallState.Extracting
        ) {
            return
        }
        scope.launch {
            actionError = null
            alist.ensureInstalled()
        }
    }

    Scaffold(
        modifier = modifier,
        containerColor = pageBackground,
        topBar = {
            AppTopBar(
                viewModel = null,
                title = "AList 网盘",
                pageBackground = pageBackground,
                searchBarBackground = searchBarBackground,
                navigationIcon = {
                    IconButton(onClick = onBack) {
                        Icon(Icons.AutoMirrored.Filled.ArrowBack, contentDescription = "返回")
                    }
                },
                showSearch = false,
                showSortMenu = false,
                showMiddleButton = false,
                showUserButton = false,
                // 空动作区：跳过默认的「用户 / 排序 / 更多」按钮分支，避免带出空菜单
                customActions = {}
            )
        }
    ) { padding ->
        Column(
            modifier = Modifier
                .fillMaxSize()
                .padding(padding)
                .verticalScroll(rememberScrollState())
                .padding(horizontal = 16.dp, vertical = 8.dp)
        ) {
            if (actionError != null) {
                Text(
                    text = actionError.orEmpty(),
                    style = MaterialTheme.typography.bodySmall,
                    color = MaterialTheme.colorScheme.error,
                    modifier = Modifier.padding(horizontal = 4.dp, vertical = 8.dp)
                )
            }

            CardGroup(
                "服务",
                { shape ->
                    SettingsCard(
                        shape = shape,
                        title = "内置 AList 服务",
                        subtitle = when (val install = installState) {
                            is AlistInstallState.Downloading -> "正在下载内核…"
                            AlistInstallState.Extracting -> "正在解压内核…"
                            is AlistInstallState.Failed -> "内核不可用"
                            else -> state.describe(alist.port)
                        },
                        hint = "只监听 127.0.0.1，不会暴露到局域网",
                        trailing = {
                            Switch(
                                checked = enabled,
                                onCheckedChange = { next ->
                                    if (!busy) {
                                        if (next && installState !is AlistInstallState.Ready) {
                                            showDownloadDialog = true
                                        } else {
                                            enabled = next
                                            actionError = null
                                            alist.setEnabled(next)
                                        }
                                    }
                                }
                            )
                        }
                    )
                },
                { shape ->
                    val canInstall = installState is AlistInstallState.Missing ||
                            installState is AlistInstallState.Failed
                    SettingsCard(
                        shape = shape,
                        title = "AList 内核",
                        subtitle = installState.describeKernel(),
                        hint = when (installState) {
                            is AlistInstallState.Failed -> "点此重试；运行需要 root"
                            AlistInstallState.Missing -> "点此下载；运行需要 root（用于设置执行标签）"
                            else -> "从 OpenList 官方 release 下载，保存在应用私有目录"
                        },
                        onClick = if (canInstall) {
                            { installKernel() }
                        } else {
                            null
                        }
                    )
                },
                { shape ->
                    SettingsCard(
                        shape = shape,
                        title = if (busy) "正在启动…" else "打开管理页面",
                        subtitle = "登录后在「存储」里添加夸克网盘、阿里云盘等",
                        onClick = { withServer(onOpenWeb) }
                    )
                }
            )

            CardGroup(
                "网盘",
                { shape ->
                    SettingsCard(
                        shape = shape,
                        title = "夸克网盘快速登录",
                        subtitle = "在应用内登录，自动获取 Cookie 并添加存储",
                        hint = "不用再手动复制浏览器里的 Cookie",
                        onClick = onOpenQuarkLogin
                    )
                }
            )

            CardGroup(
                "WebDAV 仓库",
                { shape ->
                    WebDavValueCard(
                        shape = shape,
                        title = "服务地址",
                        value = alist.webDavUrl,
                        onCopy = { copy("服务地址", alist.webDavUrl) }
                    )
                },
                { shape ->
                    WebDavValueCard(
                        shape = shape,
                        title = "用户名",
                        value = alist.webDavUser,
                        onCopy = { copy("用户名", alist.webDavUser) }
                    )
                },
                { shape ->
                    WebDavValueCard(
                        shape = shape,
                        title = "密码",
                        value = if (showPassword) alist.password else "••••••••",
                        onCopy = { copy("密码", alist.password) },
                        trailing = {
                            IconButton(onClick = { showPassword = !showPassword }) {
                                Icon(
                                    imageVector = if (showPassword) {
                                        Icons.Filled.VisibilityOff
                                    } else {
                                        Icons.Filled.Visibility
                                    },
                                    contentDescription = if (showPassword) "隐藏密码" else "显示密码"
                                )
                            }
                        }
                    )
                },
                { shape ->
                    SettingsCard(
                        shape = shape,
                        title = "新建 WebDAV 仓库",
                        subtitle = if (busy) {
                            "正在准备 AList…"
                        } else {
                            "自动填入上面的地址和账号，只需选择仓库目录"
                        },
                        onClick = { withServer { onCreateRepo(alist.webDavRepo()) } }
                    )
                },
                { shape -> AlistHelpCard(shape) }
            )

            CardGroup(
                "关于",
                { shape ->
                    SettingsCard(
                        shape = shape,
                        title = "OpenList ${BuildConfig.OPENLIST_VERSION}",
                        subtitle = "内置的 AList 内核，AGPL-3.0 · github.com/OpenListTeam/OpenList"
                    )
                }
            )
        }
    }

    if (showDownloadDialog) {
        AlertDialog(
            onDismissRequest = { showDownloadDialog = false },
            title = { Text("下载 AList 内核") },
            text = {
                Text(
                    "内置 AList 的内核不再随安装包提供。首次使用需要从 OpenList 官方 release " +
                            "下载约 60MB 的内核，校验后保存在应用私有目录。\n\n" +
                            "AList 进程以应用身份运行；下载完成后需要 root 设置一次执行标签。"
                )
            },
            confirmButton = {
                TextButton(
                    onClick = {
                        showDownloadDialog = false
                        enabled = true
                        actionError = null
                        alist.setEnabled(true)
                    }
                ) {
                    Text("下载并启用")
                }
            },
            dismissButton = {
                TextButton(onClick = { showDownloadDialog = false }) {
                    Text("取消")
                }
            }
        )
    }
}

@Composable
private fun WebDavValueCard(
    shape: Shape,
    title: String,
    value: String,
    onCopy: () -> Unit,
    trailing: (@Composable () -> Unit)? = null
) {
    SettingsCard(
        shape = shape,
        title = title,
        subtitle = value,
        subtitleMaxLines = 1,
        trailing = {
            Row {
                trailing?.invoke()
                IconButton(onClick = onCopy) {
                    Icon(Icons.Filled.ContentCopy, contentDescription = "复制$title")
                }
            }
        }
    )
}

@Composable
private fun AlistHelpCard(shape: Shape) {
    Surface(
        modifier = Modifier.fillMaxWidth(),
        shape = shape,
        color = MaterialTheme.colorScheme.surface
    ) {
        Column(modifier = Modifier.padding(horizontal = 16.dp, vertical = 14.dp)) {
            Text(
                text = "使用步骤",
                style = MaterialTheme.typography.bodyMedium,
                color = MaterialTheme.colorScheme.onSurface
            )
            val steps = listOf(
                "1. 启用服务：第一次会下载 AList 内核（约 60MB）",
                "2. 点「夸克网盘快速登录」，在应用内登录后会自动添加存储",
                "3. 点「新建 WebDAV 仓库」，在挂载路径下选择或新建一个文件夹作为仓库目录",
                "4. 之后备份 / 恢复都会经过 AList 直接读写网盘",
                "也可以在「打开管理页面」里手动登录 AList，添加其它网盘"
            )
            steps.forEach { step ->
                Text(
                    text = step,
                    style = MaterialTheme.typography.bodySmall,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                    modifier = Modifier.padding(top = 6.dp)
                )
            }
            Text(
                text = "在 AList 网页里改了密码后，本页仍显示初始密码；已保存的 WebDAV 仓库也要在「仓库 → 编辑」里同步更新。",
                style = MaterialTheme.typography.labelSmall,
                color = MaterialTheme.colorScheme.outline,
                modifier = Modifier.padding(top = 10.dp)
            )
        }
    }
}
