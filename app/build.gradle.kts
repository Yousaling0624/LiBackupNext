import java.util.Properties

plugins {
    alias(libs.plugins.android.application)
    alias(libs.plugins.kotlin.compose)
    alias(libs.plugins.kotlin.serialization)
    alias(libs.plugins.hiddenApi)
}

// ==================== AList（OpenList）内核版本 ====================

// 预编译包：https://github.com/OpenListTeam/OpenList/releases/tag/v$openListVersion
// 内核不在构建期下载、不打进 APK：应用第一次启用 AList 时按需下载并校验 MD5，
// 见 OpenListInstaller；升级这里只是换应用要拉取的版本。
// 为什么用预编译而不是源码构建：源码构建要先编前端 dist，CI 时间和维护成本都高；
// 预编译包与官方发布一致，版本和校验值都钉死，升级是有意识的一步。
val openListVersion = "4.2.10"

// ==================== 备份引擎依赖版本 ====================

// 直接读 app/native/go.mod，编译期写进 BuildConfig，设置页用来展示。
// 不在 Gradle 里再维护一份版本号。
val nativeGoModText = providers.fileContents(
    rootProject.layout.projectDirectory.file("app/native/go.mod")
).asText

fun goModuleVersion(text: String, module: String): String {
    val escaped = Regex.escape(module)
    // 单行 require 和 require (...) 块两种写法都要认
    return Regex("""(?m)^\s*(?:require\s+)?$escaped\s+(v[^\s]+)""")
        .find(text)?.groupValues?.get(1) ?: "unknown"
}

val resticVersion = goModuleVersion(nativeGoModText.getOrElse(""), "github.com/restic/restic")
val rcloneVersion = goModuleVersion(nativeGoModText.getOrElse(""), "github.com/rclone/rclone")

// 更新检查要比对的版本号，和 versionName 同一个来源（CI 用 -PversionName 覆盖）
val appVersionName = providers.gradleProperty("versionName").orNull
    ?.takeIf { it.isNotBlank() } ?: "1.0.0"

android {
    namespace = "com.cc.libackupnext"
    compileSdk {
        version = release(37)
    }
    ndkVersion = "30.0.16248370"

    defaultConfig {
        applicationId = "com.cc.libackupnext"
        minSdk = 31
        targetSdk = 37
        // CI 用 -PversionName / -PversionCode 覆盖；本地不传就是 1 / 1.0
        versionCode = providers.gradleProperty("versionCode").orNull?.toIntOrNull() ?: 1
        versionName = appVersionName

        testInstrumentationRunner = "androidx.test.runner.AndroidJUnitRunner"

        // 设置页展示用（值来自 app/native/go.mod）
        buildConfigField("String", "RESTIC_VERSION", "\"$resticVersion\"")
        buildConfigField("String", "RCLONE_VERSION", "\"$rcloneVersion\"")
        // 启动时检查更新用：GitHub release 的 tag_name 就是这个值
        buildConfigField("String", "APP_VERSION", "\"$appVersionName\"")
        // AList 页展示内置 OpenList 的版本
        buildConfigField("String", "OPENLIST_VERSION", "\"$openListVersion\"")
    }

    buildTypes {
        debug {
            // 调试包同时带 x86_64，方便模拟器跑
            ndk {
                abiFilters += listOf("arm64-v8a", "x86_64")
            }
        }
        release {
            // 正式包只打 arm64-v8a
            ndk {
                abiFilters += "arm64-v8a"
            }
            // 只裁剪 + 优化，不混淆（混淆 androidx 会导致正式包启动闪退，见 proguard-rules.pro）
            isMinifyEnabled = true
            isShrinkResources = true
            proguardFiles(
                getDefaultProguardFile("proguard-android-optimize.txt"),
                "proguard-rules.pro"
            )
        }
    }
    compileOptions {
        sourceCompatibility = JavaVersion.VERSION_17
        targetCompatibility = JavaVersion.VERSION_17
    }
    buildFeatures {
        compose = true
        aidl = true
        buildConfig = true
    }

    // 纯逻辑单测（Retention / OperationPlan / usecase）跑在 JVM 上，
    // 它们间接调用的 android.util.Log 需要返回默认值而不是抛「not mocked」
    testOptions {
        unitTests.isReturnDefaultValues = true
    }

    packaging {
        jniLibs {
            // go build -o xxx.so 会顺带生成 xxx.h，别打进 APK
            excludes += "**/*.h"
            // libackupnext_native.so 是 Go 的 c-shared 库；压缩 APK 里的 .so
            //（安装时解压），能省一大截体积。
            useLegacyPackaging = true
        }
        dex {
            useLegacyPackaging = true
        }
    }
}

dependencies {
    implementation(platform(libs.androidx.compose.bom))
    implementation(libs.androidx.activity.compose)
    implementation(libs.androidx.compose.material3)
    implementation(libs.materialKolor)
    implementation(libs.androidx.compose.ui)
    implementation(libs.androidx.compose.ui.graphics)
    implementation(libs.androidx.compose.ui.tooling.preview)
    implementation(libs.androidx.core.ktx)
    implementation(libs.androidx.lifecycle.runtime.ktx)
    implementation(libs.androidx.lifecycle.runtime.compose)
    testImplementation(libs.junit)
    androidTestImplementation(platform(libs.androidx.compose.bom))
    androidTestImplementation(libs.androidx.compose.ui.test.junit4)
    androidTestImplementation(libs.androidx.espresso.core)
    androidTestImplementation(libs.androidx.junit)
    debugImplementation(libs.androidx.compose.ui.test.manifest)
    debugImplementation(libs.androidx.compose.ui.tooling)

    implementation(libs.androidx.lifecycle.viewmodel.compose)
    implementation(libs.androidx.compose.material3.icons)

    implementation(libs.libsu.core)
    implementation(libs.libsu.service)
    implementation(libs.rikkax.parcelablelist)


    implementation(platform(libs.kotlinx.serialization.bom))
    implementation(libs.kotlinx.serialization.json)

    implementation(libs.hiddenApi.runtime)
    compileOnly(project(":hidden-api"))

}

// ==================== native JNI 库（Go） ====================

val nativeDir = rootProject.layout.projectDirectory.dir("app/native")

/** 从 local.properties 的 sdk.dir 推出 NDK 路径，给 cgo 的 CC 用。 */
val ndkHome: String? = run {
    System.getenv("ANDROID_NDK_HOME")?.takeIf { it.isNotBlank() }?.let { return@run it }

    val localProperties = rootProject.file("local.properties")
    if (!localProperties.exists()) return@run null

    val sdkDir = Properties().apply {
        localProperties.inputStream().use { load(it) }
    }.getProperty("sdk.dir") ?: return@run null

    val ndkRoot = file("$sdkDir/ndk")
    ndkRoot.listFiles()
        ?.filter { it.isDirectory }
        ?.maxByOrNull { it.name }
        ?.absolutePath
}

/** 允许用环境变量 GO 指定 go 可执行文件，否则交给 PATH 解析。 */
val goCmd: String = System.getenv("GO")?.takeIf { it.isNotBlank() } ?: "go"

/** GOOS=android + c-shared 必须走 NDK 的 clang；Windows 上带 .cmd 后缀。 */
val nativeHostTag: String = run {
    val osName = System.getProperty("os.name").orEmpty()
    when {
        osName.startsWith("Windows") -> "windows-x86_64"
        osName.startsWith("Mac") -> "darwin-x86_64"
        else -> "linux-x86_64"
    }
}
val nativeClangExt: String = if (nativeHostTag == "windows-x86_64") ".cmd" else ""

/** ABI -> (GOARCH, NDK clang 前缀)。debug 要 x86_64 给模拟器，正式包只会用到 arm64。 */
fun nativeTargetOf(abi: String): Pair<String, String> = when (abi) {
    "arm64-v8a" -> "arm64" to "aarch64-linux-android31-clang"
    "x86_64" -> "amd64" to "x86_64-linux-android31-clang"
    else -> error("native 库不支持的 ABI：$abi（nativeAbis 只支持 arm64-v8a / x86_64）")
}

// jniLibs 被 .gitignore 忽略，首次克隆后必须能自动重建，所以挂到 preBuild。
// 每个 ABI 一个 task，inputs/outputs 独立，改一个 ABI 不会触发另一个重编。
val nativeAbis: List<String> = (providers.gradleProperty("nativeAbis").orNull ?: "arm64-v8a,x86_64")
    .split(",")
    .map { it.trim() }
    .filter { it.isNotEmpty() }

val nativeTasks = nativeAbis.map { abi ->
    val (goArch, clangPrefix) = nativeTargetOf(abi)
    val out = layout.projectDirectory.file("src/main/jniLibs/$abi/libackupnext_native.so")
    val taskName = "buildNative_${abi.replace('-', '_')}"
    val clang = ndkHome?.let { ndk ->
        File(
            ndk,
            "toolchains/llvm/prebuilt/$nativeHostTag/bin/$clangPrefix$nativeClangExt"
        ).takeIf { it.exists() }?.absolutePath
    }
    if (clang == null) {
        logger.warn("找不到 NDK 的 $clangPrefix，$taskName 会失败；请检查 ANDROID_NDK_HOME / local.properties")
    }

    tasks.register<Exec>(taskName) {
        group = "build"
        description = "编译 $abi 的 libackupnext_native.so（Go，c-shared：restic + 进程内 rclone）"

        workingDir = nativeDir.asFile
        inputs.dir(nativeDir)
        outputs.file(out)

        environment("GOTOOLCHAIN", "auto")
        environment(
            "GOPROXY",
            System.getenv("GOPROXY").takeUnless { it.isNullOrBlank() } ?: "https://goproxy.cn,direct"
        )
        environment("GOOS", "android")
        environment("GOARCH", goArch)
        environment("CGO_ENABLED", "1")
        environment("GOFLAGS", "-mod=mod")
        clang?.let { environment("CC", it) }

        // 不挂 doFirst/doLast 闭包：Gradle Kotlin DSL 的脚本对象引用会破坏 configuration cache。
        // 顺带生成的 libackupnext_native.h 由 packaging.excludes 挡在 APK 外。
        commandLine(
            goCmd, "build", "-buildmode=c-shared", "-trimpath",
            "-ldflags=-s -w",
            "-o", out.asFile.absolutePath, "."
        )
    }
}

/** 聚合任务：一次编完所有目标 ABI。 */
val buildNative = tasks.register("buildNative") {
    group = "build"
    description = "编译 libackupnext_native.so（ABI：${nativeAbis.joinToString()}）"
    dependsOn(nativeTasks)
}

// OpenList 内核不再随包构建：第一次启用时由应用按需下载，
// 见 OpenListInstaller（版本号仍然是上面 BuildConfig 里的 OPENLIST_VERSION）。

tasks.named("preBuild").configure {
    dependsOn(buildNative)
}