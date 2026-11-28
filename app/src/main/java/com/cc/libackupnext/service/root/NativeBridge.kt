package com.cc.libackupnext.service.root

/**
 * :root 进程里两个 RootService 共用同一个进程，但各自按需绑定：用户可能先打开
 * 目录选择器（只绑 RcloneService），也可能先做备份（只绑 RusticService）。
 * 所以每边 onBind 都要确保 JNI 库已加载。
 *
 * 快照与 rclone 现在编在同一个 c-shared 库里（Go），System.loadLibrary 在同一进程、
 * 同一 ClassLoader 下幂等，重复调用是空操作。
 */
internal fun loadNativeBridge() = System.loadLibrary("ackupnext_native")
