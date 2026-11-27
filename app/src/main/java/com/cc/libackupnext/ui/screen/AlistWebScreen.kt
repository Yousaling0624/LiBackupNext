package com.cc.libackupnext.ui.screen

import android.graphics.Bitmap
import android.webkit.WebResourceError
import android.webkit.WebResourceRequest
import android.webkit.WebChromeClient
import android.webkit.WebView
import android.webkit.WebViewClient
import androidx.activity.compose.BackHandler
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.automirrored.filled.ArrowBack
import androidx.compose.material.icons.filled.Refresh
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.LinearProgressIndicator
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Scaffold
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.DisposableEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.unit.dp
import androidx.compose.ui.viewinterop.AndroidView
import com.cc.libackupnext.ui.AppTopBar

/**
 * 内嵌的 AList 管理页面。
 *
 * 页面只从 127.0.0.1 加载，明文回环由 network_security_config 单独放行；
 * 返回键优先退网页历史，退到头才关页面 —— 这是 WebView 页面的通用预期。
 */
@Composable
fun AlistWebScreen(
    url: String,
    onBack: () -> Unit,
    modifier: Modifier = Modifier
) {
    val pageBackground = MaterialTheme.colorScheme.surfaceContainer
    val searchBarBackground = MaterialTheme.colorScheme.surfaceContainerHighest

    var webView by remember { mutableStateOf<WebView?>(null) }
    var loading by remember { mutableStateOf(true) }
    var error by remember { mutableStateOf<String?>(null) }

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
                title = "AList",
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
        Box(
            modifier = Modifier
                .fillMaxSize()
                .padding(padding)
        ) {
            AndroidView(
                modifier = Modifier.fillMaxSize(),
                factory = { context ->
                    WebView(context).apply {
                        settings.javaScriptEnabled = true
                        settings.domStorageEnabled = true
                        settings.databaseEnabled = true
                        // 页面只服务管理配置，不需要读本地文件
                        settings.allowFileAccess = false
                        settings.allowContentAccess = false

                        webViewClient = object : WebViewClient() {
                            override fun onPageStarted(
                                view: WebView?,
                                url: String?,
                                favicon: Bitmap?
                            ) {
                                loading = true
                                error = null
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
                                    error = webError?.description?.toString()
                                        ?: "页面加载失败，请确认 AList 服务已启动"
                                }
                            }
                        }
                        webChromeClient = WebChromeClient()
                        loadUrl(url)
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

            error?.let { message ->
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

    DisposableEffect(Unit) {
        onDispose {
            webView?.apply {
                stopLoading()
                destroy()
            }
        }
    }
}
