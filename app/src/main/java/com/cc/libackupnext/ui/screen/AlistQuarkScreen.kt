package com.cc.libackupnext.ui.screen

import android.graphics.Bitmap
import android.webkit.CookieManager
import android.webkit.WebChromeClient
import android.webkit.WebResourceError
import android.webkit.WebResourceRequest
import android.webkit.WebView
import android.webkit.WebViewClient
import androidx.activity.compose.BackHandler
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.automirrored.filled.ArrowBack
import androidx.compose.material.icons.filled.Refresh
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.LinearProgressIndicator
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Scaffold
import androidx.compose.material3.Surface
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.DisposableEffect
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.collectAsState
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableIntStateOf
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.unit.dp
import androidx.compose.ui.viewinterop.AndroidView
import com.cc.libackupnext.service.alist.QuarkCookies
import com.cc.libackupnext.service.gateway.AlistInstallState
import com.cc.libackupnext.service.gateway.AlistState
import com.cc.libackupnext.ui.AppTopBar
import com.cc.libackupnext.ui.di.LocalAlist
import kotlinx.coroutines.delay

/** 夸克网盘网页版首页；登录后 `__pus` / `__puus` 会写在它的域下。 */
private const val QUARK_HOME = "https://pan.quark.cn/"

/** 快速登录的进展。Cookie 只在这条协程里短暂存在，不进状态、不打日志。 */
private sealed interface QuarkLoginPhase {

    /** 还没检测到登录态。 */
    data object Waiting : QuarkLoginPhase

    /** 已检测到登录，正在准备 AList 并添加存储。 */
    data object Working : QuarkLoginPhase

    /** 添加失败，页面上给重试入口。 */
    data class Failed(val message: String) : QuarkLoginPhase
}

/**
 * 夸克网盘快速登录。
 *
 * 手机浏览器里翻 Cookie 很麻烦，这里直接内嵌夸克网页：用户在页面里正常登录，
 * 应用每秒从 WebView 的 CookieManager 读一次（HttpOnly 也能读到），
 * 检测到完整登录态后自动把 Cookie 写进 AList 的 Quark 存储，然后返回。
 *
 * 内核没下载时会先下载（进度显示在顶部横幅里）。
 */
@Composable
fun AlistQuarkScreen(
    onBack: () -> Unit,
    /** 存储添加成功，参数是实际挂载路径（`/quark`、`/quark2`…）。 */
    onAdded: (String) -> Unit,
    modifier: Modifier = Modifier
) {
    val alist = LocalAlist.current
    val installState by alist.installState.collectAsState()
    val serverState by alist.state.collectAsState()

    var webView by remember { mutableStateOf<WebView?>(null) }
    var loading by remember { mutableStateOf(true) }
    var pageError by remember { mutableStateOf<String?>(null) }
    var phase by remember { mutableStateOf<QuarkLoginPhase>(QuarkLoginPhase.Waiting) }
    // 重试 = 重启下面的协程；WebView 不动，用户已经登录的页面还在
    var attempt by remember { mutableIntStateOf(0) }

    val pageBackground = MaterialTheme.colorScheme.surfaceContainer
    val searchBarBackground = MaterialTheme.colorScheme.surfaceContainerHighest

    /**
     * 轮询 Cookie → 自动添加存储。
     *
     * 已有登录态时（上次登录过）会立刻添加，相当于「刷新一次 Cookie」；
     * 没用登录态时一直等，直到用户在这个页面里登录成功。
     */
    LaunchedEffect(attempt) {
        phase = QuarkLoginPhase.Waiting
        while (true) {
            val header = runCatching {
                CookieManager.getInstance().getCookie(QUARK_HOME)
            }.getOrNull()
            if (header != null && QuarkCookies.isLoggedIn(header)) {
                phase = QuarkLoginPhase.Working
                alist.addQuarkStorage(header)
                    .onSuccess { path ->
                        // 用户在这里配置了网盘：顺便把服务开关打开，重启后自动拉起
                        alist.setEnabled(true)
                        onAdded(path)
                    }
                    .onFailure { error ->
                        phase = QuarkLoginPhase.Failed(error.message ?: "添加夸克网盘失败")
                    }
                return@LaunchedEffect
            }
            delay(1_000)
        }
    }

    val goBack: () -> Unit = {
        val view = webView
        if (view?.canGoBack() == true) view.goBack() else onBack()
    }
    BackHandler(onBack = goBack)

    Scaffold(
        modifier = modifier,
        containerColor = pageBackground,
        topBar = {
            AppTopBar(
                viewModel = null,
                title = "夸克网盘登录",
                pageBackground = pageBackground,
                searchBarBackground = searchBarBackground,
                navigationIcon = {
                    IconButton(onClick = goBack) {
                        Icon(Icons.AutoMirrored.Filled.ArrowBack, contentDescription = "返回")
                    }
                },
                showSearch = false,
                showSortMenu = false,
                showMiddleButton = false,
                showUserButton = false,
                customActions = {
                    IconButton(onClick = { webView?.reload() }) {
                        Icon(Icons.Filled.Refresh, contentDescription = "刷新")
                    }
                }
            )
        }
    ) { padding ->
        Column(
            modifier = Modifier
                .fillMaxSize()
                .padding(padding)
        ) {
            val downloading = installState as? AlistInstallState.Downloading
            val percent = downloading?.let {
                if (it.total > 0) (it.downloaded * 100 / it.total).toInt() else null
            }

            when (val current = phase) {
                QuarkLoginPhase.Waiting -> HintBanner(
                    text = "在下方页面登录夸克网盘，登录成功后会自动添加存储并返回"
                )

                QuarkLoginPhase.Working -> {
                    val message = when {
                        downloading != null && percent != null ->
                            "正在下载 AList 内核 $percent%（${downloading.downloaded / 1024 / 1024}MB / ${downloading.total / 1024 / 1024}MB）…"

                        downloading != null -> "正在下载 AList 内核…"
                        installState is AlistInstallState.Extracting -> "正在解压 AList 内核…"
                        installState is AlistInstallState.Missing -> "正在准备 AList…"
                        serverState is AlistState.Starting -> "正在启动 AList…"
                        else -> "正在添加夸克网盘…"
                    }
                    WorkingBanner(
                        message = message,
                        progress = percent?.let { it / 100f },
                        showProgress = downloading != null
                    )
                }

                is QuarkLoginPhase.Failed -> FailedBanner(
                    message = current.message,
                    onRetry = {
                        // 用户可能在失败后重新登录了：重试会重新读一次 Cookie
                        attempt++
                    }
                )
            }

            Box(modifier = Modifier.weight(1f)) {
                AndroidView(
                    modifier = Modifier.fillMaxSize(),
                    factory = { context ->
                        WebView(context).apply {
                            settings.javaScriptEnabled = true
                            settings.domStorageEnabled = true
                            settings.databaseEnabled = true
                            // 只做登录，不需要读本地文件
                            settings.allowFileAccess = false
                            settings.allowContentAccess = false
                            // WebView 默认 UA 带 "; wv"，部分登录页会直接拒绝；
                            // 去掉这两个标记，其余保持 Chrome 原样
                            settings.userAgentString = settings.userAgentString
                                ?.replace("; wv", "")
                                ?.replace("Version/4.0 ", "")

                            val cookies = CookieManager.getInstance()
                            cookies.setAcceptCookie(true)
                            cookies.setAcceptThirdPartyCookies(this, true)

                            webViewClient = object : WebViewClient() {
                                override fun onPageStarted(
                                    view: WebView?,
                                    url: String?,
                                    favicon: Bitmap?
                                ) {
                                    loading = true
                                    pageError = null
                                }

                                override fun onPageFinished(view: WebView?, url: String?) {
                                    loading = false
                                }

                                override fun onReceivedError(
                                    view: WebView?,
                                    request: WebResourceRequest?,
                                    webError: WebResourceError?
                                ) {
                                    if (request?.isForMainFrame == true) {
                                        loading = false
                                        pageError = webError?.description?.toString()
                                            ?: "页面加载失败，请检查网络"
                                    }
                                }

                                override fun shouldOverrideUrlLoading(
                                    view: WebView?,
                                    request: WebResourceRequest?
                                ): Boolean {
                                    // 登录页里的「打开 App」用 quark:// 之类的 scheme：
                                    // 只拦主框架，别跳出去把登录流程中断；iframe 正常放行
                                    val webRequest = request ?: return false
                                    if (!webRequest.isForMainFrame) return false
                                    val scheme = webRequest.url.scheme?.lowercase()
                                    return scheme != null &&
                                            scheme != "http" &&
                                            scheme != "https" &&
                                            scheme != "about" &&
                                            scheme != "blob" &&
                                            scheme != "data"
                                }
                            }
                            webChromeClient = WebChromeClient()
                            loadUrl(QUARK_HOME)
                            webView = this
                        }
                    }
                )

                if (loading) {
                    LinearProgressIndicator(
                        modifier = Modifier
                            .fillMaxWidth()
                            .align(Alignment.TopCenter)
                    )
                }

                pageError?.let { message ->
                    Text(
                        text = message,
                        style = MaterialTheme.typography.bodySmall,
                        color = MaterialTheme.colorScheme.error,
                        modifier = Modifier
                            .align(Alignment.TopCenter)
                            .fillMaxWidth()
                            .padding(horizontal = 16.dp, vertical = 12.dp)
                    )
                }
            }
        }
    }

    DisposableEffect(Unit) {
        onDispose {
            webView?.apply {
                stopLoading()
                destroy()
            }
            webView = null
        }
    }
}

@Composable
private fun HintBanner(text: String) {
    Surface(
        modifier = Modifier.fillMaxWidth(),
        color = MaterialTheme.colorScheme.surface
    ) {
        Text(
            text = text,
            style = MaterialTheme.typography.labelMedium,
            color = MaterialTheme.colorScheme.onSurfaceVariant,
            modifier = Modifier.padding(horizontal = 16.dp, vertical = 10.dp)
        )
    }
}

@Composable
private fun WorkingBanner(message: String, progress: Float?, showProgress: Boolean) {
    Surface(
        modifier = Modifier.fillMaxWidth(),
        color = MaterialTheme.colorScheme.secondaryContainer
    ) {
        Column(modifier = Modifier.padding(horizontal = 16.dp, vertical = 10.dp)) {
            Text(
                text = message,
                style = MaterialTheme.typography.labelMedium,
                color = MaterialTheme.colorScheme.onSecondaryContainer
            )
            if (showProgress) {
                Spacer(modifier = Modifier.height(6.dp))
                if (progress != null) {
                    LinearProgressIndicator(
                        progress = { progress.coerceIn(0f, 1f) },
                        modifier = Modifier.fillMaxWidth()
                    )
                } else {
                    LinearProgressIndicator(modifier = Modifier.fillMaxWidth())
                }
                Text(
                    text = "首次使用需要下载内核，请保持应用在前台",
                    style = MaterialTheme.typography.labelSmall,
                    color = MaterialTheme.colorScheme.onSecondaryContainer,
                    modifier = Modifier.padding(top = 6.dp)
                )
            }
        }
    }
}

@Composable
private fun FailedBanner(message: String, onRetry: () -> Unit) {
    Surface(
        modifier = Modifier.fillMaxWidth(),
        color = MaterialTheme.colorScheme.errorContainer
    ) {
        Column(modifier = Modifier.padding(horizontal = 16.dp, vertical = 10.dp)) {
            Row(verticalAlignment = Alignment.CenterVertically) {
                Column(modifier = Modifier.weight(1f)) {
                    Text(
                        text = "添加失败",
                        style = MaterialTheme.typography.labelMedium,
                        color = MaterialTheme.colorScheme.onErrorContainer
                    )
                    Text(
                        text = message,
                        style = MaterialTheme.typography.bodySmall,
                        color = MaterialTheme.colorScheme.onErrorContainer
                    )
                }
                TextButton(onClick = onRetry) {
                    Text("重试")
                }
            }
            Text(
                text = "登录失效时请在下方重新登录；root 或网络问题处理后点重试",
                style = MaterialTheme.typography.labelSmall,
                color = MaterialTheme.colorScheme.onErrorContainer,
                modifier = Modifier.padding(top = 4.dp)
            )
        }
    }
}
