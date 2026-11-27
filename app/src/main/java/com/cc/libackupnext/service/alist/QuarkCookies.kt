package com.cc.libackupnext.service.alist

/**
 * 夸克登录态的纯逻辑，JVM 单测直接跑。
 *
 * 夸克网页登录成功后，`__pus` 和 `__puus` 会一起出现（都是 HttpOnly，
 * JS 读不到，只能从 WebView 的 CookieManager 取）；缺一个 AList 初始化都会失败。
 */
internal object QuarkCookies {

    private val REQUIRED = listOf("__pus", "__puus")

    /** 把 `a=1; b=2` 拆成 Map；值里可能带 `=`，只按第一个等号切。 */
    fun parse(header: String?): Map<String, String> {
        val result = LinkedHashMap<String, String>()
        header.orEmpty().split(';').forEach { part ->
            val index = part.indexOf('=')
            if (index <= 0) return@forEach
            val name = part.substring(0, index).trim()
            if (name.isEmpty()) return@forEach
            result[name] = part.substring(index + 1).trim()
        }
        return result
    }

    /** 两个关键 Cookie 都有值才算登录成功。 */
    fun isLoggedIn(header: String?): Boolean {
        val cookies = parse(header)
        return REQUIRED.all { !cookies[it].isNullOrEmpty() }
    }
}
