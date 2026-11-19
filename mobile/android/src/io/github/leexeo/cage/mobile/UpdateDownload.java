package io.github.leexeo.cage.mobile;

import android.content.Context;
import android.content.pm.PackageInfo;
import android.content.pm.PackageManager;
import android.os.Build;
import android.os.SystemClock;

import cagemobile.AndroidUpdate;
import cagemobile.Cagemobile;
import java.io.ByteArrayOutputStream;
import java.io.File;
import java.io.FileOutputStream;
import java.io.InputStream;
import java.io.IOException;
import java.io.OutputStream;
import java.net.HttpURLConnection;
import java.net.URL;
import java.security.DigestOutputStream;
import java.security.MessageDigest;
import java.util.Arrays;
import java.util.HashSet;
import java.util.function.IntConsumer;

final class UpdateDownload {
    static AndroidUpdate check(String version) throws Exception {
        return Cagemobile.selectAndroidUpdate(version, read(Cagemobile.androidReleasesURL(), 4 << 20));
    }

    private static byte[] read(String url, int limit) throws IOException {
        ByteArrayOutputStream output = new ByteArrayOutputStream();
        transfer(url, output, limit, null);
        return output.toByteArray();
    }

    // The catalog supplies the initial GitHub URL. Asset redirects may only go to GitHub's CDN.
    private static long transfer(String source, OutputStream output, long limit, IntConsumer progress) throws IOException {
        URL url = new URL(source);
        long deadline = SystemClock.elapsedRealtime() + (progress == null ? 60_000 : 20 * 60_000);
        for (int redirects = 0; redirects <= 4; redirects++) {
            HttpURLConnection connection = (HttpURLConnection) url.openConnection();
            try {
                connection.setConnectTimeout(15000);
                connection.setReadTimeout(30000);
                connection.setInstanceFollowRedirects(false);
                connection.setRequestProperty("User-Agent", "HTTP-Client");
                int status = connection.getResponseCode();
                if (status == 301 || status == 302 || status == 303 || status == 307 || status == 308) {
                    String location = connection.getHeaderField("Location");
                    if (location == null) throw new IOException("下载地址无效。");
                    url = new URL(url, location);
                    if (!"https".equals(url.getProtocol()) || url.getUserInfo() != null ||
                            (url.getPort() != -1 && url.getPort() != 443) ||
                            !("release-assets.githubusercontent.com".equals(url.getHost()) ||
                              "objects.githubusercontent.com".equals(url.getHost()))) {
                        throw new IOException("更新源返回了不受信任的下载地址。");
                    }
                    continue;
                }
                if (status != 200) throw new IOException("更新源返回 HTTP " + status + "，请稍后重试。");
                if (connection.getContentLengthLong() > limit) throw new IOException("更新文件超过预期大小。");
                try (InputStream input = connection.getInputStream()) {
                    byte[] buffer = new byte[32768];
                    long count = 0;
                    int lastPercent = -1, n;
                    while ((n = input.read(buffer)) != -1) {
                        count += n;
                        if (count > limit) throw new IOException("更新文件超过预期大小。");
                        if (Thread.currentThread().isInterrupted() || SystemClock.elapsedRealtime() >= deadline)
                            throw new IOException("下载已取消或超时，请重试。");
                        output.write(buffer, 0, n);
                        int percent = (int)(count * 100 / limit);
                        if (progress != null && percent != lastPercent) progress.accept(percent);
                        lastPercent = percent;
                    }
                    return count;
                }
            } finally {
                connection.disconnect();
            }
        }
        throw new IOException("下载重定向次数过多。");
    }

    static File fetch(Context context, AndroidUpdate update, IntConsumer progress) throws Exception {
        String manifest = update.checksumsURL();
        String expected = update.expectedDigest(manifest.isEmpty() ? null : read(manifest, 65536));
        File dir = new File(context.getCacheDir(), "updates");
        if (!dir.isDirectory() && !dir.mkdirs()) throw new IOException("无法创建更新目录。");
        File part = File.createTempFile("download-", ".apk", dir);
        try {
            MessageDigest hash = MessageDigest.getInstance("SHA-256");
            try (DigestOutputStream output = new DigestOutputStream(new FileOutputStream(part), hash)) {
                if (transfer(update.downloadURL(), output, update.size(), progress) != update.size())
                    throw new IOException("下载不完整，请重试。");
            }
            StringBuilder actual = new StringBuilder();
            for (byte b : hash.digest()) actual.append(String.format(java.util.Locale.ROOT, "%02x", b));
            if (!expected.contentEquals(actual)) throw new IOException("安装包 SHA-256 校验失败。");
            verifyPackage(context, part, update.version());
            File apk = new File(dir, expected + ".apk");
            if (!part.renameTo(apk)) throw new IOException("无法保存已校验的安装包。");
            return apk;
        } finally {
            part.delete();
        }
    }

    @SuppressWarnings("deprecation")
    static void verifyPackage(Context context, File apk, String version) throws Exception {
        PackageManager pm = context.getPackageManager();
        PackageInfo installed = pm.getPackageInfo(context.getPackageName(), PackageManager.GET_SIGNATURES);
        PackageInfo next = pm.getPackageArchiveInfo(apk.getAbsolutePath(), PackageManager.GET_SIGNATURES);
        if (next == null || !installed.packageName.equals(next.packageName) || !version.equals(next.versionName) ||
                code(next) <= code(installed)) throw new IOException("安装包的应用或版本不匹配。");
        if (installed.signatures == null || installed.signatures.length == 0 || next.signatures == null ||
                !new HashSet<>(Arrays.asList(installed.signatures)).equals(new HashSet<>(Arrays.asList(next.signatures))))
            throw new IOException("安装包签名与当前应用不一致。");
    }

    @SuppressWarnings("deprecation")
    private static long code(PackageInfo info) {
        return Build.VERSION.SDK_INT >= 28 ? info.getLongVersionCode() : info.versionCode;
    }
}
