# 锂备份 · LiBackupNext

一个基于 [rustic](https://github.com/rustic-rs/rustic) 的 Android 应用与数据备份工具。
需要 **root**，支持整应用备份、批量操作、多用户和增量去重；仓库可以放在本地目录，
也可以放在 WebDAV / SFTP / SMB 远程服务上。

## 特性

**备份内容**

- **应用**：apk（含 Split APK）、obb、`user` / `user_de` 数据、外部存储的 `Android/data`，以及已授予的运行时权限
- **文件夹**：自定目录，跳过 `.thumbnails` 与 `.trashed*`；恢复时覆盖同名文件、保留新增文件
- **系统数据**：已保存的 Wi-Fi（含密码）、联系人、通话记录，导出成 JSON 进仓库
- **图标快照**：一个包只存一份，应用卸载后列表里仍能显示图标

**仓库与操作**

- **增量去重**：rustic 的内容寻址存储，重复数据只存一份，重复备份只写变化的部分
- **多仓库**：本地 / WebDAV / SFTP / SMB
- **内置 AList（OpenList）**：应用内直接启动 AList 服务，登录夸克网盘、阿里云盘等，再通过本机 WebDAV 备份；只监听 127.0.0.1，不会暴露到局域网。内核不随安装包提供，首次启用时按需下载；夸克支持应用内快速登录，不用手动复制 Cookie
- **批量操作**：一次备份 / 恢复 / 覆盖 / 删除多个目标，支持按版本变化筛选；中断后可以「继续未完成」续批
- **多用户**：备份 / 恢复指定 Android 用户（工作资料、分身）；备份前 SIGSTOP 暂停应用，恢复前 force-stop
- **应用操作**：强停、冻结 / 解冻、清空数据、卸载、开关 Wi-Fi
- **快照管理**：备注编辑、版本变化标记、搜索与排序、显示系统应用

**系统体验**

- 前台服务 + WakeLock + Android 16 起用 Live Update 常驻通知，切到后台也能看进度
- 动态配色（可跟随系统壁纸，附带多种种子色与调色风格）、启动时检查更新
- 日志实时写盘、详细日志开关、用系统文件选择器导出日志包；崩溃后自动恢复被暂停的应用
- 备份自身安装包、从仓库取回未安装应用的图标、破坏性操作二次确认

## 架构

```
主进程（Compose UI + 业务层）
  AppListScreen / RepoListScreen / SettingsContent / ...
        │
  RusticClient     RcloneClient     AppManagerService 客户端
        │  AIDL：阻塞调用 + oneway 进度回调（大结果走共享内存）
        │
        │  主进程里另有内置 AList 子进程（OpenList，内核按需下载，监听 127.0.0.1:5244）
        ▼
:root 进程（三个独立绑定的 RootService）
  RusticServer     RcloneServer     AppManagerService.Service
        │ JNI            │ JNI              │ 隐藏 API
        ▼                ▼                 ▼
  librustic_bridge.so  librclone_bridge.so   PackageManager / ActivityManager ...
        │                │ dlopen
        │                ▼
        │          librclone.so（Go 版精简 rclone，进程内 RC：列目录 / serve restic）
        ▼                │
  本地仓库目录       serve restic：127.0.0.1 随机端口 + token（连 WebDAV / SFTP / SMB）
     rustic_core + rustic_backend（rest feature，远程时用 rest_url）
```

## 环境要求

| 项 | 版本 |
|---|---|
| Android | 12（API 31）及以上；minSdk 31 / targetSdk 37 / compileSdk 37 |
| root | Magisk、KernelSU、APatch |
| JDK | 21 |
| Android SDK / NDK | NDK `30.0.16248370` |
| Rust | stable + `aarch64-linux-android`；cargo-ndk 任意近期版本 |
| Go | 1.26（`GOTOOLCHAIN=auto` 可自动下载），用于编译 `librclone.so` |

## 构建

```bash
# 首次：装 Rust target 和 cargo-ndk
rustup target add aarch64-linux-android x86_64-linux-android
cargo install cargo-ndk

# debug 包（arm64 + x86_64，模拟器也能跑）
./gradlew :app:assembleDebug

# 正式包（APK 只打 arm64-v8a）
./gradlew :app:assembleRelease

# 只编 arm64 的 native 库，编译时间减半
./gradlew :app:assembleRelease -PrustAbis=arm64-v8a

# 打版本号（默认 versionName=1.0.0 / versionCode=1）
./gradlew :app:assembleRelease -PversionName=1.2.3 -PversionCode=10203
```

- `preBuild` 会自动跑 `buildRust`（`cargo ndk -P 30 -o jniLibs build --release --workspace`）和按 ABI 拆开的 `buildLibrclone_*`（`go build -buildmode=c-shared`）；`jniLibs/` 被 gitignore 忽略，首次克隆会自动重建，产物是 `librustic_bridge.so`、`librclone_bridge.so`、`librclone.so` 三个库。
- 内置 AList 的内核**不打进 APK**：应用第一次启用 AList 时，`OpenListInstaller` 从上游 release 下载 `openlist-android-{arm64,amd64}.tar.gz`（约 55MB）、校验 MD5 后解出 `libopenlist.so`（约 165MB），放在应用私有目录（`noBackupFilesDir/openlist/bin/`）。版本在 `app/build.gradle.kts` 顶部的 `openListVersion`，下载地址和 MD5 在安装器里，升级时一起改。
- Android 10+ 不允许应用从可写目录 execve，所以下载完成后用 root 执行一次 `chcon u:object_r:apk_data_file:s0`（进程仍以应用 uid 运行），每次启动前会重新打标签；运行内置 AList 需要 root。APK 因此不再带约 60MB 的内核，但首次启用需要联网下载。
- 夸克快速登录在内嵌 WebView 里完成：登录成功后从 `CookieManager` 读到 `__pus` / `__puus`，校验有效后通过 AList 管理 API 添加或更新 Quark 存储；Cookie 不写日志。
- `-PrustAbis` 默认 `arm64-v8a,x86_64`；debug 带 x86_64，release 的 `abiFilters` 只留 arm64-v8a。环境变量 `CARGO` / `GO` / `ANDROID_NDK_HOME` / `GOPROXY` 可覆盖工具路径。
- `BuildConfig` 写入 `APP_VERSION`（更新检查用）以及从 Cargo.toml 读出的 `RUSTIC_CORE_VERSION` / `RUSTIC_BACKEND_VERSION`（设置页展示）。
- `scripts/build-librclone.sh` 用于单独重编某个 ABI 排错，`scripts/dedupe-hardlinks.sh` 用于修复早期版本恢复时丢失的硬链接；`.github/workflows/release.yml` 打 tag 自动构建、签名并发 Release。
- **体积优化**：release 用 fat LTO + `opt-level="s"` + `panic=abort` + strip，Go 侧 `-trimpath -ldflags="-s -w"`，rclone 只注册 `local/webdav/sftp/smb` 与 `serve restic`（29MB → 约 19MB）；APK 排除多余的 `.so` / `.h`，`useLegacyPackaging` 压缩 so 与 dex，资源裁剪但不混淆。

## 仓库结构

```
app/
  src/main/java/com/cc/libackupnext/
    data/          纯数据模型 + AppPrefs + JSON DTO（BackupEntry / SavedRepo / RepoSpec ...）
    entity/        AppInfo
    service/       主进程业务层：RusticClient / RcloneClient / RemoteBackend /
                   RootServiceManager / ProgressService / UpdateChecker / LocalVolumes
                   :root 服务：RusticServer / RcloneServer / AppManagerService.Service
    ui/            Compose 界面：screen/（页面）、dialog/（弹窗）、theme/、util/
    util/          AppLog / LogReport / BackupNotifier / RepoPath
  src/main/aidl/   IRusticService / IRcloneService / IAppManagerService /
                   IBackupProgressCallback
  src/main/rust_bridge/   Rust virtual workspace
    rustic_bridge/        JNI：快照 CRUD / prune / restore（rustic_core + rustic_backend）
    rclone_bridge/        JNI：目录浏览、serve restic 启停，dlopen librclone.so
  librclone/       Go 精简版 rclone（main.go + go.mod），编成 c-shared 库
  src/main/jniLibs/  native 构建产物（gitignore）
hidden-api/        隐藏 API 的编译期 stub（compileOnly，不进 APK）
scripts/           构建 / 修复脚本
.github/workflows/ CI 发版
```

`RemoteBackend.kt` 是唯一拼 `librclone.so` 路径与 rclone inline 配置的地方；
目录选择（本地 + 远程）统一走 `FolderPickerDialog`，本地同样复用 rclone 的 `local` 后端，
在 `:root` 进程里列目录因此不需要存储权限。

## 备份格式

每次备份按「勾选单位」各生成一个或多个快照，label 形如 `<键>:<类型>`，键是包名或目录绝对路径：

| 勾选单位 | 快照类型 | 内容 |
|---|---|---|
| `apk` | `apk` / `obb` / `permissions` | 安装包（含 Split APK）、`/Android/obb/<包名>`、已授予的运行时权限 |
| `user` | `user` / `user_de` | `/data/user/<用户>/<包名>` 与 `/data/user_de/<用户>/<包名>` |
| `data` | `data` | `/data/media/<用户>/Android/data/<包名>` |
| `dir` | `dir` | 自定文件夹，label 为 `<路径>:dir` |
| `wifi` | `wifi` | 已保存的 Wi-Fi 配置（含密码）JSON |
| `contacts` | `contacts` | 联系人导出的 JSON |
| `calls` | `calls` | 通话记录导出的 JSON |
| （附带） | `icon` | 应用图标，一个包一份，不参与分组 |

同一次备份的快照共用一条 tag：`应用名|版本|用户名|时间戳`，靠时间戳把它们归成一组。
拼成一条是因为 rustic 的 tags 是 `BTreeSet`，拆开会被排序打乱；图标快照的 tag 只有包名。
`apk` 带着 obb 和权限、`user` 带着 `user_de` 一起备份；仓库里出现未知类型时容错跳过，不报错。

## 依赖

| 组件 | 版本 / 用途 |
|---|---|
| [rustic_core](https://github.com/rustic-rs/rustic) / rustic_backend | 0.13.0 / 0.7.0（`rest` feature）：仓库格式与备份引擎，RestBackend 用来访问远程 |
| [rclone](https://github.com/rclone/rclone) | v1.76.0（Go 1.26）：精简成进程内 RC 库，只留四个后端 + `serve restic` |
| [OpenList](https://github.com/OpenListTeam/OpenList) | v4.2.10（AGPL-3.0）：内置 AList 内核，首次启用时下载，应用内启动，把夸克 / 阿里云盘等翻译成本机 WebDAV |
| [libsu](https://github.com/topjohnwu/libsu) | 6.0.0：`RootService` 绑定与断线重连 |
| [Refine](https://github.com/RikkaW/Refine) | 4.4.0 + `:hidden-api` stub：隐藏 API 访问 |
| [rikkax.parcelablelist](https://github.com/RikkaW/ParcelableList) | 2.0.1：AIDL 传 Parcelable 列表 |
| Jetpack Compose / Material 3 | BOM 2026.09.00、material3 1.5.0-alpha27（与参考 App 对齐） |
| material-kolor | 5.0.1：动态配色；跟随系统壁纸时按深浅取系统 6 色作调色板覆盖色。需 Kotlin 2.4.10 配套（库用该版本编译），**勿升 6.0.0** |
| kotlinx-serialization | 1.9.0（用 BOM 统一 core / json，避免运行时版本错配） |
