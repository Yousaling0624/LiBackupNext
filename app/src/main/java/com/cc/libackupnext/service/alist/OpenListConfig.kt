package com.cc.libackupnext.service.alist

import kotlinx.serialization.json.Json
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.JsonPrimitive
import kotlinx.serialization.json.buildJsonObject
import kotlinx.serialization.json.contentOrNull
import kotlinx.serialization.json.jsonObject
import kotlinx.serialization.json.put
import java.security.SecureRandom

/**
 * OpenList 的纯逻辑部分：写 config.json、从日志里认出失败原因、给日志里的初始密码打码。
 *
 * 不碰 Android，JVM 单测直接跑。OpenList 自己的配置文件字段很多，
 * 我们只强制 `scheme.address` / `scheme.http_port`（只监听本机）和关掉文件日志，
 * 其余字段（数据库、临时目录、用户设置）原样保留 —— 升级 OpenList 时不会丢配置。
 */
internal object OpenListConfig {

    /** 只监听本机回环，WebDAV 不会暴露到局域网。 */
    const val ADDRESS = "127.0.0.1"

    private val json = Json { prettyPrint = true }

    private val random = SecureRandom()

    /** 无歧义字符集：日志里抄密码、手输时不容易看错。 */
    private const val ALPHABET = "ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz23456789"

    /** 生成 URL 安全的随机串，用作 admin 密码 / JWT 密钥。 */
    fun randomSecret(length: Int = 16): String = buildString(length) {
        repeat(length) { append(ALPHABET[random.nextInt(ALPHABET.length)]) }
    }

    /**
     * 生成 / 合并 config.json 的文本。
     *
     * @param existing 已有的配置文本；空或非法时按空配置处理。
     * @param jwtSecret 没有旧密钥时写入的新密钥；已有非空值则保留，登录态不会因重启失效。
     */
    fun prepare(existing: String?, port: Int, jwtSecret: String): String {
        val root = existing
            ?.takeIf { it.isNotBlank() }
            ?.let { runCatching { Json.parseToJsonElement(it).jsonObject }.getOrNull() }
            ?: JsonObject(emptyMap())
        val scheme = root["scheme"] as? JsonObject ?: JsonObject(emptyMap())
        val log = root["log"] as? JsonObject ?: JsonObject(emptyMap())
        val oldSecret = (root["jwt_secret"] as? JsonPrimitive)?.contentOrNull

        val merged = buildJsonObject {
            root.forEach { (key, value) ->
                if (key != "scheme" && key != "log" && key != "jwt_secret") put(key, value)
            }
            put("jwt_secret", oldSecret?.takeIf { it.isNotBlank() } ?: jwtSecret)
            put("scheme", buildJsonObject {
                scheme.forEach { (key, value) ->
                    if (key != "address" && key != "http_port") put(key, value)
                }
                put("address", ADDRESS)
                put("http_port", port)
            })
            put("log", buildJsonObject {
                log.forEach { (key, value) ->
                    if (key != "enable") put(key, value)
                }
                // 日志统一由我们捕获 stdout（--log-std）落到自己的有界文件里，
                // OpenList 自己再留一份会重复占用空间
                put("enable", false)
            })
        }
        return json.encodeToString(JsonObject.serializer(), merged)
    }

    /**
     * 把日志里的可识别失败翻成一句中文；认不出来返回 null。
     *
     * 只匹配「服务本身起不来」的原生报错，不做宽泛的「失败 / permission denied」
     * 之类的关键词匹配：已挂载的网盘在启动时初始化失败也会打这些字，把它们当成
     * AList 启动失败会让服务再也起不来。
     */
    fun classify(log: String, port: Int): String? {
        val text = log.lowercase()
        return when {
            "address already in use" in text ->
                "端口 $port 已被其他应用占用，请关闭占用端口的应用后重试"

            "failed to start http" in text ->
                "AList 无法监听 127.0.0.1:$port"

            "no space left on device" in text -> "手机存储空间不足"

            "database is locked" in text -> "AList 数据库被占用，请重试"

            "panic:" in text -> "AList 内核异常退出"

            else -> null
        }
    }

    private val ADMIN_PASSWORD_LINE =
        Regex("(?i)(initial password is:).*")

    /** OpenList 首次初始化会把密码打到 stdout；写日志前把后半段抹掉。 */
    fun redact(line: String): String = line.replace(ADMIN_PASSWORD_LINE) {
        "${it.groupValues[1]} ***"
    }
}
