import com.cc.libackupnext.build.DownloadOpenListTask
import java.util.Properties

plugins {
    alias(libs.plugins.android.application)
    alias(libs.plugins.kotlin.compose)
    alias(libs.plugins.kotlin.serialization)
    alias(libs.plugins.hiddenApi)
}

// ==================== 内置 AList（OpenList）版本 ====================

// 预编译包：https://github.com/OpenListTeam/OpenList/releases/tag/v$openListVersion
// 升级时同步改这里和下面 openListAssetOf 里的 MD5（release 的 md5-android.txt）。
// 为什么用预编译而不是源码构建：源码构建要先编前端 dist，CI 时间和维护成本都高；
// 预编译包与官方发布一致，版本和校验值都钉死，升级是有意识的一步。
val openListVersion = "4.2.10"

// ==================== rustic 依赖版本 ====================

// 直接读 rust_bridge/rustic_bridge/Cargo.toml，编译期写进 BuildConfig，设置页用来展示
val cargoTomlText = providers.fileContents(
    layout.projectDirectory.file("src/main/rust_bridge/rustic_bridge/Cargo.toml")
).asText

fun cargoCrateVersion(text: String, crate: String): String {
    val escaped = Regex.escape(crate)
    // rustic_backend = { version = "0.7.0", default-features = false }
    Regex("""(?m)^\s*$escaped\s*=\s*\{[^}]*?version\s*=\s*"([^"]+)"""")
        .find(text)?.groupValues?.get(1)
        ?.let { return it }
    // rustic_core = "0.13.0"
    Regex("""(?m)^\s*$escaped\s*=\s*"([^"]+)"""")
        .find(text)?.groupValues?.get(1)
        ?.let { return it }
    return "unknown"
}

val rusticCoreVersion = cargoCrateVersion(cargoTomlText.getOrElse(""), "rustic_core")
val rusticBackendVersion = cargoCrateVersion(cargoTomlText.getOrElse(""), "rustic_backend")

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

        // 设置页展示用（值来自 Cargo.toml）
        buildConfigField("String", "RUSTIC_CORE_VERSION", "\"$rusticCoreVersion\"")
        buildConfigField("String", "RUSTIC_BACKEND_VERSION", "\"$rusticBackendVersion\"")
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

    // 内置 OpenList 的预编译产物（fetchOpenList_* 下载到这里）。
    // 单独一个目录而不是塞进 src/main/jniLibs：那边整体是 buildRust 的 outputs，
    // 两个任务写同一棵目录会让 up-to-date 判断互相干扰。
    sourceSets {
        getByName("main") {
            jniLibs.srcDir("src/main/openlistJniLibs")
        }
    }

    // 纯逻辑单测（Retention / OperationPlan / usecase）跑在 JVM 上，
    // 它们间接调用的 android.util.Log 需要返回默认值而不是抛「not mocked」
    testOptions {
        unitTests.isReturnDefaultValues = true
    }

    packaging {
        jniLibs {
            // cargo-ndk 会把依赖 crate 的 cdylib 一起拷进来，这里只保留我们自己的库
            excludes += "**/libcrc_fast-*.so"
            // go build -o xxx.so 会顺带生成 xxx.h，别打进 APK
            excludes += "**/*.h"
            // librclone.so 是精简版 rclone 的 c-shared 库，librclone_bridge.so 会按
            // nativeLibraryDir 里的绝对路径 dlopen 它，所以这些 .so 都必须解压出来
            // 压缩 APK 里的 .so（安装时解压），能省一大截体积。
            // libopenlist.so 是 Go 可执行文件（不是动态库），同样靠解压后才能 execve。
            useLegacyPackaging = true
            // OpenList 已经是 strip 过的 Go 二进制，别让 AGP 再拿 llvm-strip 处理一遍
            keepDebugSymbols += "**/libopenlist.so"
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

// ==================== rustic JNI 库 ====================

val rustDir = rootProject.layout.projectDirectory.dir("app/src/main/rust_bridge")
val jniLibsDir = layout.projectDirectory.dir("src/main/jniLibs")

/** 从 local.properties 的 sdk.dir 推出 NDK 路径，给 cargo-ndk 用。 */
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

val cargoBinDir: String? = run {
    val home = File(System.getProperty("user.home"), ".cargo/bin")
    home.takeIf { it.isDirectory }?.absolutePath
}

val cargoCmd: String = run {
    System.getenv("CARGO")?.takeIf { it.isNotBlank() }?.let { return@run it }

    cargoBinDir?.let { dir ->
        listOf(File(dir, "cargo.exe"), File(dir, "cargo"))
            .firstOrNull { it.exists() }?.absolutePath
    } ?: "cargo"
}

// 默认两个 ABI；打正式包可以 `-PrustAbis=arm64-v8a` 只编 v8a，省一半时间
val rustAbis: List<String> = (providers.gradleProperty("rustAbis").orNull ?: "arm64-v8a,x86_64")
    .split(",")
    .map { it.trim() }
    .filter { it.isNotEmpty() }

val buildRust = tasks.register<Exec>("buildRust") {
    group = "build"
    description = "编译 rustic_bridge / rclone_bridge 两个 JNI 库（默认 arm64-v8a + x86_64，可用 -PrustAbis 指定）"

    workingDir = rustDir.asFile
    ndkHome?.let { environment("ANDROID_NDK_HOME", it) }
    cargoBinDir?.let {
        val path = System.getenv("PATH").orEmpty()
        environment("PATH", if (path.isBlank()) it else "$it${File.pathSeparator}$path")
    }

    commandLine(
        buildList {
            add(cargoCmd)
            add("ndk")
            rustAbis.forEach {
                add("-t")
                add(it)
            }
            add("-P")
            add("30")
            add("-o")
            add(jniLibsDir.asFile.absolutePath)
            add("build")
            add("--release")
            // virtual workspace：显式带 --workspace，保证两个 cdylib 都产出
            add("--workspace")
        }
    )

    inputs.file(rustDir.file("Cargo.toml"))
    // 两个 crate 的源码 / manifest 都要进 inputs，改任意一个都触发重编
    inputs.dir(rustDir.dir("rustic_bridge/src"))
    inputs.file(rustDir.file("rustic_bridge/Cargo.toml"))
    inputs.dir(rustDir.dir("rclone_bridge/src"))
    inputs.file(rustDir.file("rclone_bridge/Cargo.toml"))
    outputs.dir(jniLibsDir)
}

// ==================== 精简 librclone（进程内 RC 库） ====================

/** 和 cargoCmd 一样，允许用环境变量 GO 指定 go 可执行文件，否则交给 PATH 解析。 */
val goCmd: String = System.getenv("GO")?.takeIf { it.isNotBlank() } ?: "go"

val librcloneSrcDir = rootProject.layout.projectDirectory.dir("app/librclone")

/** GOOS=android + c-shared 必须走 NDK 的 clang；Windows 上带 .cmd 后缀。 */
val librcloneHostTag: String = run {
    val osName = System.getProperty("os.name").orEmpty()
    when {
        osName.startsWith("Windows") -> "windows-x86_64"
        osName.startsWith("Mac") -> "darwin-x86_64"
        else -> "linux-x86_64"
    }
}
val librcloneClangExt: String = if (librcloneHostTag == "windows-x86_64") ".cmd" else ""

/** ABI -> (GOARCH, NDK clang 前缀)。debug 要 x86_64 给模拟器，正式包只会用到 arm64。 */
fun librcloneTargetOf(abi: String): Pair<String, String> = when (abi) {
    "arm64-v8a" -> "arm64" to "aarch64-linux-android31-clang"
    "x86_64" -> "amd64" to "x86_64-linux-android31-clang"
    else -> error("librclone 不支持的 ABI：$abi（rustAbis 只支持 arm64-v8a / x86_64）")
}

// jniLibs 被 .gitignore 忽略，首次克隆后必须能自动重建，所以挂到 preBuild。
// ABI 列表直接复用 rustAbis：debug 默认 arm64 + x86_64 两个，打正式包时用
// 每个 ABI 一个 task，inputs/outputs 独立，改一个 ABI 不会触发另一个重编。
val librcloneTasks = rustAbis.map { abi ->
    val (goArch, clangPrefix) = librcloneTargetOf(abi)
    val out = layout.projectDirectory.file("src/main/jniLibs/$abi/librclone.so")
    val taskName = "buildLibrclone_${abi.replace('-', '_')}"
    val clang = ndkHome?.let { ndk ->
        File(
            ndk,
            "toolchains/llvm/prebuilt/$librcloneHostTag/bin/$clangPrefix$librcloneClangExt"
        ).takeIf { it.exists() }?.absolutePath
    }
    if (clang == null) {
        logger.warn("找不到 NDK 的 $clangPrefix，$taskName 会失败；请检查 ANDROID_NDK_HOME / local.properties")
    }

    tasks.register<Exec>(taskName) {
        group = "build"
        description = "编译 $abi 的 librclone.so（c-shared，webdav/sftp/smb + serve restic RC）"

        workingDir = librcloneSrcDir.asFile
        inputs.file(librcloneSrcDir.file("main.go"))
        inputs.file(librcloneSrcDir.file("go.mod"))
        inputs.file(librcloneSrcDir.file("go.sum"))
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
        // 顺带生成的 librclone.h 由 packaging.excludes 挡在 APK 外。
        commandLine(
            goCmd, "build", "-buildmode=c-shared", "-trimpath",
            "-ldflags=-s -w",
            "-o", out.asFile.absolutePath, "."
        )
    }
}

/** 聚合任务：保留原来的任务名，外部脚本 / 文档不用改。 */
val buildLibrclone = tasks.register("buildLibrclone") {
    group = "build"
    description = "编译 librclone.so（ABI：${rustAbis.joinToString()}）"
    dependsOn(librcloneTasks)
}

// ==================== 内置 OpenList（AList） ====================

/**
 * ABI -> (上游 release 的 GOARCH, md5-android.txt 里对应 tar.gz 的 MD5)。
 *
 * 版本和 MD5 一起钉死：升级时要同时改 `openListVersion` 和这里的值，
 * 下载内容对不上会直接让构建失败，而不是悄悄打进一个不同的二进制。
 */
fun openListAssetOf(abi: String): Pair<String, String> = when (abi) {
    "arm64-v8a" -> "arm64" to "12986ca60a8b561ea26b3f53cac738ed"
    "x86_64" -> "amd64" to "1e50e78407d8d2789f3aae3f93bb99d9"
    else -> error("OpenList 不支持的 ABI：$abi（rustAbis 只支持 arm64-v8a / x86_64）")
}

val openListTasks = rustAbis.map { abi ->
    val (goArch, expectedMd5) = openListAssetOf(abi)
    val out = layout.projectDirectory.file("src/main/openlistJniLibs/$abi/libopenlist.so")
    tasks.register<DownloadOpenListTask>("fetchOpenList_${abi.replace('-', '_')}") {
        group = "build"
        description = "下载 $abi 的内置 OpenList（AList）v$openListVersion"
        url = "https://github.com/OpenListTeam/OpenList/releases/download/" +
                "v$openListVersion/openlist-android-$goArch.tar.gz"
        md5 = expectedMd5
        outputFile = out.asFile
    }
}

tasks.named("preBuild").configure {
    dependsOn(buildRust, buildLibrclone, openListTasks)
}