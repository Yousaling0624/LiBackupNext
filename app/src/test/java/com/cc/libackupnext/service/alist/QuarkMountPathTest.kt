package com.cc.libackupnext.service.alist

import org.junit.Assert.assertEquals
import org.junit.Test

class QuarkMountPathTest {

    @Test
    fun `没有冲突时用 quark`() {
        assertEquals("/quark", freeQuarkMountPath(emptySet()))
        assertEquals("/quark", freeQuarkMountPath(setOf("/aliyun", "/onedrive")))
    }

    @Test
    fun `quark 被占用时依次往后找`() {
        assertEquals("/quark2", freeQuarkMountPath(setOf("/quark")))
        assertEquals("/quark3", freeQuarkMountPath(setOf("/quark", "/quark2")))
        assertEquals("/quark4", freeQuarkMountPath(setOf("/quark", "/quark2", "/quark3")))
    }
}
