package com.cc.libackupnext.service.alist

import kotlinx.serialization.json.Json
import kotlinx.serialization.json.int
import kotlinx.serialization.json.jsonObject
import kotlinx.serialization.json.jsonPrimitive
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test

class OpenListConfigTest {

    private fun parse(text: String) = Json.parseToJsonElement(text).jsonObject

    @Test
    fun `空配置时写入回环地址与端口并关闭文件日志`() {
        val config = parse(OpenListConfig.prepare(existing = null, port = 5244, jwtSecret = "jwt-1"))

        val scheme = config["scheme"]!!.jsonObject
        assertEquals("127.0.0.1", scheme["address"]!!.jsonPrimitive.content)
        assertEquals(5244, scheme["http_port"]!!.jsonPrimitive.int)
        assertFalse(config["log"]!!.jsonObject["enable"]!!.jsonPrimitive.content.toBoolean())
        assertEquals("jwt-1", config["jwt_secret"]!!.jsonPrimitive.content)
    }

    @Test
    fun `已有配置保留数据库与密钥只强制网络字段`() {
        val existing = """
            {
              "site_url": "/panel",
              "jwt_secret": "keep-me",
              "database": {"type": "sqlite3", "db_file": "/data/app/openlist/data.db"},
              "scheme": {"address": "0.0.0.0", "http_port": 9000, "https_port": -1},
              "log": {"enable": true, "name": "/data/app/openlist/log.log"}
            }
        """.trimIndent()

        val config = parse(
            OpenListConfig.prepare(existing = existing, port = 6000, jwtSecret = "new-jwt")
        )

        assertEquals("keep-me", config["jwt_secret"]!!.jsonPrimitive.content)
        assertEquals("/panel", config["site_url"]!!.jsonPrimitive.content)
        assertEquals(
            "/data/app/openlist/data.db",
            config["database"]!!.jsonObject["db_file"]!!.jsonPrimitive.content
        )

        val scheme = config["scheme"]!!.jsonObject
        assertEquals("127.0.0.1", scheme["address"]!!.jsonPrimitive.content)
        assertEquals(6000, scheme["http_port"]!!.jsonPrimitive.int)
        assertEquals(-1, scheme["https_port"]!!.jsonPrimitive.int)

        val log = config["log"]!!.jsonObject
        assertFalse(log["enable"]!!.jsonPrimitive.content.toBoolean())
        assertEquals("/data/app/openlist/log.log", log["name"]!!.jsonPrimitive.content)
    }

    @Test
    fun `损坏的配置按空配置重建`() {
        val config = parse(
            OpenListConfig.prepare(existing = "{not json", port = 5244, jwtSecret = "jwt-2")
        )
        assertEquals("127.0.0.1", config["scheme"]!!.jsonObject["address"]!!.jsonPrimitive.content)
        assertEquals("jwt-2", config["jwt_secret"]!!.jsonPrimitive.content)
    }

    @Test
    fun `端口冲突翻成中文并带上端口号`() {
        val message = OpenListConfig.classify(
            "failed to start http: listen tcp 127.0.0.1:5244: bind: address already in use",
            port = 5244
        )
        assertTrue(message!!.contains("5244"))
        assertTrue(message.contains("占用"))
    }

    @Test
    fun `正常启动日志不会被误判`() {
        assertNull(
            OpenListConfig.classify(
                "INFO start HTTP server @ 127.0.0.1:5244\nINFO init offline download tool",
                port = 5244
            )
        )
    }

    @Test
    fun `首次启动日志里的明文密码会被打码`() {
        val redacted = OpenListConfig.redact(
            "Successfully created the admin user and the initial password is: SuperSecret123"
        )
        assertFalse(redacted.contains("SuperSecret123"))
        assertTrue(redacted.contains("initial password is:"))
    }

    @Test
    fun `生成的口令不包含容易看错的字符`() {
        val secret = OpenListConfig.randomSecret(32)
        assertEquals(32, secret.length)
        assertTrue(secret.all { it.isLetterOrDigit() })
        assertFalse(secret.any { it in "0O1Il" })
    }
}
