package io.github.leexeo.cage.mobile;

import android.app.Activity;
import android.app.AlertDialog;
import android.app.ProgressDialog;
import android.content.Intent;
import android.content.SharedPreferences;
import android.content.pm.PackageManager;
import android.net.Uri;
import android.os.Bundle;
import android.provider.Settings;
import android.widget.Toast;
import androidx.core.content.FileProvider;
import cagemobile.AndroidUpdate;
import java.io.File;
import java.util.concurrent.ExecutorService;
import java.util.concurrent.Executors;
import java.util.concurrent.Future;
import java.util.concurrent.TimeUnit;

final class AppUpdate {
    private static final int ALLOW_INSTALL = 43, INSTALL = 44;
    private static final long INTERVAL = TimeUnit.HOURS.toMillis(12);
    private final Activity activity;
    private final SharedPreferences preferences;
    private final ExecutorService worker = Executors.newSingleThreadExecutor();
    private final String current;
    private boolean resumed, checking, manual;
    private Runnable deferred;
    private File ready;
    private String readyVersion;
    private ProgressDialog progress;
    private AlertDialog prompt;

    AppUpdate(Activity activity, Bundle saved) {
        this.activity = activity;
        preferences = activity.getSharedPreferences("app-updates", Activity.MODE_PRIVATE);
        try {
            current = activity.getPackageManager().getPackageInfo(activity.getPackageName(), 0).versionName;
        } catch (PackageManager.NameNotFoundException e) {
            throw new IllegalStateException(e);
        }
        if (saved != null && saved.getString("update-apk", "").matches("[a-f0-9]{64}\\.apk")) {
            ready = new File(new File(activity.getCacheDir(), "updates"), saved.getString("update-apk"));
            readyVersion = saved.getString("update-version");
        }
    }

    String currentVersion() { return "development".equals(current) ? "开发版" : current; }

    void saveState(Bundle state) {
        if (ready != null) {
            state.putString("update-apk", ready.getName());
            state.putString("update-version", readyVersion);
        }
    }

    void pause() { resumed = false; }
    void resume() {
        resumed = true;
        if (ready != null && (current.equals(readyVersion) || !ready.isFile() ||
                System.currentTimeMillis() - ready.lastModified() >= INTERVAL)) ready = null;
        if (deferred != null) {
            Runnable action = deferred;
            deferred = null;
            action.run();
        } else if (ready == null) check(false);
    }

    void check(boolean requested) {
        if ("development".equals(current)) {
            if (requested) message("本地开发版请通过构建工具更新。");
            return;
        }
        if (progress != null || (prompt != null && prompt.isShowing())) return;
        if (requested && ready != null && ready.isFile()) { offerInstall(); return; }
        if (requested) {
            manual = true;
            message("正在检查更新…");
        }
        if (checking) return;
        long now = System.currentTimeMillis();
        long elapsed = now - preferences.getLong("checked-at", 0);
        if (!requested && elapsed >= 0 && elapsed < INTERVAL) return;
        preferences.edit().putLong("checked-at", now).apply();
        checking = true;
        worker.execute(() -> {
            try {
                File[] files = new File(activity.getCacheDir(), "updates").listFiles();
                if (files != null) for (File file : files)
                    if (now - file.lastModified() > TimeUnit.DAYS.toMillis(1)) file.delete();
                AndroidUpdate update = UpdateDownload.check(current);
                deliver(() -> {
                    checking = false;
                    boolean report = manual;
                    manual = false;
                    if (update == null) {
                        if (report) message("已是当前通道的最新版本。");
                        return;
                    }
                    prompt = new AlertDialog.Builder(activity)
                            .setTitle("发现新版本 " + update.version())
                            .setMessage("当前版本 " + current + "。下载后由 Android 确认安装。")
                            .setNegativeButton("稍后", null)
                            .setPositiveButton("下载更新", (d, w) -> download(update)).show();
                });
            } catch (Exception e) {
                deliver(() -> {
                    checking = false;
                    if (manual) failure("检查更新失败", e);
                    manual = false;
                });
            }
        });
    }

    @SuppressWarnings("deprecation")
    private void download(AndroidUpdate update) {
        ProgressDialog dialog = new ProgressDialog(activity);
        progress = dialog;
        progress.setProgressStyle(ProgressDialog.STYLE_HORIZONTAL);
        progress.setTitle("正在下载 " + update.version());
        progress.setMax(100);
        progress.setCanceledOnTouchOutside(false);
        Future<?> task = worker.submit(() -> {
            try {
                File file = UpdateDownload.fetch(activity, update, value -> activity.runOnUiThread(() -> {
                    if (progress == dialog) dialog.setProgress(value);
                }));
                deliver(() -> {
                    if (progress != dialog) return;
                    finishDownload();
                    ready = file;
                    readyVersion = update.version();
                    install();
                });
            } catch (Exception e) {
                deliver(() -> {
                    if (progress != dialog) return;
                    finishDownload();
                    failure("更新下载失败", e);
                });
            }
        });
        dialog.setOnCancelListener(d -> {
            task.cancel(true);
            progress = null;
        });
        dialog.setButton(ProgressDialog.BUTTON_NEGATIVE, "取消", (d, which) -> dialog.cancel());
        dialog.show();
    }

    private void finishDownload() {
        if (progress != null) { progress.dismiss(); progress = null; }
    }

    private void offerInstall() {
        prompt = new AlertDialog.Builder(activity).setTitle("安装 " + readyVersion)
                .setMessage("更新已下载并校验完成。")
                .setNegativeButton("稍后", null)
                .setPositiveButton("继续安装", (d, w) -> install()).show();
    }

    @SuppressWarnings("deprecation")
    private void install() {
        if (ready == null || !ready.isFile()) {
            ready = null;
            message("更新文件已清理，请重新检查更新。");
            return;
        }
        try {
            if (!activity.getPackageManager().canRequestPackageInstalls()) {
                prompt = new AlertDialog.Builder(activity).setTitle("允许安装应用更新")
                        .setMessage("在系统设置中开启「允许来自此来源的应用」，返回后即可确认安装。")
                        .setNegativeButton("稍后", null)
                        .setPositiveButton("打开设置", (d, w) -> openPermissionSettings()).show();
                return;
            }
            Uri uri = FileProvider.getUriForFile(activity, activity.getPackageName() + ".updates", ready);
            Intent intent = new Intent(Intent.ACTION_INSTALL_PACKAGE, uri)
                    .addFlags(Intent.FLAG_GRANT_READ_URI_PERMISSION).putExtra(Intent.EXTRA_RETURN_RESULT, true);
            activity.startActivityForResult(intent, INSTALL);
        } catch (RuntimeException e) {
            message("无法打开系统安装器，请在官网下载更新。");
        }
    }

    private void openPermissionSettings() {
        try {
            activity.startActivityForResult(new Intent(Settings.ACTION_MANAGE_UNKNOWN_APP_SOURCES,
                    Uri.parse("package:" + activity.getPackageName())), ALLOW_INSTALL);
        } catch (RuntimeException e) {
            message("请在系统设置中允许 Cage 安装未知应用，再到「检查更新」继续。");
        }
    }

    boolean onResult(int code, int result) {
        if (code == ALLOW_INSTALL) {
            if (activity.getPackageManager().canRequestPackageInstalls()) install();
            else message("安装权限未开启，可稍后从「检查更新」继续。");
            return true;
        }
        if (code == INSTALL) {
            if (result == Activity.RESULT_OK) {
                if (ready != null) ready.delete();
                ready = null;
            } else message("更新尚未安装，可稍后从「检查更新」重试。");
            return true;
        }
        return false;
    }

    private void deliver(Runnable action) {
        activity.runOnUiThread(() -> {
            if (activity.isDestroyed() || activity.isFinishing()) return;
            if (resumed) action.run(); else deferred = action;
        });
    }

    private void message(String text) { Toast.makeText(activity, text, Toast.LENGTH_LONG).show(); }

    private void failure(String title, Exception e) {
        String detail = e.getMessage();
        if (detail == null || !detail.matches("[\\p{IsHan}].{0,100}")) detail = "请检查网络连接后重试。";
        prompt = new AlertDialog.Builder(activity).setTitle(title).setMessage(detail)
                .setPositiveButton("知道了", null).show();
    }

    void close() {
        worker.shutdownNow();
        if (prompt != null) prompt.dismiss();
        finishDownload();
        deferred = null;
    }
}
