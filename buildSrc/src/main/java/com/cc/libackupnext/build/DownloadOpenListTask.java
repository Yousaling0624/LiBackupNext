package com.cc.libackupnext.build;

import org.gradle.api.DefaultTask;
import org.gradle.api.GradleException;
import org.gradle.api.tasks.Input;
import org.gradle.api.tasks.OutputFile;
import org.gradle.api.tasks.TaskAction;

import java.io.BufferedInputStream;
import java.io.BufferedOutputStream;
import java.io.ByteArrayOutputStream;
import java.io.File;
import java.io.FileInputStream;
import java.io.FileOutputStream;
import java.io.IOException;
import java.io.InputStream;
import java.io.OutputStream;
import java.net.HttpURLConnection;
import java.net.URI;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.StandardCopyOption;
import java.security.MessageDigest;

/**
 * 下载 OpenList 的 Android 预编译包（.tar.gz），校验 MD5 后把里面的 openlist 解出来，
 * 写成 {@code libopenlist.so}。
 *
 * 为什么叫 .so 而不是可执行文件：Android 10 起 targetSdk 29+ 的应用不能在可写目录
 * （files / cache）里 execve；但安装时解压到 nativeLibraryDir 的 lib*.so 位于
 * apk_data_file，允许执行。项目已经打开 useLegacyPackaging，native 库会解压出来，
 * 所以运行期直接按 {@code nativeLibraryDir/libopenlist.so} 启动即可。
 *
 * 用 Java 写在 buildSrc 里而不是 build.gradle.kts 的 doLast：本工程开了
 * configuration cache，脚本里的动作闭包会把脚本对象一起捕获，Gradle 会拒绝缓存。
 */
public abstract class DownloadOpenListTask extends DefaultTask {

    private String url;
    private String md5;
    private File outputFile;

    /** 上游 release 的 tar.gz 地址。 */
    @Input
    public String getUrl() {
        return url;
    }

    public void setUrl(String url) {
        this.url = url;
    }

    /** 上游 md5-android.txt 里公布的压缩包 MD5。 */
    @Input
    public String getMd5() {
        return md5;
    }

    public void setMd5(String md5) {
        this.md5 = md5;
    }

    /** 产出的 libopenlist.so。 */
    @OutputFile
    public File getOutputFile() {
        return outputFile;
    }

    public void setOutputFile(File outputFile) {
        this.outputFile = outputFile;
    }

    @TaskAction
    public void run() throws Exception {
        File output = getOutputFile().getAbsoluteFile();
        File dir = output.getParentFile();
        if (dir == null || (!dir.isDirectory() && !dir.mkdirs())) {
            throw new GradleException("无法创建 OpenList 输出目录：" + dir);
        }

        File archive = new File(dir, output.getName() + ".tar.gz.part");
        File staged = new File(dir, output.getName() + ".part");
        try {
            getLogger().lifecycle("下载 OpenList：{}", getUrl());
            download(getUrl(), archive);

            String actual = md5(archive);
            if (!actual.equalsIgnoreCase(getMd5())) {
                throw new GradleException(
                        "OpenList 校验失败：期望 " + getMd5() + "，实际 " + actual
                );
            }

            try (InputStream gzip = new java.util.zip.GZIPInputStream(
                    new BufferedInputStream(new FileInputStream(archive)))) {
                if (!extract(gzip, "openlist", staged)) {
                    throw new GradleException("压缩包里找不到 openlist 可执行文件");
                }
            }

            Files.move(
                    staged.toPath(),
                    output.toPath(),
                    StandardCopyOption.REPLACE_EXISTING
            );
        } finally {
            //noinspection ResultOfMethodCallIgnored
            archive.delete();
            //noinspection ResultOfMethodCallIgnored
            staged.delete();
        }

        // Windows 上没有执行位；APK 里的权限由系统安装时决定，这里失败也不影响。
        //noinspection ResultOfMethodCallIgnored
        output.setExecutable(true, false);
        getLogger().lifecycle("OpenList 已就绪：{}（{} MB）", output, output.length() / 1024 / 1024);
    }

    private static void download(String url, File target) throws IOException {
        HttpURLConnection connection = (HttpURLConnection) URI.create(url)
                .toURL()
                .openConnection();
        try {
            connection.setInstanceFollowRedirects(true);
            connection.setConnectTimeout(30_000);
            connection.setReadTimeout(180_000);
            connection.setRequestProperty("User-Agent", "LiBackupNext-build");

            int code = connection.getResponseCode();
            if (code != HttpURLConnection.HTTP_OK) {
                throw new GradleException("下载 OpenList 失败：HTTP " + code + " " + url);
            }

            try (InputStream in = new BufferedInputStream(connection.getInputStream());
                 OutputStream out = new BufferedOutputStream(new FileOutputStream(target))) {
                copy(in, out);
            }
        } finally {
            connection.disconnect();
        }
    }

    private static String md5(File file) throws Exception {
        MessageDigest digest = MessageDigest.getInstance("MD5");
        try (InputStream in = new BufferedInputStream(new FileInputStream(file))) {
            byte[] buffer = new byte[64 * 1024];
            int read;
            while ((read = in.read(buffer)) != -1) {
                digest.update(buffer, 0, read);
            }
        }
        StringBuilder hex = new StringBuilder();
        for (byte b : digest.digest()) {
            hex.append(String.format("%02x", b));
        }
        return hex.toString();
    }

    /**
     * 极简 tar 读取器：只需要从 gzip 流里取出一个普通文件。
     * 支持 GNU longname（'L'），其它类型的条目直接跳过。
     */
    private static boolean extract(InputStream in, String wanted, File output) throws IOException {
        byte[] header = new byte[512];
        String longName = null;

        while (true) {
            if (!readFully(in, header)) return false;
            if (isZeroBlock(header)) return false;

            String name = readString(header, 0, 100);
            char type = (char) (header[156] & 0xff);
            long size = readOctal(header, 124, 12);

            if (type == 'L') {
                longName = readEntryData(in, size).trim();
                longName = trimNul(longName);
                continue;
            }

            String effectiveName = longName != null ? longName : name;
            longName = null;

            boolean isFile = type == '0' || type == 0 || type == '\0';
            if (isFile && (effectiveName.equals(wanted)
                    || effectiveName.endsWith("/" + wanted))) {
                try (OutputStream out = new BufferedOutputStream(new FileOutputStream(output))) {
                    copyExactly(in, out, size);
                }
                return true;
            }

            skipExactly(in, size);
        }
    }

    private static String readEntryData(InputStream in, long size) throws IOException {
        if (size > 64 * 1024) {
            throw new GradleException("tar 头信息异常：条目过大 " + size);
        }
        ByteArrayOutputStream out = new ByteArrayOutputStream((int) size);
        copyExactly(in, out, size);
        return out.toString(StandardCharsets.UTF_8);
    }

    private static boolean readFully(InputStream in, byte[] buffer) throws IOException {
        int offset = 0;
        while (offset < buffer.length) {
            int read = in.read(buffer, offset, buffer.length - offset);
            if (read == -1) {
                if (offset == 0) return false;
                throw new IOException("tar 头不完整");
            }
            offset += read;
        }
        return true;
    }

    private static boolean isZeroBlock(byte[] block) {
        for (byte b : block) {
            if (b != 0) return false;
        }
        return true;
    }

    private static String readString(byte[] block, int offset, int length) {
        int end = offset;
        while (end < offset + length && block[end] != 0) end++;
        return new String(block, offset, end - offset, StandardCharsets.UTF_8);
    }

    private static long readOctal(byte[] block, int offset, int length) {
        String value = readString(block, offset, length).trim();
        if (value.isEmpty()) return 0;
        try {
            return Long.parseLong(value, 8);
        } catch (NumberFormatException e) {
            throw new GradleException("tar 头里的文件大小不是八进制：" + value);
        }
    }

    private static String trimNul(String value) {
        int end = value.indexOf('\0');
        return end >= 0 ? value.substring(0, end) : value;
    }

    private static void skipExactly(InputStream in, long size) throws IOException {
        long remaining = size + padding(size);
        byte[] buffer = new byte[16 * 1024];
        while (remaining > 0) {
            int read = in.read(buffer, 0, (int) Math.min(buffer.length, remaining));
            if (read == -1) throw new IOException("tar 数据提前结束");
            remaining -= read;
        }
    }

    private static void copyExactly(InputStream in, OutputStream out, long size)
            throws IOException {
        byte[] buffer = new byte[64 * 1024];
        long remaining = size;
        while (remaining > 0) {
            int read = in.read(buffer, 0, (int) Math.min(buffer.length, remaining));
            if (read == -1) throw new IOException("tar 数据提前结束");
            out.write(buffer, 0, read);
            remaining -= read;
        }
        // 条目数据按 512 字节对齐，补位要读掉
        long skip = padding(size);
        while (skip > 0) {
            long skipped = in.skip(skip);
            if (skipped <= 0) {
                if (in.read() == -1) throw new IOException("tar 数据提前结束");
                skipped = 1;
            }
            skip -= skipped;
        }
    }

    private static long padding(long size) {
        return (512 - (size % 512)) % 512;
    }

    private static void copy(InputStream in, OutputStream out) throws IOException {
        byte[] buffer = new byte[64 * 1024];
        int read;
        while ((read = in.read(buffer)) != -1) {
            out.write(buffer, 0, read);
        }
    }
}
