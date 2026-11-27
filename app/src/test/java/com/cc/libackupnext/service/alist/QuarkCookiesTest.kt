package com.cc.libackupnext.service.alist

import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * 夸克登录态判定：CookieManager 返回的是 `a=1; b=2` 形式的长字符串，
 * 值里可能带 `=`，也可能夹杂无关 Cookie。
 */
class QuarkCookiesTest {

    @Test
    fun `同时有 __pus 和 __puus 才算登录`() {
        val header = "__pus=abc; __puus=def; other=1"
        assertTrue(QuarkCookies.isLoggedIn(header))
    }

    @Test
    fun `缺少任意一个关键 Cookie 都不算登录`() {
        assertFalse(QuarkCookies.isLoggedIn("__pus=abc; other=1"))
        assertFalse(QuarkCookies.isLoggedIn("__puus=def; other=1"))
        assertFalse(QuarkCookies.isLoggedIn("__pus=; __puus=def"))
        assertFalse(QuarkCookies.isLoggedIn(null))
        assertFalse(QuarkCookies.isLoggedIn(""))
    }

    @Test
    fun `值里带等号时只按第一个等号切分`() {
        val header = "__pus=a=b=c; __puus=d=e"
        val parsed = QuarkCookies.parse(header)

        assertEquals("a=b=c", parsed["__pus"])
        assertEquals("d=e", parsed["__puus"])
        assertTrue(QuarkCookies.isLoggedIn(header))
    }

    @Test
    fun `名字两边的空格会被去掉`() {
        val parsed = QuarkCookies.parse(" __pus = abc ;  __puus=def ")

        assertEquals("abc", parsed["__pus"])
        assertEquals("def", parsed["__puus"])
    }
}
