package com.cc.libackupnext.service.alist

import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext
import kotlinx.serialization.json.Json
import kotlinx.serialization.json.JsonArray
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.JsonPrimitive
import kotlinx.serialization.json.buildJsonObject
import kotlinx.serialization.json.booleanOrNull
import kotlinx.serialization.json.contentOrNull
import kotlinx.serialization.json.intOrNull
import kotlinx.serialization.json.put
import java.io.IOException
import java.net.HttpURLConnection
import java.net.URL
import java.nio.charset.StandardCharsets

/** OpenList 的夸克驱动名（对应 driver.Config.Name）。 */
private const val QUARK_DRIVER = "Quark"

/** 新存储的默认挂载路径；被占用时自动用 /quark2、/quark3 … */
private const val DEFAULT_QUARK_MOUNT = "/quark"

/**
 * OpenList 管理 API 的最小客户端，只连本机 127.0.0.1。
 *
 * 快速登录只做两件事：拿到 admin token，然后把夸克 Cookie 写进一个 Quark 存储。
 * Cookie 是完整登录态，不能写日志。
 */
internal class OpenListAdminApi(
    baseUrl: String,
    private val password: String
) {

    private val base = baseUrl.trimEnd('/')

    /**
     * 添加或更新夸克存储。
     *
     * 已经有 Quark 存储时只换 `addition.cookie`（保留挂载路径和其它设置）；
     * 没有就新建一个，挂载路径优先 `/quark`。
     *
     * @return 实际使用的挂载路径（比如 `/quark`），给界面显示。
     */
    suspend fun upsertQuarkStorage(cookie: String): Result<String> = withContext(Dispatchers.IO) {
        runCatching {
            // 先拿夸克自己的 /config 验证 Cookie，别等 AList 建完存储才发现登录失效
            QuarkAuth.validate(cookie)
            val token = login()
            val storages = listStorages(token)
            val existing = storages.firstOrNull { it.mountPath == DEFAULT_QUARK_MOUNT && it.driver == QUARK_DRIVER }
                ?: storages.firstOrNull { it.driver == QUARK_DRIVER }
            if (existing != null) {
                updateQuark(token, existing, cookie)
            } else {
                createQuark(token, storages, cookie)
            }
        }
    }

    private fun login(): String {
        val body = buildJsonObject {
            put("username", "admin")
            put("password", password)
        }
        val data = post("api/auth/login", null, body)["data"] as? JsonObject
        return data?.get("token")?.let { (it as? JsonPrimitive)?.contentOrNull }
            ?.takeIf { it.isNotBlank() }
            ?: throw ApiException("AList 登录失败")
    }

    private fun listStorages(token: String): List<StorageInfo> {
        val data = get("api/admin/storage/list?page=1&per_page=100", token)["data"] as? JsonObject
            ?: return emptyList()
        val content = data["content"] as? JsonArray ?: return emptyList()
        return content.mapNotNull { element ->
            val obj = element as? JsonObject ?: return@mapNotNull null
            val id = obj.int("id") ?: return@mapNotNull null
            StorageInfo(
                id = id,
                mountPath = obj.string("mount_path").orEmpty(),
                driver = obj.string("driver").orEmpty(),
                disabled = obj.boolean("disabled") ?: false
            )
        }
    }

    private fun updateQuark(token: String, existing: StorageInfo, cookie: String): String {
        val storage = get("api/admin/storage/get?id=${existing.id}", token)["data"] as? JsonObject
            ?: throw ApiException("读取 AList 存储配置失败")
        val addition = storage.string("addition")
            ?.let { runCatching { Json.parseToJsonElement(it) as? JsonObject }.getOrNull() }
            ?: JsonObject(emptyMap())
        val mergedAddition = JsonObject(addition + ("cookie" to JsonPrimitive(cookie)))
        val payload = JsonObject(storage + ("addition" to JsonPrimitive(mergedAddition.toString())))
        post("api/admin/storage/update", token, payload)
        if (existing.disabled) {
            // UpdateStorage 对已停用的存储只落库、不加载；用户刚登录，顺手启用。
            // 开启接口是 POST（不是 GET），query 里带 id
            post("api/admin/storage/enable?id=${existing.id}", token, JsonObject(emptyMap()))
        }
        return existing.mountPath
    }

    private fun createQuark(token: String, storages: List<StorageInfo>, cookie: String): String {
        val mount = freeQuarkMountPath(storages.map { it.mountPath }.toSet())
        val addition = buildJsonObject {
            put("cookie", cookie)
            put("root_folder_id", "0")
            put("order_by", "none")
            put("order_direction", "asc")
            put("use_transcoding_address", false)
            put("only_list_video_file", false)
            // 夸克下载链接直接 302 给客户端时容易因缺 Referer / UA 被拒；
            // 和驱动 Init 的迁移保持一致，走 AList 自己的代理
            put("AdditionVersion", 2)
        }
        val payload = buildJsonObject {
            put("mount_path", mount)
            put("order", 0)
            put("driver", QUARK_DRIVER)
            put("cache_expiration", 30)
            put("status", "work")
            put("addition", addition.toString())
            put("remark", "")
            put("disabled", false)
            put("order_by", "name")
            put("order_direction", "asc")
            put("extract_folder", "")
            put("web_proxy", true)
            put("webdav_policy", "native_proxy")
            put("proxy_range", false)
            put("down_proxy_url", "")
            put("disable_proxy_sign", false)
        }
        post("api/admin/storage/create", token, payload)
        return mount
    }

    private fun post(path: String, token: String?, body: JsonObject): JsonObject =
        execute("POST", path, token, body)

    private fun get(path: String, token: String): JsonObject =
        execute("GET", path, token, null)

    private fun execute(
        method: String,
        path: String,
        token: String?,
        body: JsonObject?
    ): JsonObject {
        val connection = (URL("$base/${path.trimStart('/')}").openConnection() as HttpURLConnection)
            .apply {
                requestMethod = method
                connectTimeout = 5_000
                // 新建 Quark 存储时 AList 会同步初始化一次驱动，留足时间
                readTimeout = 60_000
                setRequestProperty("Accept", "application/json")
                token?.let { setRequestProperty("Authorization", it) }
                if (body != null) {
                    doOutput = true
                    setRequestProperty("Content-Type", "application/json; charset=utf-8")
                }
            }
        try {
            if (body != null) {
                connection.outputStream.use {
                    it.write(body.toString().toByteArray(StandardCharsets.UTF_8))
                }
            }
            val http = connection.responseCode
            val text = (if (http in 200..299) connection.inputStream else connection.errorStream)
                ?.bufferedReader(StandardCharsets.UTF_8)?.use { it.readText() }
                .orEmpty()
            val root = runCatching { Json.parseToJsonElement(text) as? JsonObject }.getOrNull()
            val code = root?.int("code") ?: -1
            if (http !in 200..299 || code != 200) {
                val message = root?.string("message")?.takeIf { it.isNotBlank() }
                    ?: "AList 接口返回 HTTP $http"
                throw ApiException(message)
            }
            return root ?: throw ApiException("AList 返回了空响应")
        } finally {
            connection.disconnect()
        }
    }

    private data class StorageInfo(
        val id: Int,
        val mountPath: String,
        val driver: String,
        val disabled: Boolean
    )

    private class ApiException(message: String) : Exception(message)
}

/** 新夸克存储的挂载路径：/quark 被占用就往后排。 */
internal fun freeQuarkMountPath(taken: Set<String>): String {
    if (DEFAULT_QUARK_MOUNT !in taken) return DEFAULT_QUARK_MOUNT
    var index = 2
    while ("$DEFAULT_QUARK_MOUNT$index" in taken) index++
    return "$DEFAULT_QUARK_MOUNT$index"
}

/**
 * 用夸克自己的接口验证 Cookie 是否还能用。
 *
 * 和 AList Quark 驱动的 Init 请求同一个端点（/config），
 * 这样「登录态失效」能在建存储之前就报出来。
 */
internal object QuarkAuth {

    private const val CONFIG_URL = "https://drive.quark.cn/1/clouddrive/config?pr=ucpro&fr=pc"

    /** 和 OpenList Quark 驱动一致的 UA / Referer，避免被风控当成异常客户端。 */
    private const val UA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
            "(KHTML, like Gecko) quark-cloud-drive/2.5.20 Chrome/100.0.4896.160 " +
            "Electron/18.3.5.4-b478491100 Safari/537.36 Channel/pckk_other_ch"

    fun validate(cookie: String) {
        if (!QuarkCookies.isLoggedIn(cookie)) {
            throw IllegalStateException("没有检测到完整的夸克登录态，请重新登录")
        }
        val connection = (URL(CONFIG_URL).openConnection() as HttpURLConnection).apply {
            connectTimeout = 8_000
            readTimeout = 15_000
            setRequestProperty("Cookie", cookie)
            setRequestProperty("Referer", "https://pan.quark.cn")
            setRequestProperty("Accept", "application/json, text/plain, */*")
            setRequestProperty("User-Agent", UA)
        }
        try {
            val http = connection.responseCode
            val text = (if (http in 200..299) connection.inputStream else connection.errorStream)
                ?.bufferedReader(StandardCharsets.UTF_8)?.use { it.readText() }
                .orEmpty()
            val root = runCatching { Json.parseToJsonElement(text) as? JsonObject }.getOrNull()
                ?: throw IllegalStateException("夸克接口返回异常，请稍后重试")
            val status = root.int("status") ?: http
            val code = root.int("code") ?: 0
            if (http !in 200..299 || status >= 400 || code != 0) {
                throw IllegalStateException("夸克登录已失效，请重新登录")
            }
        } catch (e: IOException) {
            throw IllegalStateException("无法连接夸克服务器，请检查网络", e)
        } finally {
            connection.disconnect()
        }
    }
}

private fun JsonObject.string(key: String): String? =
    (this[key] as? JsonPrimitive)?.contentOrNull

private fun JsonObject.int(key: String): Int? =
    (this[key] as? JsonPrimitive)?.intOrNull

private fun JsonObject.boolean(key: String): Boolean? =
    (this[key] as? JsonPrimitive)?.booleanOrNull
